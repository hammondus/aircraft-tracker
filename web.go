package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	defaultSessionTTL  = 7 * 24 * time.Hour
	defaultTilesPath   = "tiles/australia.pmtiles"
	defaultHistoryPath = "history.db"
	// immutableCache is for URLs that name their own content. See
	// DESIGN-DECISIONS.md §11.
	immutableCache = "public, max-age=31536000, immutable"
	// concurrentLogins bounds how many password verifications run at once.
	// PBKDF2 at 600k iterations is deliberately expensive, which makes an
	// unbounded login endpoint a CPU exhaustion vector; it also throttles
	// brute force to a few attempts a second.
	concurrentLogins = 2
)

//go:embed web
var webFS embed.FS

// asset is one embedded static file, served from a URL containing a hash of its
// own content so it can be cached forever.
//
// Hashing once at startup is correct here precisely because these are embedded:
// the bytes are fixed at build time and cannot change under a running process.
// The tiles archive is the opposite case and is handled separately.
type asset struct {
	url   string
	body  []byte
	ctype string
}

type assets struct {
	byURL  map[string]*asset // hashed URL -> content
	byName map[string]string // "app.js" -> hashed URL
}

func loadAssets() (*assets, error) {
	a := &assets{byURL: map[string]*asset{}, byName: map[string]string{}}
	entries, err := fs.ReadDir(webFS, "web/static")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := webFS.ReadFile("web/static/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		ext := path.Ext(e.Name())
		name := strings.TrimSuffix(e.Name(), ext)
		u := fmt.Sprintf("/static/%s.%s%s", name, hex.EncodeToString(sum[:])[:12], ext)

		ctype := mime.TypeByExtension(ext)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		a.byURL[u] = &asset{url: u, body: body, ctype: ctype}
		a.byName[e.Name()] = u
	}
	return a, nil
}

// vendorHandler serves third-party client libraries from version-pinned paths.
//
// Not content-hashed, unlike our own assets: MapLibre's ESM build imports
// "./maplibre-gl-shared.mjs" relatively, and a hashed filename would break that
// import. The version in the directory name is the cache key instead --
// upgrading means a new path, so the URL still changes with the bytes.
//
// Vendored rather than loaded from a CDN because an internal ops display must
// keep working when a third party is down, and should not tell anyone else who
// is looking at it.
func vendorHandler() (http.Handler, error) {
	sub, err := fs.Sub(webFS, "web/vendor")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/vendor/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", immutableCache)
		fileServer.ServeHTTP(w, r)
	})), nil
}

func (a *assets) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := a.byURL[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", got.ctype)
		w.Header().Set("Cache-Control", immutableCache)
		http.ServeContent(w, r, got.url, time.Time{}, strings.NewReader(string(got.body)))
	}
}

// tiles serves the Protomaps archive. MapLibre reads it with HTTP range
// requests, which http.ServeContent implements for us.
type tiles struct {
	path string

	mu    sync.Mutex
	token string
	size  int64
	mod   time.Time
}

// url returns a cache-busting URL for the archive.
//
// The standing rule is to hash a runtime-read asset per request, so that
// editing it without a restart cannot serve stale bytes. The archive is a
// gigabyte, so hashing it per request is not viable. Stat is the proportionate
// compromise: size and mtime change on any deliberate rebuild, which is the
// only way this file ever changes.
func (t *tiles) url() string {
	fi, err := os.Stat(t.path)
	if err != nil {
		return "/tiles/missing/" + path.Base(t.path)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == "" || fi.Size() != t.size || !fi.ModTime().Equal(t.mod) {
		t.size, t.mod = fi.Size(), fi.ModTime()
		t.token = fmt.Sprintf("%x-%x", fi.Size(), fi.ModTime().UnixNano())
	}
	return "/tiles/" + t.token + "/" + path.Base(t.path)
}

func (t *tiles) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(t.path)
		if err != nil {
			log.Printf("tiles: %v", err)
			http.Error(w, "map tiles unavailable; run `make tiles`", http.StatusNotFound)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, "map tiles unavailable", http.StatusInternalServerError)
			return
		}
		// The URL carries a token derived from size and mtime, so the bytes
		// behind a given URL never change.
		w.Header().Set("Cache-Control", immutableCache)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, path.Base(t.path), fi.ModTime(), f)
	}
}

type server struct {
	vendor  http.Handler
	store   *Store
	cfg     *Config
	hub     *Hub
	poller  *Poller
	sess    *sessions
	assets  *assets
	tiles   *tiles
	tmpl    *template.Template
	logging chan struct{} // semaphore bounding concurrent password verification
}

func newServer(cfg *Config, hub *Hub, p *Poller, store *Store) (*server, error) {
	a, err := loadAssets()
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}
	tmpl, err := template.ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	vendor, err := vendorHandler()
	if err != nil {
		return nil, fmt.Errorf("vendor assets: %w", err)
	}
	return &server{
		vendor:  vendor,
		store:   store,
		cfg:     cfg,
		hub:     hub,
		poller:  p,
		sess:    newSessions(cfg.SessionTTL.Duration),
		assets:  a,
		tiles:   &tiles{path: cfg.TilesPath},
		tmpl:    tmpl,
		logging: make(chan struct{}, concurrentLogins),
	}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.assets.handler())
	mux.Handle("GET /vendor/", s.vendor)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleIndex)))
	mux.Handle("GET /events", s.requireAuth(s.hub))
	mux.Handle("GET /tiles/{token}/{name}", s.requireAuth(s.tiles.handler()))
	mux.Handle("GET /api/flights", s.requireAuth(http.HandlerFunc(s.handleFlights)))
	mux.Handle("GET /api/track", s.requireAuth(http.HandlerFunc(s.handleTrack)))
	mux.Handle("GET /api/watch", s.requireAuth(http.HandlerFunc(s.handleWatchList)))
	mux.Handle("POST /api/watch", s.requireAuth(http.HandlerFunc(s.handleWatchAdd)))
	mux.Handle("DELETE /api/watch", s.requireAuth(http.HandlerFunc(s.handleWatchRemove)))

	return securityHeaders(cacheDefaults(mux))
}

// cacheDefaults sets the conservative policy at the single chokepoint every
// response passes through, rather than per handler. Handlers needing something
// stronger set their own Cache-Control, which replaces this -- so the strict
// cases are opt-in and visible at the call site.
//
// no-cache means "store, but always revalidate", not "do not store". A
// server-rendered page carries the URLs of every asset it references, so
// serving it stale would defeat asset cache-busting entirely. private because
// every page here is behind a session.
func cacheDefaults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, private")
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *server) authenticated(r *http.Request) bool {
	if s.cfg.PasswordHash == "" {
		return true // explicitly unauthenticated; main refuses this without -insecure
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.sess.valid(c.Value, time.Now())
}

func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Only redirect things a human is navigating. Sending the login page to
		// an EventSource or a tile request produces a confusing HTML body where
		// the caller expected data; a status code is actionable.
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorised", http.StatusUnauthorized)
	})
}

// safeNext prevents the ?next= parameter from becoming an open redirect: only
// a local absolute path is ever honoured.
func safeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}

func (s *server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.authenticated(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", map[string]any{
		"Next":  safeNext(r.URL.Query().Get("next")),
		"CSS":   s.assets.byName["app.css"],
		"Error": r.URL.Query().Get("error") != "",
	})
}

func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostFormValue("next"))

	// Bound concurrent verification: PBKDF2 is deliberately expensive, which
	// makes an unbounded login endpoint a way to exhaust the CPU.
	s.logging <- struct{}{}
	ok := VerifyPassword(s.cfg.PasswordHash, r.PostFormValue("password"))
	<-s.logging

	if !ok {
		log.Printf("login: failed attempt from %s", r.RemoteAddr)
		http.Redirect(w, r, "/login?error=1&next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	token, err := s.sess.create(time.Now())
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, token, s.cfg.SessionTTL.Duration)
	log.Printf("login: %s signed in (%d sessions)", r.RemoteAddr, s.sess.count())
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sess.destroy(c.Value)
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.render(w, "index.html", map[string]any{
		"Fleet":  s.cfg.Fleet,
		"CSS":    s.assets.byName["app.css"],
		"JS":     s.assets.byName["app.js"],
		"Layers": s.assets.byName["basemap-layers.json"],
		"Tiles":  s.tiles.url(),
		// Trailing "dark" is a prefix, not a file: MapLibre appends .json,
		// .png and the @2x variants itself.
		"Sprite": "/vendor/basemaps-assets@v4/sprites/dark",
		"Glyphs": "/vendor/basemaps-assets@v4/fonts/{fontstack}/{range}.pbf",
		"Bounds": australiaBounds,
	})
}

// render writes a template, reporting failures loudly rather than serving a
// half-written page. Buffering first would be tidier; at this size the
// simplicity is worth more than the edge case.
func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// australiaBounds matches the bbox the tile archive was clipped to by
// `make tiles`. Panning outside it shows empty space, so the map is fenced to
// where there is data.
var australiaBounds = [4]float64{112.9, -43.7, 153.7, -10.6}

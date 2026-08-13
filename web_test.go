package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPassword = "test-password"

func testServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	hash, err := HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	// A stand-in for the 1 GB archive; the handler only cares that it exists.
	tilePath := filepath.Join(t.TempDir(), "australia.pmtiles")
	if err := os.WriteFile(tilePath, []byte("PMTilesFake"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Fleet:        []Member{{Rego: "VH-YSO", Type: "B190"}, {Rego: "VH-TAV", Type: "P68"}},
		PasswordHash: hash,
		TilesPath:    tilePath,
	}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(cfg, NewHub(), NewPoller(cfg), testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

// noRedirect turns the client into something that reports redirects rather than
// following them, which is what the auth tests need to assert.
func noRedirectClient(ts *httptest.Server) *http.Client {
	jar, _ := newJar()
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newJar() (http.CookieJar, error) {
	return &memoryJar{m: map[string][]*http.Cookie{}}, nil
}

// memoryJar is a minimal CookieJar; net/http/cookiejar would work but pulls in
// public-suffix handling that localhost tests do not need.
type memoryJar struct{ m map[string][]*http.Cookie }

func (j *memoryJar) SetCookies(u *url.URL, cs []*http.Cookie) { j.m[u.Host] = cs }
func (j *memoryJar) Cookies(u *url.URL) []*http.Cookie        { return j.m[u.Host] }

func login(t *testing.T, ts *httptest.Server, c *http.Client, password string) *http.Response {
	t.Helper()
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Everything that reveals fleet data must be behind the session.
func TestProtectedRoutesRequireAuth(t *testing.T) {
	_, ts := testServer(t)
	c := noRedirectClient(ts)

	for _, path := range []string{"/", "/events", "/tiles/abc/australia.pmtiles"} {
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s served without authentication", path)
		}
	}
}

// A browser navigating should be sent to the login page; a data request should
// get a status code it can act on, not an HTML page it cannot use.
func TestUnauthenticatedRedirectVersusStatus(t *testing.T) {
	_, ts := testServer(t)
	c := noRedirectClient(ts)

	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("navigation: status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("navigation redirected to %q", loc)
	}

	req, _ = http.NewRequest("GET", ts.URL+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("EventSource: status = %d, want 401", resp.StatusCode)
	}
}

func TestLoginFlow(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)

	resp := login(t, ts, c, "wrong-password")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("bad password: status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=1") {
		t.Errorf("bad password redirected to %q, want an error", loc)
	}
	if got := s.sess.count(); got != 0 {
		t.Errorf("a failed login created %d sessions", got)
	}

	resp = login(t, ts, c, testPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("good password: status = %d", resp.StatusCode)
	}
	if got := s.sess.count(); got != 1 {
		t.Fatalf("session count = %d after login", got)
	}

	page, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("authenticated index: status = %d", page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	for _, want := range []string{"VH-YSO", "VH-TAV", "Fleet Tracker"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index does not mention %q", want)
		}
	}
}

func TestLogoutDestroysSession(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()

	resp, err := c.PostForm(ts.URL+"/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := s.sess.count(); got != 0 {
		t.Errorf("session survived logout: count = %d", got)
	}
}

// ?next= must never become an open redirect.
func TestNextParameterCannotLeaveTheSite(t *testing.T) {
	for raw, want := range map[string]string{
		"":                          "/",
		"/":                         "/",
		"/somewhere":                "/somewhere",
		"//evil.example.com":        "/",
		"https://evil.example.com":  "/",
		"http://evil.example.com/x": "/",
		"javascript:alert(1)":       "/",
	} {
		if got := safeNext(raw); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Cache policy per DESIGN-DECISIONS.md §11. Getting this wrong in production is
// the bug, not the optimisation.
func TestCachePolicy(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()

	for _, tc := range []struct {
		path string
		want string
		why  string
	}{
		{"/", "no-cache, private", "authenticated HTML must revalidate and never be shared"},
		{"/login", "no-cache, private", "login page is HTML"},
		{"/healthz", "no-store", "health checks must never be cached"},
		{s.assets.byName["app.css"], immutableCache, "hashed asset URL names its own bytes"},
		{s.assets.byName["app.js"], immutableCache, "hashed asset URL names its own bytes"},
		{s.tiles.url(), immutableCache, "tile URL carries a size-mtime token"},
	} {
		resp, err := c.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != tc.want {
			t.Errorf("%s: Cache-Control = %q, want %q (%s)", tc.path, got, tc.want, tc.why)
		}
	}
}

// Assets are addressed by a hash of their content, so the URL changes whenever
// the bytes do -- which is what makes caching them forever safe.
func TestAssetURLsAreContentAddressed(t *testing.T) {
	s, ts := testServer(t)

	css := s.assets.byName["app.css"]
	if !strings.HasPrefix(css, "/static/app.") || !strings.HasSuffix(css, ".css") {
		t.Fatalf("unexpected asset URL %q", css)
	}
	if css == "/static/app.css" {
		t.Error("asset URL carries no content hash")
	}

	// Static assets are public: the CSS reveals nothing and gating them would
	// only mean the login page renders unstyled.
	resp, err := http.Get(ts.URL + css)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("asset status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestUnknownAssetIs404(t *testing.T) {
	_, ts := testServer(t)
	resp, err := http.Get(ts.URL + "/static/app.deadbeef.css")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// MapLibre reads the archive with range requests; serving it without range
// support would mean shipping a gigabyte for every tile.
func TestTilesSupportRangeRequests(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()

	req, _ := http.NewRequest("GET", ts.URL+s.tiles.url(), nil)
	req.Header.Set("Range", "bytes=0-6")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "PMTiles" {
		t.Errorf("range body = %q, want the first 7 bytes", body)
	}
}

// The token changes when the file does, so a cached URL can never serve stale
// tile data.
func TestTileURLTracksFileChanges(t *testing.T) {
	s, _ := testServer(t)
	first := s.tiles.url()
	if first != s.tiles.url() {
		t.Fatal("token is unstable for an unchanged file")
	}

	if err := os.WriteFile(s.tiles.path, []byte("PMTilesFakeLonger"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.tiles.path, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if second := s.tiles.url(); second == first {
		t.Error("token did not change after the archive was rebuilt")
	}
}

// A missing archive must be a clear message, not a panic or a blank map.
func TestMissingTilesIsHandled(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()
	os.Remove(s.tiles.path)

	resp, err := c.Get(ts.URL + "/tiles/whatever/australia.pmtiles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "make tiles") {
		t.Errorf("error does not say how to fix it: %q", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, ts := testServer(t)
	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

// With no password configured the server is open; main refuses to start in that
// state without -insecure, but the handler behaviour is asserted here.
func TestNoPasswordMeansOpen(t *testing.T) {
	cfg := &Config{Fleet: []Member{{Rego: "VH-YSO"}}, TilesPath: "nonexistent"}
	if err := cfg.normalise(); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(cfg, NewHub(), NewPoller(cfg), testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with auth disabled", resp.StatusCode)
	}
}

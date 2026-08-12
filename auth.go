package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Password hashing is PBKDF2-HMAC-SHA256 from crypto/pbkdf2, in the standard
// library since Go 1.24. Authentication therefore adds no dependency at all --
// no golang.org/x/crypto, no bcrypt package.
const (
	// OWASP's current guidance for PBKDF2-SHA256. This runs once per login, not
	// per request: the session cookie carries every request after it, so the
	// cost is paid on a human timescale and is invisible.
	pbkdf2Iterations = 600_000
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
	pbkdf2Scheme     = "pbkdf2-sha256"
)

// HashPassword produces a self-describing encoding:
//
//	pbkdf2-sha256$<iterations>$<salt-base64>$<key-base64>
//
// Self-describing so the iteration count can be raised later without
// invalidating existing hashes -- each one carries the cost it was made with.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password is empty")
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Scheme, pbkdf2Iterations, b64(salt), b64(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash. The
// comparison is constant time; the parsing deliberately is not, since the
// encoded hash is not a secret input.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Scheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

const sessionCookie = "session"

// sessions holds live sessions in memory. A restart logs everyone out, which
// is acceptable for an internal tool and avoids needing a session store, a
// schema, and a cleanup job for what is at most a handful of people.
type sessions struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]time.Time // token -> expiry
}

func newSessions(ttl time.Duration) *sessions {
	return &sessions{ttl: ttl, m: make(map[string]time.Time)}
}

// create issues a token. Expired entries are swept here rather than by a
// background goroutine: logins are rare and the map is tiny, so there is
// nothing to gain from a timer that runs forever.
func (s *sessions) create(now time.Time) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	for t, exp := range s.m {
		if now.After(exp) {
			delete(s.m, t)
		}
	}
	s.m[token] = now.Add(s.ttl)
	return token, nil
}

// valid checks a token and, if good, slides its expiry forward. Sliding rather
// than fixed because this is meant to sit open on an ops display for weeks
// without anyone being asked to log in again mid-shift.
func (s *sessions) valid(token string, now time.Time) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.m, token)
		return false
	}
	s.m[token] = now.Add(s.ttl)
	return true
}

func (s *sessions) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, token)
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// secureRequest reports whether the client reached us over TLS. Behind nginx
// proxy manager the connection to us is plain HTTP, so the proxy's
// X-Forwarded-Proto is the only evidence -- without this the Secure flag would
// never be set in the one deployment that needs it.
//
// Trusting a client-settable header is safe only because this always sits
// behind a proxy that overwrites it. Exposed directly, the worst outcome is a
// cookie marked Secure that need not be.
func secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true, // no reason for scripts to read it
		Secure:   secureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

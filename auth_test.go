package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, "correct horse battery stapl") {
		t.Error("wrong password accepted")
	}
	if VerifyPassword(hash, "") {
		t.Error("empty password accepted")
	}
}

// The encoding carries its own parameters, so the iteration count can be raised
// later without invalidating hashes already in config files.
func TestPasswordHashIsSelfDescribing(t *testing.T) {
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		t.Fatalf("got %d fields, want 4: %q", len(parts), hash)
	}
	if parts[0] != pbkdf2Scheme {
		t.Errorf("scheme = %q", parts[0])
	}
	if parts[1] != "600000" {
		t.Errorf("iterations = %q, want 600000", parts[1])
	}
}

// Two hashes of the same password must differ, or the salt is not doing its job.
func TestPasswordHashIsSalted(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("identical hashes for the same password: salt is not random")
	}
	if !VerifyPassword(a, "same") || !VerifyPassword(b, "same") {
		t.Error("salted hashes do not verify")
	}
}

// Malformed hashes must be rejected rather than panicking or, worse, matching.
func TestVerifyRejectsMalformed(t *testing.T) {
	for name, h := range map[string]string{
		"empty":           "",
		"no fields":       "garbage",
		"too few fields":  "pbkdf2-sha256$600000$c2FsdA",
		"unknown scheme":  "bcrypt$10$c2FsdA$aGFzaA",
		"bad iterations":  "pbkdf2-sha256$abc$c2FsdA$aGFzaA",
		"zero iterations": "pbkdf2-sha256$0$c2FsdA$aGFzaA",
		"bad base64 salt": "pbkdf2-sha256$600000$!!!$aGFzaA",
		"bad base64 key":  "pbkdf2-sha256$600000$c2FsdA$!!!",
		"empty salt":      "pbkdf2-sha256$600000$$aGFzaA",
		"empty key":       "pbkdf2-sha256$600000$c2FsdA$",
	} {
		if VerifyPassword(h, "anything") {
			t.Errorf("%s: malformed hash %q accepted", name, h)
		}
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Error("expected an error for an empty password")
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newSessions(time.Hour)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

	token, err := s.create(now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(token, now) {
		t.Error("fresh token rejected")
	}
	if s.valid("not-a-token", now) {
		t.Error("unknown token accepted")
	}
	if s.valid("", now) {
		t.Error("empty token accepted")
	}

	s.destroy(token)
	if s.valid(token, now) {
		t.Error("destroyed token still valid")
	}
	if got := s.count(); got != 0 {
		t.Errorf("count = %d after destroy", got)
	}
}

// Expiry slides on use, so a display left open does not log itself out
// mid-shift, but an abandoned session still lapses.
func TestSessionExpirySlides(t *testing.T) {
	s := newSessions(time.Hour)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	token, err := s.create(now)
	if err != nil {
		t.Fatal(err)
	}

	// Used every 50 minutes: still valid well past the original hour.
	at := now
	for range 10 {
		at = at.Add(50 * time.Minute)
		if !s.valid(token, at) {
			t.Fatalf("session lapsed at %v despite regular use", at.Sub(now))
		}
	}

	// Then left alone for longer than the TTL.
	if s.valid(token, at.Add(2*time.Hour)) {
		t.Error("abandoned session did not expire")
	}
}

// Expired sessions are swept on the next login rather than by a timer.
func TestExpiredSessionsSweptOnCreate(t *testing.T) {
	s := newSessions(time.Hour)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	for range 5 {
		if _, err := s.create(now); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.count(); got != 5 {
		t.Fatalf("count = %d, want 5", got)
	}
	if _, err := s.create(now.Add(3 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := s.count(); got != 1 {
		t.Errorf("count = %d after sweep, want only the new session", got)
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	s := newSessions(time.Hour)
	now := time.Now()
	seen := map[string]bool{}
	for range 200 {
		tok, err := s.create(now)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatal("duplicate session token")
		}
		seen[tok] = true
	}
}

// Behind nginx the connection to us is plain HTTP, so X-Forwarded-Proto is the
// only evidence the client used TLS. Without it the Secure flag would never be
// set in the one deployment that needs it.
func TestSecureRequestDetection(t *testing.T) {
	plain := httptest.NewRequest("GET", "/", nil)
	if secureRequest(plain) {
		t.Error("plain HTTP treated as secure")
	}

	proxied := httptest.NewRequest("GET", "/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if !secureRequest(proxied) {
		t.Error("X-Forwarded-Proto: https not honoured")
	}

	mixedCase := httptest.NewRequest("GET", "/", nil)
	mixedCase.Header.Set("X-Forwarded-Proto", "HTTPS")
	if !secureRequest(mixedCase) {
		t.Error("header comparison should be case-insensitive")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	setSessionCookie(w, r, "tok", time.Hour)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	c := cookies[0]
	if c.Name != sessionCookie || c.Value != "tok" {
		t.Errorf("cookie = %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly; scripts have no reason to read it")
	}
	if !c.Secure {
		t.Error("cookie is not Secure behind an https proxy")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("path = %q", c.Path)
	}
}

func TestClearSessionCookieExpires(t *testing.T) {
	w := httptest.NewRecorder()
	clearSessionCookie(w, httptest.NewRequest("POST", "/logout", nil))
	c := w.Result().Cookies()[0]
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative to delete", c.MaxAge)
	}
	if c.Value != "" {
		t.Errorf("value = %q, want empty", c.Value)
	}
}

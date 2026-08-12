package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		value string
		want  time.Duration
	}{
		"delta seconds": {"30", 30 * time.Second},
		"zero":          {"0", 0},
		"http date":     {now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		"past date":     {now.Add(-time.Minute).Format(http.TimeFormat), 0},
		"absent":        {"", 0},
		"unparseable":   {"soon", 0},
		"negative":      {"-5", 0},
		"padded":        {"  15  ", 15 * time.Second},
	} {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		if got := retryAfter(h, now); got != tc.want {
			t.Errorf("%s: retryAfter(%q) = %v, want %v", name, tc.value, got, tc.want)
		}
	}
}

func TestRetryDelayBacksOffExponentially(t *testing.T) {
	s := &source{interval: 2 * time.Second}
	err := &httpError{Provider: "test", StatusCode: 429}

	var got []time.Duration
	for range 10 {
		got = append(got, s.retryDelay(err))
	}

	// interval + cooldown, cooldown doubling from interval, capped.
	want := []time.Duration{4, 6, 10, 18, 34, 66, 120, 120, 120, 120}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Errorf("failure %d: delay = %v, want %v", i+1, got[i], w*time.Second)
		}
	}
	for _, d := range got {
		if d > maxBackoff {
			t.Errorf("delay %v exceeds cap %v", d, maxBackoff)
		}
	}
}

// The cooldown must outlive recovery, or a rate-limited provider oscillates:
// back off, recover, snap back to base, trip again. That is the failure this
// mechanism exists to prevent, so assert the whole cycle.
func TestCooldownPersistsAcrossRecovery(t *testing.T) {
	s := &source{interval: 5 * time.Second}
	err := &httpError{StatusCode: 429}

	s.retryDelay(err)
	s.retryDelay(err) // cooldown now 10s
	if got, want := s.base(), 15*time.Second; got != want {
		t.Fatalf("after failures: wait = %v, want %v", got, want)
	}

	// A single good response is not evidence the limit lifted.
	if failed := s.noteSuccess(); failed != 2 {
		t.Errorf("noteSuccess reported %d prior failures, want 2", failed)
	}
	if got, want := s.base(), 15*time.Second; got != want {
		t.Errorf("after 1 success: wait = %v, want %v (unchanged)", got, want)
	}

	// Only sustained success eases it, and then only halfway.
	s.noteSuccess()
	s.noteSuccess()
	if got, want := s.base(), 10*time.Second; got != want {
		t.Errorf("after %d successes: wait = %v, want %v", cooldownDecayAfter, got, want)
	}

	// Eventually it returns all the way to the configured interval.
	for range 3 * cooldownDecayAfter {
		s.noteSuccess()
	}
	if got := s.base(); got != s.interval {
		t.Errorf("after sustained success: wait = %v, want %v", got, s.interval)
	}
	if s.cooldown != 0 {
		t.Errorf("cooldown = %v, want 0", s.cooldown)
	}
}

// A healthy provider must never accumulate a penalty.
func TestNoCooldownWithoutFailure(t *testing.T) {
	s := &source{interval: 5 * time.Second}
	for range 20 {
		if failed := s.noteSuccess(); failed != 0 {
			t.Fatalf("noteSuccess reported %d failures on a healthy source", failed)
		}
	}
	if s.cooldown != 0 || s.base() != s.interval {
		t.Errorf("healthy source drifted: cooldown=%v wait=%v", s.cooldown, s.base())
	}
}

// A server that tells us when to come back is obeyed, but never to poll it
// faster than we would have anyway.
func TestRetryDelayHonoursRetryAfter(t *testing.T) {
	// Longer than our own backoff: obeyed.
	s := &source{interval: 2 * time.Second}
	if got := s.retryDelay(&httpError{StatusCode: 429, RetryAfter: 45 * time.Second}); got != 45*time.Second {
		t.Errorf("long Retry-After: got %v, want 45s", got)
	}

	// Shorter than our own backoff: ignored. A provider that just refused us is
	// not one to push harder than we already decided to.
	s = &source{interval: 2 * time.Second, cooldown: 30 * time.Second}
	if got := s.retryDelay(&httpError{StatusCode: 429, RetryAfter: time.Second}); got != 62*time.Second {
		t.Errorf("short Retry-After: got %v, want 62s", got)
	}

	// Never faster than the configured interval, even on the first failure of
	// a plain error carrying no Retry-After at all.
	s = &source{interval: 5 * time.Second}
	if got := s.retryDelay(errors.New("connection reset")); got < s.interval {
		t.Errorf("got %v, faster than interval %v", got, s.interval)
	}

	// Retry-After must never exceed the cap.
	s = &source{interval: 2 * time.Second}
	if got := s.retryDelay(&httpError{StatusCode: 429, RetryAfter: time.Hour}); got != maxBackoff {
		t.Errorf("huge Retry-After: got %v, want the %v cap", got, maxBackoff)
	}
}

func TestHTTPErrorMessage(t *testing.T) {
	if got := (&httpError{Provider: "adsb.lol", StatusCode: 429}).Error(); got != "adsb.lol: http 429" {
		t.Errorf("got %q", got)
	}
	e := &httpError{Provider: "adsb.lol", StatusCode: 429, RetryAfter: 30 * time.Second}
	if got := e.Error(); got != "adsb.lol: http 429 (retry after 30s)" {
		t.Errorf("got %q", got)
	}
}

// Sources must be spread across the interval rather than firing together;
// that is what makes two 2s providers behave like a 1s refresh.
func TestRunStaggersSources(t *testing.T) {
	c := &Config{
		Fleet:             []Member{{Rego: "VH-YSO"}},
		BroadcastInterval: duration{time.Second},
		PollInterval:      duration{2 * time.Second},
		Providers: []Provider{
			{Name: "a", URL: "http://a/"},
			{Name: "b", URL: "http://b/"},
		},
	}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	if len(p.sources) != 2 {
		t.Fatalf("got %d sources", len(p.sources))
	}
	for _, s := range p.sources {
		if s.interval != 2*time.Second {
			t.Errorf("%s: interval = %v, want the config default", s.Name, s.interval)
		}
	}
}

// A provider may override the default interval; others keep it.
func TestPerProviderInterval(t *testing.T) {
	c := &Config{
		Fleet:        []Member{{Rego: "VH-YSO"}},
		PollInterval: duration{2 * time.Second},
		Providers: []Provider{
			{Name: "slow", URL: "http://slow/", Interval: duration{10 * time.Second}},
			{Name: "default", URL: "http://default/"},
		},
	}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	if p.sources[0].interval != 10*time.Second {
		t.Errorf("explicit interval ignored: %v", p.sources[0].interval)
	}
	if p.sources[1].interval != 2*time.Second {
		t.Errorf("default interval not applied: %v", p.sources[1].interval)
	}
}

// The default must stay well clear of the documented limits: airplanes.live
// permits 1 req/s and adsb.lol's is dynamic. This is a regression guard against
// someone "optimising" the interval toward the ceiling -- there is no benefit to
// collect, and doing so once cost this project its API access.
func TestDefaultProvidersStayWellUnderDocumentedLimits(t *testing.T) {
	c := &Config{Fleet: []Member{{Rego: "VH-YSO"}}}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	if len(p.sources) != 2 {
		t.Fatalf("got %d default providers, want 2", len(p.sources))
	}
	for _, s := range p.sources {
		if s.interval != defaultPollInterval {
			t.Errorf("%s: interval = %v, want %v", s.Name, s.interval, defaultPollInterval)
		}
		// airplanes.live documents 1 req/s; stay several times clear of it.
		if s.interval < 3*time.Second {
			t.Errorf("%s: interval %v is too close to the documented limit", s.Name, s.interval)
		}
	}
}

// Staggering is what keeps a conservative per-provider interval from becoming
// conservative staleness: two sources at 5s should be half an interval apart.
func TestStaggerHalvesEffectiveRefresh(t *testing.T) {
	c := &Config{Fleet: []Member{{Rego: "VH-YSO"}}}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)

	// Two 5s sources: the first starts immediately, the second half an interval
	// in, so fleet state refreshes every 2.5s.
	if got := p.startOffset(0); got != 0 {
		t.Errorf("first source offset = %v, want 0", got)
	}
	if got, want := p.startOffset(1), defaultPollInterval/2; got != want {
		t.Errorf("second source offset = %v, want %v", got, want)
	}

	// A lone provider has nothing to interleave with and must not be delayed.
	solo := &Config{
		Fleet:     []Member{{Rego: "VH-YSO"}},
		Providers: []Provider{{Name: "only", URL: "http://only/"}},
	}
	if err := solo.normalise(); err != nil {
		t.Fatal(err)
	}
	if got := NewPoller(solo).startOffset(0); got != 0 {
		t.Errorf("solo provider offset = %v, want 0", got)
	}
}

// Three providers should spread across the interval, not bunch up.
func TestStaggerThreeSources(t *testing.T) {
	c := &Config{
		Fleet:        []Member{{Rego: "VH-YSO"}},
		PollInterval: duration{6 * time.Second},
		Providers: []Provider{
			{Name: "a", URL: "http://a/"},
			{Name: "b", URL: "http://b/"},
			{Name: "c", URL: "http://c/"},
		},
	}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	want := []time.Duration{0, 2 * time.Second, 4 * time.Second}
	for i, w := range want {
		if got := p.startOffset(i); got != w {
			t.Errorf("source %d offset = %v, want %v", i, got, w)
		}
	}
}

// A 403 is an access refusal, not a rate limit: it will not clear by waiting,
// so it must jump straight to the long backoff rather than creeping up from
// the base interval and hammering a provider that has already said no.
func TestBlockedGoesStraightToLongBackoff(t *testing.T) {
	s := &source{interval: 5 * time.Second}
	blocked := &httpError{Provider: "airplanes.live", StatusCode: 403,
		Body: `{"error": "please contact us at contact@airplanes.live"}`}

	if got := s.retryDelay(blocked); got != blockedBackoff {
		t.Errorf("first 403: delay = %v, want %v", got, blockedBackoff)
	}
	// Repeated blocks must not compound into something absurd.
	for range 5 {
		if got := s.retryDelay(blocked); got != blockedBackoff {
			t.Errorf("repeated 403: delay = %v, want %v", got, blockedBackoff)
		}
	}
	if got := (&httpError{StatusCode: 401}).Blocked(); !got {
		t.Error("401 should count as blocked")
	}
	for _, code := range []int{429, 500, 502, 200} {
		if (&httpError{StatusCode: code}).Blocked() {
			t.Errorf("%d should not count as blocked", code)
		}
	}
}

// The body explains the refusal; "http 403" alone does not distinguish a block
// from a rate limit, which is exactly the confusion that cost us access.
func TestHTTPErrorIncludesBody(t *testing.T) {
	e := &httpError{Provider: "airplanes.live", StatusCode: 403,
		Body: `{"error": "please contact us"}`}
	got := e.Error()
	if !strings.Contains(got, "403") || !strings.Contains(got, "please contact us") {
		t.Errorf("error message loses the diagnosis: %q", got)
	}
}

// Failures must be counted once per failure. They were briefly counted twice,
// which silently inflated every "recovered after N failures" log line.
func TestFailuresCountedOnce(t *testing.T) {
	s := &source{interval: time.Second}
	err := &httpError{StatusCode: 429}
	s.retryDelay(err)
	s.retryDelay(err)
	s.retryDelay(err)
	if failed := s.noteSuccess(); failed != 3 {
		t.Errorf("reported %d failures after exactly 3, want 3", failed)
	}
}

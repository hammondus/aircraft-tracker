package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// recordFlight writes a straight climbing leg into the store.
func recordFlight(t *testing.T, s *Store, hex string, start time.Time, n int) {
	t.Helper()
	for i := range n {
		st := state(hex, start.Add(time.Duration(i)*10*time.Second), -33.0-float64(i)*0.02, 151.0, false)
		st.Fix.AltFt = 1000 + i*100
		if _, err := s.Record([]State{st}); err != nil {
			t.Fatal(err)
		}
	}
}

func historyServer(t *testing.T) (*server, *httptest.Server, *http.Client) {
	t.Helper()
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()
	return s, ts, c
}

func getJSON(t *testing.T, c *http.Client, url string, into any) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decoding %s: %v", url, err)
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return resp
}

// History exposes where the aircraft have been, so it must be behind the
// session like everything else.
func TestHistoryRoutesRequireAuth(t *testing.T) {
	_, ts := testServer(t)
	c := noRedirectClient(ts)
	for _, p := range []string{"/api/flights", "/api/track?hex=7c7c16"} {
		resp, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s served without authentication", p)
		}
	}
}

func TestFlightsEndpoint(t *testing.T) {
	s, ts, c := historyServer(t)
	now := time.Now().UTC()
	recordFlight(t, s.store, s.cfg.Fleet[0].Hex, now.Add(-2*time.Hour), 30)
	recordFlight(t, s.store, s.cfg.Fleet[1].Hex, now.Add(-24*time.Hour), 30)

	var got struct {
		Flights []struct {
			Hex        string    `json:"hex"`
			Rego       string    `json:"rego"`
			Started    time.Time `json:"started"`
			DistanceNM float64   `json:"distance_nm"`
			MaxAltFt   int       `json:"max_alt_ft"`
		} `json:"flights"`
		Truncated bool `json:"truncated"`
	}
	if resp := getJSON(t, c, ts.URL+"/api/flights", &got); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got.Flights) != 2 {
		t.Fatalf("got %d flights, want 2", len(got.Flights))
	}
	// Most recent first: what happened last is what you want to see.
	if got.Flights[0].Started.Before(got.Flights[1].Started) {
		t.Error("flights are not newest-first")
	}
	// The registration is joined on so the list is readable without the client
	// having to map hexes back to aircraft.
	if got.Flights[0].Rego == "" {
		t.Error("flights carry no registration")
	}
	if got.Flights[0].DistanceNM <= 0 || got.Flights[0].MaxAltFt <= 0 {
		t.Errorf("implausible summary: %+v", got.Flights[0])
	}
}

func TestFlightsFilteredByAircraft(t *testing.T) {
	s, ts, c := historyServer(t)
	now := time.Now().UTC()
	recordFlight(t, s.store, s.cfg.Fleet[0].Hex, now.Add(-time.Hour), 30)
	recordFlight(t, s.store, s.cfg.Fleet[1].Hex, now.Add(-time.Hour), 30)

	var got struct {
		Flights []struct {
			Hex string `json:"hex"`
		} `json:"flights"`
	}
	getJSON(t, c, ts.URL+"/api/flights?hex="+s.cfg.Fleet[0].Hex, &got)
	if len(got.Flights) != 1 || got.Flights[0].Hex != s.cfg.Fleet[0].Hex {
		t.Errorf("filter returned %+v", got.Flights)
	}
}

// The default window must not silently include everything, or the list becomes
// unusable once there is a year of data.
func TestFlightsDefaultWindowExcludesOldData(t *testing.T) {
	s, ts, c := historyServer(t)
	recordFlight(t, s.store, s.cfg.Fleet[0].Hex, time.Now().UTC().AddDate(0, 0, -400), 30)

	var got struct {
		Flights []any `json:"flights"`
	}
	getJSON(t, c, ts.URL+"/api/flights", &got)
	if len(got.Flights) != 0 {
		t.Errorf("default window returned %d flights from 400 days ago", len(got.Flights))
	}
	// ...but it is reachable when explicitly asked for.
	getJSON(t, c, ts.URL+"/api/flights?from=2000-01-01", &got)
	if len(got.Flights) != 1 {
		t.Errorf("explicit range returned %d flights, want 1", len(got.Flights))
	}
}

func TestTrackEndpoint(t *testing.T) {
	s, ts, c := historyServer(t)
	start := time.Now().UTC().Add(-time.Hour)
	recordFlight(t, s.store, s.cfg.Fleet[0].Hex, start, 30)

	var got struct {
		Hex    string `json:"hex"`
		Points []struct {
			At      time.Time `json:"at"`
			Lat     float64   `json:"lat"`
			Lon     float64   `json:"lon"`
			AltFt   int       `json:"alt_ft"`
			SpeedKt float64   `json:"speed_kt"`
		} `json:"points"`
		Truncated bool `json:"truncated"`
	}
	resp := getJSON(t, c, ts.URL+"/api/track?hex="+s.cfg.Fleet[0].Hex+"&from=2000-01-01", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got.Points) != 30 {
		t.Fatalf("got %d points, want 30", len(got.Points))
	}
	if got.Points[0].Lat == 0 || got.Points[0].AltFt == 0 {
		t.Errorf("first point looks empty: %+v", got.Points[0])
	}
	for i := 1; i < len(got.Points); i++ {
		if !got.Points[i].At.After(got.Points[i-1].At) {
			t.Fatalf("points out of order at %d; a track drawn from these would zigzag", i)
		}
	}
}

// A typo'd or foreign hex should say so rather than return an empty track that
// looks like "nothing was recorded".
func TestUnknownAircraftIs404(t *testing.T) {
	_, ts, c := historyServer(t)
	for _, u := range []string{"/api/track?hex=abcdef", "/api/flights?hex=abcdef"} {
		if resp := getJSON(t, c, ts.URL+u, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", u, resp.StatusCode)
		}
	}
	if resp := getJSON(t, c, ts.URL+"/api/track", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing hex: status = %d, want 404", resp.StatusCode)
	}
}

func TestHistoryRejectsBadRanges(t *testing.T) {
	s, ts, c := historyServer(t)
	hex := s.cfg.Fleet[0].Hex
	for _, u := range []string{
		"/api/flights?from=yesterday",
		"/api/flights?to=not-a-date",
		"/api/flights?from=2026-01-02&to=2026-01-01",
		"/api/track?hex=" + hex + "&from=nonsense",
	} {
		if resp := getJSON(t, c, ts.URL+u, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", u, resp.StatusCode)
		}
	}
}

// A date-only "to" must cover that whole day. Treating it as midnight would
// make a single-day query return nothing, which is a baffling result.
func TestDateOnlyToCoversWholeDay(t *testing.T) {
	s, ts, c := historyServer(t)
	day := time.Date(2026, 3, 15, 14, 0, 0, 0, time.UTC)
	recordFlight(t, s.store, s.cfg.Fleet[0].Hex, day, 30)

	var got struct {
		Flights []any `json:"flights"`
	}
	getJSON(t, c, ts.URL+"/api/flights?from=2026-03-15&to=2026-03-15", &got)
	if len(got.Flights) != 1 {
		t.Errorf("single-day query returned %d flights, want 1", len(got.Flights))
	}
}

// Query results reflect an archive still being written to.
func TestHistoryIsNeverCached(t *testing.T) {
	s, ts, c := historyServer(t)
	for _, u := range []string{"/api/flights", "/api/track?hex=" + s.cfg.Fleet[0].Hex} {
		resp := getJSON(t, c, ts.URL+u, nil)
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", u, got)
		}
	}
}

func TestParseWhen(t *testing.T) {
	fallback := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got, _ := parseWhen("", fallback); !got.Equal(fallback) {
		t.Errorf("empty should use the fallback, got %v", got)
	}
	if got, err := parseWhen("2026-03-15", fallback); err != nil || got.Day() != 15 {
		t.Errorf("date-only: %v %v", got, err)
	}
	if got, err := parseWhen("2026-03-15T06:30:00Z", fallback); err != nil || got.Hour() != 6 {
		t.Errorf("RFC3339: %v %v", got, err)
	}
	if _, err := parseWhen("15/03/2026", fallback); err == nil {
		t.Error("expected an error for an unrecognised format")
	}
}

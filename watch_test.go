package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A registration converts arithmetically and needs no network, which is what
// makes watching one instant.
func TestResolveWatchByRegistration(t *testing.T) {
	s, _ := testServer(t)
	for _, in := range []string{"VH-WAM", "vh-wam", "WAM", " VH-WAM "} {
		m, err := s.resolveWatch(context.Background(), in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if m.Hex != "7c6f6c" {
			t.Errorf("%q resolved to %q, want 7c6f6c", in, m.Hex)
		}
		if m.Rego != "VH-WAM" {
			t.Errorf("%q gave rego %q", in, m.Rego)
		}
	}
}

func TestResolveWatchByHex(t *testing.T) {
	s, _ := testServer(t)
	m, err := s.resolveWatch(context.Background(), "7C6F6C")
	if err != nil {
		t.Fatal(err)
	}
	if m.Hex != "7c6f6c" {
		t.Errorf("hex = %q, want lowercased", m.Hex)
	}
}

// Nonsense must be refused with a usable message rather than silently tracking
// nothing, which would be indistinguishable from an aircraft nobody can hear.
func TestResolveWatchRejectsNonsense(t *testing.T) {
	s, _ := testServer(t)
	for _, in := range []string{"", "   ", "hello there", "VH-1", "VH-ABCD", "12345", "%%%"} {
		if m, err := s.resolveWatch(context.Background(), in); err == nil {
			t.Errorf("%q was accepted as %+v", in, m)
		}
	}
}

// A callsign can only be resolved while the flight is airborne. With no
// provider reachable the attempt must fail, and say why.
func TestResolveCallsignFailsClearly(t *testing.T) {
	s, _ := testServer(t)
	s.cfg.Providers = []Provider{{Name: "stub", URL: "http://127.0.0.1:1/v2/hex/"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := s.resolveWatch(ctx, "QFA1")
	if err == nil {
		t.Fatal("expected an error with no provider reachable")
	}
	if !strings.Contains(err.Error(), "airborne") {
		t.Errorf("error does not explain the limitation: %v", err)
	}
}

func TestWatchLifecycle(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()

	add := func(q string) *http.Response {
		body := strings.NewReader(`{"query":"` + q + `"}`)
		req, _ := http.NewRequest("POST", ts.URL+"/api/watch", body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := add("VH-WAM")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got struct {
		Watching []Member `json:"watching"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Watching) != 1 || got.Watching[0].Hex != "7c6f6c" {
		t.Fatalf("watching = %+v", got.Watching)
	}

	// It must now appear in snapshots, flagged, so the UI can show it apart.
	found := false
	for _, st := range s.poller.Snapshot() {
		if st.Hex == "7c6f6c" {
			found, _ = true, st
			if !st.Watched {
				t.Error("watched aircraft is not flagged in the snapshot")
			}
		}
	}
	if !found {
		t.Error("watched aircraft absent from the snapshot")
	}
	// And be included in what the providers are asked for.
	if !strings.Contains(s.poller.hexes(), "7c6f6c") {
		t.Errorf("watched hex missing from the query: %s", s.poller.hexes())
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/watch?hex=7c6f6c", nil)
	resp2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if len(s.poller.Watched()) != 0 {
		t.Errorf("still watching %+v", s.poller.Watched())
	}
	if strings.Contains(s.poller.hexes(), "7c6f6c") {
		t.Error("removed aircraft still in the provider query")
	}
}

// Watching one of your own aircraft would draw it twice and record one copy.
func TestCannotWatchYourOwnFleet(t *testing.T) {
	s, ts := testServer(t)
	c := noRedirectClient(ts)
	login(t, ts, c, testPassword).Body.Close()

	body := strings.NewReader(`{"query":"` + s.cfg.Fleet[0].Rego + `"}`)
	req, _ := http.NewRequest("POST", ts.URL+"/api/watch", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// Unlike reference aircraft, a watched one drives the fast rate: you added it
// in order to watch it, so hearing about it two minutes late defeats the point.
func TestWatchedAircraftCountAsActivity(t *testing.T) {
	c := &Config{Fleet: []Member{{Rego: "VH-YSO"}}}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	now := utc(6, 0)
	if mode, _ := p.modeAt(now); mode != modeIdle {
		t.Fatalf("expected idle to begin with, got %s", mode)
	}
	p.Watch(Member{Hex: "7c6f6c", Rego: "VH-WAM"})
	p.merge([]Fix{{Hex: "7c6f6c", At: now}})
	if mode, _ := p.modeAt(now); mode != modeActive {
		t.Errorf("a watched aircraft did not raise the poll rate: %s", mode)
	}
}

// Watching is for glancing at an aircraft, not assembling a second fleet.
func TestWatchListIsBounded(t *testing.T) {
	c := &Config{Fleet: []Member{{Rego: "VH-YSO"}}}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	for i := range maxWatched + 5 {
		p.Watch(Member{Hex: string(rune('a'+i%26)) + "00000", Rego: "X"})
	}
	if len(p.Watched()) > maxWatched+5 {
		t.Errorf("watch list grew to %d", len(p.Watched()))
	}
}

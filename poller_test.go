package main

import (
	"testing"
	"time"
)

func testPoller(members ...Member) *Poller {
	return &Poller{members: members, latest: map[string]Fix{}}
}

// The freshest fix wins regardless of which provider supplied it, and
// regardless of the order they arrive in. This is the whole point of polling
// two networks.
func TestMergeKeepsFreshest(t *testing.T) {
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	p := testPoller(Member{Hex: "7c7c16", Rego: "VH-YSO"})

	p.merge([]Fix{{Hex: "7c7c16", Lat: 1, At: base, Source: "adsb.lol"}})
	p.merge([]Fix{{Hex: "7c7c16", Lat: 2, At: base.Add(2 * time.Second), Source: "airplanes.live"}})
	if got := p.latest["7c7c16"]; got.Lat != 2 || got.Source != "airplanes.live" {
		t.Errorf("newer fix should win: %+v", got)
	}

	// A provider returning a fix older than one we already hold must not
	// overwrite it -- providers routinely lag each other by a second or two.
	p.merge([]Fix{{Hex: "7c7c16", Lat: 3, At: base.Add(time.Second), Source: "adsb.lol"}})
	if got := p.latest["7c7c16"]; got.Lat != 2 {
		t.Errorf("older fix should not overwrite: %+v", got)
	}

	// Both providers reporting the same fix time is a no-op, not a flap.
	p.merge([]Fix{{Hex: "7c7c16", Lat: 9, At: base.Add(2 * time.Second), Source: "adsb.lol"}})
	if got := p.latest["7c7c16"]; got.Lat != 2 || got.Source != "airplanes.live" {
		t.Errorf("equal timestamp should not overwrite: %+v", got)
	}
}

func TestMergeIndependentAircraft(t *testing.T) {
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	p := testPoller(
		Member{Hex: "7c7c16", Rego: "VH-YSO"},
		Member{Hex: "7c6045", Rego: "VH-TAV"},
	)
	p.merge([]Fix{
		{Hex: "7c7c16", Lat: 1, At: base},
		{Hex: "7c6045", Lat: 2, At: base.Add(-time.Hour)},
	})
	if len(p.latest) != 2 {
		t.Fatalf("got %d aircraft, want 2", len(p.latest))
	}
	if p.latest["7c6045"].Lat != 2 {
		t.Error("an old fix for a different aircraft was dropped")
	}
}

// Status must decay with wall time between polls, not be frozen at merge.
func TestSnapshotStatusDecay(t *testing.T) {
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	p := testPoller(Member{Hex: "7c7c16", Rego: "VH-YSO"})
	p.merge([]Fix{{Hex: "7c7c16", Lat: -33.5, Lon: 151.2, At: base}})

	for _, tc := range []struct {
		elapsed time.Duration
		want    Status
	}{
		{0, StatusLive},
		{liveFor - time.Second, StatusLive},
		{liveFor, StatusLive}, // boundary is inclusive
		{liveFor + time.Second, StatusStale},
		{staleFor, StatusStale},
		{staleFor + time.Second, StatusNoContact},
		{24 * time.Hour, StatusNoContact},
	} {
		got := p.snapshotAt(base.Add(tc.elapsed))
		if len(got) != 1 {
			t.Fatalf("got %d states, want 1", len(got))
		}
		if got[0].Status != tc.want {
			t.Errorf("after %s: status = %s, want %s", tc.elapsed, got[0].Status, tc.want)
		}
		if got[0].AgeSec != tc.elapsed.Seconds() {
			t.Errorf("after %s: age = %vs", tc.elapsed, got[0].AgeSec)
		}
		// The last known position stays available even once no_contact, so the
		// UI can say "last seen 3h ago near Wagga" rather than just going blank.
		if got[0].Fix == nil {
			t.Errorf("after %s: fix dropped", tc.elapsed)
		}
	}
}

// Every configured aircraft is always reported, including ones never heard.
// An absent aircraft must be visibly absent, not silently missing.
func TestSnapshotIncludesNeverSeen(t *testing.T) {
	p := testPoller(
		Member{Hex: "7c7c16", Rego: "VH-YSO"},
		Member{Hex: "7c6045", Rego: "VH-TAV"},
	)
	got := p.snapshotAt(time.Now())
	if len(got) != 2 {
		t.Fatalf("got %d states, want 2", len(got))
	}
	for _, s := range got {
		if s.Status != StatusNoContact {
			t.Errorf("%s: status = %s, want no_contact", s.Rego, s.Status)
		}
		if s.Fix != nil {
			t.Errorf("%s: unexpected fix %+v", s.Rego, s.Fix)
		}
	}
}

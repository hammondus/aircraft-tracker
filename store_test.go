package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// state builds a live snapshot entry at a position and time.
func state(hex string, at time.Time, lat, lon float64, ground bool) State {
	return State{
		Member: Member{Hex: hex, Rego: "VH-" + hex[:3]},
		Status: StatusLive,
		Fix: &Fix{
			Hex: hex, At: at, Lat: lat, Lon: lon,
			AltFt: 5000, OnGround: ground, SpeedKt: 150, TrackDeg: 90, Source: "test",
		},
	}
}

var base = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

func TestRecordAndTrack(t *testing.T) {
	s := testStore(t)
	for i := range 5 {
		at := base.Add(time.Duration(i) * 10 * time.Second)
		if _, err := s.Record([]State{state("7c7c16", at, -33.0+float64(i)*0.1, 151.0, false)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Track("7c7c16", base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d fixes, want 5", len(got))
	}
	if !got[0].At.Equal(base) || got[0].Lat != -33.0 {
		t.Errorf("first fix wrong: %+v", got[0])
	}
	// Ordered oldest first, so a track can be drawn straight from the rows.
	for i := 1; i < len(got); i++ {
		if !got[i].At.After(got[i-1].At) {
			t.Errorf("fixes out of order at %d", i)
		}
	}
	if got[4].Source != "test" || got[4].AltFt != 5000 {
		t.Errorf("fields lost in round trip: %+v", got[4])
	}
}

// The archive must not fill with thousands of identical rows for an aircraft
// parked with its transponder on. This is the single most important property of
// the write path.
func TestStationaryAircraftIsThinned(t *testing.T) {
	s := testStore(t)
	// Ten minutes of a parked aircraft, a fix every second, never moving.
	for i := range 600 {
		at := base.Add(time.Duration(i) * time.Second)
		if _, err := s.Record([]State{state("7c7c16", at, -33.0, 151.0, true)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Track("7c7c16", base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// One per heartbeat, not 600. Allow the first plus one a minute.
	if len(got) > 12 {
		t.Errorf("stored %d rows for a stationary aircraft, want about 11", len(got))
	}
	if len(got) < 2 {
		t.Errorf("stored %d rows; the heartbeat should still prove it was there", len(got))
	}
}

// A moving aircraft must be recorded finely, not thinned away.
func TestMovingAircraftIsKept(t *testing.T) {
	s := testStore(t)
	// 150 kt is about 77 m/s, so every 5s step clears the 50m threshold.
	for i := range 20 {
		at := base.Add(time.Duration(i) * 5 * time.Second)
		if _, err := s.Record([]State{state("7c7c16", at, -33.0+float64(i)*0.005, 151.0, false)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Track("7c7c16", base.Add(-time.Hour), base.Add(time.Hour))
	if len(got) != 20 {
		t.Errorf("kept %d of 20 moving fixes", len(got))
	}
}

// Providers re-offer the same fix, and a restart replays it. Neither should
// duplicate a row.
func TestDuplicateFixesIgnored(t *testing.T) {
	s := testStore(t)
	st := state("7c7c16", base, -33.0, 151.0, false)
	for range 10 {
		if _, err := s.Record([]State{st}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Track("7c7c16", base.Add(-time.Hour), base.Add(time.Hour))
	if len(got) != 1 {
		t.Errorf("got %d rows for one repeated fix, want 1", len(got))
	}

	// A fresh Store has no memory of what it wrote, so it will offer the fix
	// again; the unique index must absorb that.
	s2, err := OpenStore(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.Record([]State{st})
	s2.Record([]State{st})
	got2, _ := s2.Track("7c7c16", base.Add(-time.Hour), base.Add(time.Hour))
	if len(got2) != 1 {
		t.Errorf("got %d rows after a simulated restart, want 1", len(got2))
	}
}

// An aircraft nobody can hear has no position worth recording.
func TestNoContactIsNotRecorded(t *testing.T) {
	s := testStore(t)
	st := state("7c7c16", base, -33.0, 151.0, false)
	st.Status = StatusNoContact
	if n, err := s.Record([]State{st}); err != nil || n != 0 {
		t.Errorf("recorded %d fixes for a no_contact aircraft (err %v)", n, err)
	}
	st.Fix = nil
	st.Status = StatusLive
	if n, _ := s.Record([]State{st}); n != 0 {
		t.Errorf("recorded %d fixes for a state with no fix", n)
	}
}

func TestWorthStoring(t *testing.T) {
	prev := Fix{At: base, Lat: -33.0, Lon: 151.0}

	if !worthStoring(nil, prev) {
		t.Error("the first fix for an aircraft must always be stored")
	}
	// Same timestamp: a slower provider re-offering what we already have.
	if worthStoring(&prev, Fix{At: base, Lat: -34.0, Lon: 151.0}) {
		t.Error("a fix with no newer timestamp was stored")
	}
	// Older: providers disagree by seconds and can arrive out of order.
	if worthStoring(&prev, Fix{At: base.Add(-time.Second), Lat: -34.0}) {
		t.Error("an older fix was stored")
	}
	// Newer but barely moved.
	if worthStoring(&prev, Fix{At: base.Add(2 * time.Second), Lat: -33.0001, Lon: 151.0}) {
		t.Error("a fix 11m away was stored")
	}
	// Newer and moved far enough.
	if !worthStoring(&prev, Fix{At: base.Add(2 * time.Second), Lat: -33.001, Lon: 151.0}) {
		t.Error("a fix 111m away was not stored")
	}
	// Stationary, but the heartbeat is due.
	if !worthStoring(&prev, Fix{At: base.Add(heartbeat), Lat: -33.0, Lon: 151.0}) {
		t.Error("the heartbeat did not force a row")
	}
}

func TestMetresBetween(t *testing.T) {
	// One degree of latitude is close to 111.2 km anywhere.
	if got := metresBetween(-33, 151, -34, 151); math.Abs(got-111195) > 500 {
		t.Errorf("1 degree of latitude = %.0f m, want about 111195", got)
	}
	if got := metresBetween(-33, 151, -33, 151); got != 0 {
		t.Errorf("distance to itself = %v", got)
	}
	// Across the antimeridian: 2 degrees of longitude at the equator, not 358.
	if got := metresBetween(0, 179, 0, -179); math.Abs(got-222390) > 1000 {
		t.Errorf("across the antimeridian = %.0f m, want about 222390", got)
	}
}

// Flight segmentation is derived on query, so these exercise the heuristic
// directly rather than any stored table.
func TestSegmentFlightsSplitsOnContactGap(t *testing.T) {
	var fixes []Fix
	add := func(start time.Time, n int) {
		for i := range n {
			fixes = append(fixes, Fix{At: start.Add(time.Duration(i) * 10 * time.Second),
				Lat: -33.0 + float64(i)*0.01, Lon: 151.0, AltFt: 8000})
		}
	}
	add(base, 10)
	add(base.Add(time.Hour), 10) // an hour of silence between them

	got := segmentFlights("7c7c16", fixes)
	if len(got) != 2 {
		t.Fatalf("got %d flights, want 2", len(got))
	}
	if !got[0].Started.Equal(base) {
		t.Errorf("first flight starts %v", got[0].Started)
	}
	if got[0].Fixes != 10 || got[1].Fixes != 10 {
		t.Errorf("fix counts: %d, %d", got[0].Fixes, got[1].Fixes)
	}
}

// A parked aircraft emits a heartbeat every minute, so there is no gap to
// detect -- the on-ground rule is what separates two flights either side of a
// turnaround.
func TestSegmentFlightsSplitsOnGroundTime(t *testing.T) {
	var fixes []Fix
	at := base
	push := func(n int, ground bool, step time.Duration) {
		for range n {
			fixes = append(fixes, Fix{At: at, Lat: -33.0, Lon: 151.0, AltFt: 9000, OnGround: ground})
			at = at.Add(step)
		}
	}
	push(10, false, 10*time.Second) // airborne
	push(20, true, time.Minute)     // 20 minutes parked, heartbeat only
	push(10, false, 10*time.Second) // airborne again

	got := segmentFlights("7c7c16", fixes)
	if len(got) != 2 {
		t.Fatalf("got %d flights, want 2 either side of the turnaround", len(got))
	}
}

// An aircraft sitting in a hangar with its transponder on is not a flight.
func TestSegmentFlightsDropsGroundOnly(t *testing.T) {
	var fixes []Fix
	for i := range 30 {
		fixes = append(fixes, Fix{At: base.Add(time.Duration(i) * time.Minute),
			Lat: -33.0, Lon: 151.0, OnGround: true})
	}
	if got := segmentFlights("7c7c16", fixes); len(got) != 0 {
		t.Errorf("got %d flights from an aircraft that never left the ground", len(got))
	}
}

func TestSegmentFlightsDropsSpecks(t *testing.T) {
	fixes := []Fix{
		{At: base, Lat: -33, Lon: 151, AltFt: 3000},
		{At: base.Add(10 * time.Second), Lat: -33.01, Lon: 151, AltFt: 3000},
	}
	if got := segmentFlights("7c7c16", fixes); len(got) != 0 {
		t.Errorf("got %d flights from %d stray fixes", len(got), len(fixes))
	}
	if got := segmentFlights("7c7c16", nil); len(got) != 0 {
		t.Errorf("got %d flights from no fixes", len(got))
	}
}

func TestFlightSummary(t *testing.T) {
	var fixes []Fix
	for i := range 10 {
		fixes = append(fixes, Fix{
			At: base.Add(time.Duration(i) * time.Minute),
			// A degree of latitude per step is about 60 nm.
			Lat: -33.0 - float64(i), Lon: 151.0, AltFt: 1000 * (i + 1),
		})
	}
	got := segmentFlights("7c7c16", fixes)
	if len(got) != 1 {
		t.Fatalf("got %d flights, want 1", len(got))
	}
	f := got[0]
	if f.MaxAltFt != 10000 {
		t.Errorf("max alt = %d, want 10000", f.MaxAltFt)
	}
	if f.Duration() != 9*time.Minute {
		t.Errorf("duration = %v, want 9m", f.Duration())
	}
	if math.Abs(f.DistanceNM-540) > 10 { // 9 degrees of latitude
		t.Errorf("distance = %.0f nm, want about 540", f.DistanceNM)
	}
	if f.Hex != "7c7c16" {
		t.Errorf("hex = %q", f.Hex)
	}
}

// End to end: record a snapshot stream, then ask for flights back.
func TestFlightsFromRecordedData(t *testing.T) {
	s := testStore(t)
	at := base
	for i := range 40 { // ~7 minutes climbing away
		s.Record([]State{state("7c7c16", at, -33.0-float64(i)*0.02, 151.0, false)})
		at = at.Add(10 * time.Second)
	}
	at = at.Add(time.Hour) // gone for an hour
	for i := range 40 {
		s.Record([]State{state("7c7c16", at, -35.0-float64(i)*0.02, 151.0, false)})
		at = at.Add(10 * time.Second)
	}

	got, err := s.Flights("7c7c16", base.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d flights, want 2", len(got))
	}
	for _, f := range got {
		if f.Fixes < minFlightFixes || f.DistanceNM <= 0 {
			t.Errorf("implausible flight: %+v", f)
		}
	}
}

func TestStatsReportsArchiveSize(t *testing.T) {
	s := testStore(t)
	n, oldest, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || !oldest.IsZero() {
		t.Errorf("empty archive reports %d fixes since %v", n, oldest)
	}
	s.Record([]State{state("7c7c16", base, -33, 151, false)})
	s.Record([]State{state("7c6045", base.Add(time.Hour), -34, 151, false)})
	n, oldest, err = s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("got %d fixes, want 2", n)
	}
	if !oldest.Equal(base) {
		t.Errorf("oldest = %v, want %v", oldest, base)
	}
}

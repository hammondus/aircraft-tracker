package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"
)

const (
	// defaultHistoryDays is how far back the flight list looks when no range is
	// given. Long enough to be useful on opening, short enough to stay quick.
	defaultHistoryDays = 30
	// maxFlights bounds the list a browser has to render.
	maxFlights = 500
	// maxTrackPoints bounds one track response. A five-hour flight recorded at
	// one fix per five seconds is about 3,600 points, so this only bites on a
	// deliberately huge range -- but "all of time for one aircraft" is one URL
	// away, and an unbounded response would wedge the browser.
	maxTrackPoints = 20000
)

// parseWhen accepts either a plain date or a full RFC3339 timestamp, because
// the UI sends dates and links may carry precise times.
func parseWhen(s string, fallback time.Time) (time.Time, error) {
	if s == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("time %q: want YYYY-MM-DD or RFC3339", s)
}

// historyRange pulls from/to out of a request, defaulting to the recent past.
func historyRange(r *http.Request) (from, to time.Time, err error) {
	now := time.Now().UTC()
	from, err = parseWhen(r.URL.Query().Get("from"), now.AddDate(0, 0, -defaultHistoryDays))
	if err != nil {
		return
	}
	to, err = parseWhen(r.URL.Query().Get("to"), now)
	if err != nil {
		return
	}
	// Order matters: validate the range as the caller wrote it, then widen. The
	// other way round, extending "to" to the end of its day turns a backwards
	// range into a valid zero-length one and the mistake passes silently.
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("to is before from")
	}
	// A date-only "to" means the end of that day, not its midnight -- otherwise
	// asking for a single day returns nothing, which is a baffling result.
	if len(r.URL.Query().Get("to")) == len("2006-01-02") {
		to = to.AddDate(0, 0, 1)
	}
	return
}

// known reports whether a hex is one of ours. The archive only ever contains
// fleet aircraft, so this is about giving a clear error rather than a silently
// empty result for a typo'd registration.
func (s *server) known(hex string) bool {
	for _, m := range s.cfg.Fleet {
		if m.Hex == hex {
			return true
		}
	}
	return false
}

// handleFlights lists inferred flights, for one aircraft or the whole fleet.
func (s *server) handleFlights(w http.ResponseWriter, r *http.Request) {
	from, to, err := historyRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hexes := make([]string, 0, len(s.cfg.Fleet))
	if hex := r.URL.Query().Get("hex"); hex != "" {
		if !s.known(hex) {
			http.Error(w, "unknown aircraft", http.StatusNotFound)
			return
		}
		hexes = append(hexes, hex)
	} else {
		for _, m := range s.cfg.Fleet {
			hexes = append(hexes, m.Hex)
		}
	}

	type listed struct {
		Flight
		Rego string `json:"rego"`
	}
	regos := map[string]string{}
	for _, m := range s.cfg.Fleet {
		regos[m.Hex] = m.Rego
	}

	out := []listed{}
	for _, hex := range hexes {
		flights, err := s.store.Flights(hex, from, to)
		if err != nil {
			log.Printf("history: flights %s: %v", hex, err)
			http.Error(w, "history unavailable", http.StatusInternalServerError)
			return
		}
		for _, f := range flights {
			out = append(out, listed{Flight: f, Rego: regos[f.Hex]})
		}
	}
	// Most recent first: what you want to see is almost always what happened
	// last.
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	truncated := len(out) > maxFlights
	if truncated {
		out = out[:maxFlights]
	}

	writeJSON(w, map[string]any{
		"flights":   out,
		"truncated": truncated,
		"from":      from,
		"to":        to,
	})
}

// handleTrack returns the recorded positions for one aircraft over a period,
// which is what the map draws.
func (s *server) handleTrack(w http.ResponseWriter, r *http.Request) {
	hex := r.URL.Query().Get("hex")
	if !s.known(hex) {
		http.Error(w, "unknown aircraft", http.StatusNotFound)
		return
	}
	from, to, err := historyRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fixes, err := s.store.Track(hex, from, to)
	if err != nil {
		log.Printf("history: track %s: %v", hex, err)
		http.Error(w, "history unavailable", http.StatusInternalServerError)
		return
	}
	truncated := len(fixes) > maxTrackPoints
	if truncated {
		fixes = fixes[:maxTrackPoints]
	}

	// A trimmed shape rather than the full Fix: the client draws a line and
	// scrubs along it, and a track is the one response here big enough for the
	// unused fields to matter.
	points := make([]map[string]any, 0, len(fixes))
	for _, f := range fixes {
		points = append(points, map[string]any{
			"at": f.At, "lat": f.Lat, "lon": f.Lon, "alt_ft": f.AltFt,
			"on_ground": f.OnGround, "speed_kt": f.SpeedKt, "track_deg": f.TrackDeg,
			"mlat": f.MLAT, "source": f.Source,
		})
	}
	writeJSON(w, map[string]any{"hex": hex, "points": points, "truncated": truncated})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Query results reflect an archive that is still being written to, so they
	// must never be reused.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("history: encode: %v", err)
	}
}

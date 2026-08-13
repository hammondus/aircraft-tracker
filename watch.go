package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// maxWatched bounds the runtime watch list. Every entry lengthens the provider
// query and clutters the map, and the feature is for glancing at one or two
// aircraft rather than assembling a second fleet.
const maxWatched = 20

var (
	hexPattern      = regexp.MustCompile(`^[0-9a-f]{6}$`)
	callsignPattern = regexp.MustCompile(`^[A-Z]{2,3}[0-9]{1,4}[A-Z]?$`)
)

// resolveWatch turns whatever was typed into an aircraft to track.
//
// Three forms, in order of cost:
//
//   - a VH- registration, which converts arithmetically and needs no network
//   - a raw ICAO hex address, used as-is
//   - a callsign or flight number, which has to be looked up because the
//     mapping only exists while the flight is in the air
//
// The callsign case is the reason this returns an error rather than a guess: a
// flight that has landed simply cannot be resolved, and saying so is far more
// use than tracking nothing and calling it "no contact".
func (s *server) resolveWatch(ctx context.Context, query string) (Member, error) {
	q := strings.ToUpper(strings.TrimSpace(query))
	q = strings.ReplaceAll(q, " ", "")
	if q == "" {
		return Member{}, fmt.Errorf("type a registration, hex address or callsign")
	}

	// A registration, with or without the VH- prefix.
	if hex, err := vhToHex(q); err == nil {
		rego := q
		if !strings.HasPrefix(rego, "VH-") {
			rego = "VH-" + strings.TrimPrefix(rego, "VH")
		}
		return Member{Hex: hex, Rego: rego}, nil
	}
	if strings.HasPrefix(q, "VH-") || strings.HasPrefix(q, "VH") {
		return Member{}, fmt.Errorf("%q is not a valid VH- registration", query)
	}

	// A raw ICAO address.
	if hexPattern.MatchString(strings.ToLower(q)) {
		return Member{Hex: strings.ToLower(q), Rego: strings.ToLower(q)}, nil
	}

	if !callsignPattern.MatchString(q) {
		return Member{}, fmt.Errorf("%q is not a registration, hex address or callsign", query)
	}
	return s.resolveCallsign(ctx, q)
}

// resolveCallsign asks the providers which aircraft is currently flying under a
// callsign. Once resolved it is tracked by hex like anything else, so this
// happens once rather than on every poll.
func (s *server) resolveCallsign(ctx context.Context, callsign string) (Member, error) {
	for _, p := range s.cfg.Providers {
		// The callsign endpoint sits alongside the hex one on every provider
		// speaking this schema.
		base := strings.NewReplacer("/hex/", "/callsign/", "/icao/", "/callsign/").Replace(p.URL)
		if base == p.URL {
			continue // not a shape we recognise
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+callsign, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := s.poller.client.Do(req)
		if err != nil {
			continue
		}
		var out struct {
			AC []struct {
				Hex    string `json:"hex"`
				Rego   string `json:"r"`
				Type   string `json:"t"`
				Desc   string `json:"desc"`
				Flight string `json:"flight"`
			} `json:"ac"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || len(out.AC) == 0 {
			continue
		}
		a := out.AC[0]
		m := Member{Hex: strings.ToLower(a.Hex), Rego: a.Rego, Type: a.Type, Desc: a.Desc}
		if m.Rego == "" {
			m.Rego = strings.TrimSpace(a.Flight)
		}
		return m, nil
	}
	return Member{}, fmt.Errorf("no aircraft is currently flying as %s "+
		"(a callsign can only be found while the flight is airborne)", callsign)
}

func (s *server) handleWatchList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"watching": s.poller.Watched()})
}

func (s *server) handleWatchAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m, err := s.resolveWatch(ctx, body.Query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Already configured: tracking it twice would draw two aircraft in one place
	// and record one of them.
	for _, f := range s.cfg.Fleet {
		if f.Hex == m.Hex {
			http.Error(w, f.Rego+" is already in your fleet", http.StatusConflict)
			return
		}
	}
	if len(s.poller.Watched()) >= maxWatched && !s.poller.watching(m.Hex) {
		http.Error(w, "too many aircraft being watched; remove one first", http.StatusConflict)
		return
	}

	s.poller.Watch(m)
	log.Printf("watch: tracking %s (%s) for this session", m.Rego, m.Hex)
	writeJSON(w, map[string]any{"watching": s.poller.Watched(), "added": m})
}

func (s *server) handleWatchRemove(w http.ResponseWriter, r *http.Request) {
	hex := strings.ToLower(r.URL.Query().Get("hex"))
	if !hexPattern.MatchString(hex) {
		http.Error(w, "bad hex", http.StatusBadRequest)
		return
	}
	s.poller.Unwatch(hex)
	writeJSON(w, map[string]any{"watching": s.poller.Watched()})
}

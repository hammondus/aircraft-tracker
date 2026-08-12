package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Provider is one upstream ADS-B aggregator. Both currently supported services
// serve the readsb/tar1090 schema, so one decoder covers them.
//
// Interval is per provider because their rate limits differ and are not
// documented in the responses -- see DESIGN-DECISIONS.md §1, "Rate limits and
// poll scheduling". Zero means use the config's default.
type Provider struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"` // comma-separated hex list is appended
	Interval duration `json:"interval,omitzero"`
}

// httpError carries the status and any Retry-After so the caller can back off
// as instructed rather than guessing.
type httpError struct {
	Provider   string
	StatusCode int
	RetryAfter time.Duration // zero when the header was absent or unparseable
}

func (e *httpError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s: http %d (retry after %s)", e.Provider, e.StatusCode, e.RetryAfter)
	}
	return fmt.Sprintf("%s: http %d", e.Provider, e.StatusCode)
}

// retryAfter parses the header in both permitted forms: delta-seconds, or an
// HTTP date. Returns zero if absent or unparseable, which the caller treats as
// "back off by your own schedule".
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// defaultPollInterval applies to any provider that does not set its own. It is
// set from measurement rather than documentation: neither service publishes a
// limit we could rely on, and neither advertises one in response headers.
//
// Measured, both providers, evening AEST:
//   - 1s      -- adsb.lol returned HTTP 429 within about four seconds.
//   - 2s      -- both providers 429'd intermittently, roughly every six to ten
//     requests, recovering and then tripping again.
//   - 5s      -- ran clean.
//
// The intermittent pattern looks like a token bucket refilling slower than one
// per two seconds, rather than a fixed-rate gate; the fix for that is a longer
// interval, not more backoff.
//
// Treat this number as provisional. A lot of requests were made from one IP
// while characterising this, which may itself have depleted a longer-window
// budget, so the true sustainable rate could be faster than 5s. It is worth
// re-checking in normal operation. The design is deliberately insensitive to
// getting it exactly right: Retry-After is honoured, failures back off, and two
// providers cover each other.
//
// 5s per provider is not 5s of staleness. Sources are staggered, so two
// providers refresh fleet state every ~2.5s, and client-side dead reckoning
// smooths the gap -- an airliner covers about 500m in that time, which
// interpolates cleanly from ground speed and track.
const defaultPollInterval = 5 * time.Second

// DefaultProviders is deliberately more than one. The dominant failure mode of
// this tool is not knowing where an aircraft is because no volunteer receiver
// could hear it; the two networks have different feeder populations, so the
// union of their coverage is strictly better than either alone. At fleet scale
// the redundancy costs one extra request every few seconds, and it halves the
// effective refresh interval as a side effect of staggering.
//
// adsb.fi serves the same schema and can be added here as a third source.
func DefaultProviders() []Provider {
	return []Provider{
		{Name: "adsb.lol", URL: "https://api.adsb.lol/v2/hex/"},
		{Name: "airplanes.live", URL: "https://api.airplanes.live/v2/hex/"},
	}
}

// altBaro decodes barometric altitude, which is a number in flight but the
// string "ground" on the surface. A plain int field fails on the latter.
type altBaro struct {
	Feet     int
	OnGround bool
}

func (a *altBaro) UnmarshalJSON(b []byte) error {
	if string(b) == `"ground"` {
		a.OnGround = true
		return nil
	}
	return json.Unmarshal(b, &a.Feet)
}

// wireAircraft is the provider schema. Only the fields we render are listed;
// the rest of the ~40 are ignored by encoding/json.
//
// Lat/Lon/SeenPos are pointers because an aircraft can legitimately appear in a
// response having been heard but not positionally resolved, and zero is a
// valid coordinate.
type wireAircraft struct {
	Hex      string   `json:"hex"`
	Flight   string   `json:"flight"`
	Rego     string   `json:"r"`
	Type     string   `json:"t"`
	Lat      *float64 `json:"lat"`
	Lon      *float64 `json:"lon"`
	Alt      altBaro  `json:"alt_baro"`
	GS       float64  `json:"gs"`
	Track    float64  `json:"track"`
	BaroRate int      `json:"baro_rate"`
	Squawk   string   `json:"squawk"`
	SeenPos  *float64 `json:"seen_pos"`
	MLAT     []any    `json:"mlat"`
}

type wireResponse struct {
	AC []wireAircraft `json:"ac"`
}

// Fix is one position report, normalised across providers.
type Fix struct {
	Hex      string    `json:"hex"`
	Flight   string    `json:"flight,omitempty"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	AltFt    int       `json:"alt_ft"`
	OnGround bool      `json:"on_ground"`
	SpeedKt  float64   `json:"speed_kt"`
	TrackDeg float64   `json:"track_deg"`
	VertFPM  int       `json:"vert_fpm"`
	Squawk   string    `json:"squawk,omitempty"`
	MLAT     bool      `json:"mlat"`
	At       time.Time `json:"at"`
	Source   string    `json:"source"`
}

// decode converts a provider response into fixes.
//
// recv is the local time the response arrived. seen_pos is the age of the
// position in seconds at that moment, so the absolute fix time is recv-seen_pos.
// We deliberately use our own clock rather than the response's "now" field:
// each provider stamps "now" from its own clock, and comparing two providers'
// timestamps to pick the freshest fix would then be measuring their clock skew
// as much as the data. Network latency biases both sources near-identically,
// which is what makes them comparable.
func decode(body io.Reader, source string, recv time.Time) ([]Fix, error) {
	var resp wireResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}
	fixes := make([]Fix, 0, len(resp.AC))
	for _, a := range resp.AC {
		if a.Lat == nil || a.Lon == nil {
			continue // heard, but no position -- nothing to plot
		}
		age := 0.0
		if a.SeenPos != nil {
			age = *a.SeenPos
		}
		fixes = append(fixes, Fix{
			Hex:      strings.ToLower(a.Hex),
			Flight:   strings.TrimSpace(a.Flight), // padded to 8 chars on the wire
			Lat:      *a.Lat,
			Lon:      *a.Lon,
			AltFt:    a.Alt.Feet,
			OnGround: a.Alt.OnGround,
			SpeedKt:  a.GS,
			TrackDeg: a.Track,
			VertFPM:  a.BaroRate,
			Squawk:   a.Squawk,
			MLAT:     len(a.MLAT) > 0,
			At:       recv.Add(-time.Duration(age * float64(time.Second))),
			Source:   source,
		})
	}
	return fixes, nil
}

func (p Provider) poll(ctx context.Context, c *http.Client, hexes string) ([]Fix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+hexes, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	recv := time.Now()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &httpError{
			Provider:   p.Name,
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfter(resp.Header, recv),
		}
	}
	fixes, err := decode(resp.Body, p.Name, recv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	return fixes, nil
}

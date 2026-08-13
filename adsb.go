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

// httpError carries the status, any Retry-After, and a snippet of the response
// body so the caller can back off as instructed rather than guessing -- and so
// the log says what actually went wrong.
type httpError struct {
	Provider   string
	StatusCode int
	RetryAfter time.Duration // zero when the header was absent or unparseable
	Body       string        // truncated; these APIs explain refusals in the body
}

func (e *httpError) Error() string {
	s := fmt.Sprintf("%s: http %d", e.Provider, e.StatusCode)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// Blocked reports whether this is an access refusal rather than a transient
// limit. The distinction matters: a 429 clears on its own, a 403 does not, and
// retrying one every couple of minutes only compounds whatever caused it.
func (e *httpError) Blocked() bool {
	return e.StatusCode == http.StatusForbidden || e.StatusCode == http.StatusUnauthorized
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

// defaultPollInterval applies to any provider that does not set its own.
//
// Documented limits:
//   - airplanes.live: 1 request per second. https://airplanes.live/api-guide/
//   - adsb.lol:       "dynamic based on the environment load", i.e. no fixed
//     number exists. https://github.com/adsblol/api
//
// 10s is therefore ten times slower than airplanes.live permits, deliberately.
// Two aircraft need nothing faster; adsb.lol's limit moves with their load so
// headroom is worth more than throughput; and these are free services carrying
// a production dependency. Do not "optimise" this toward the documented ceiling
// without a reason -- there is no benefit to collect.
//
// 10s per provider is not 10s of staleness. Sources are staggered, so two
// providers refresh fleet state every ~5s, and client-side dead reckoning
// smooths the gap: an airliner covers about a kilometre in that time, which
// interpolates cleanly from ground speed and track.
//
// Read the provider's documentation before changing this. Measuring their
// limits empirically got this project's IP blocked -- see DESIGN-DECISIONS.md,
// "Incident: airplanes.live blocked the development IP".
const defaultPollInterval = 10 * time.Second

// Idle rates. A two-aircraft fleet is on the ground for most of the day, and
// confirming that six times a minute spends a free service's capacity to learn
// nothing. Polling drops to defaultIdleInterval once nothing has been detected
// for defaultIdleTimeout, and rises back to defaultPollInterval the moment any
// aircraft appears -- at any hour, because the reason to watch it is the same
// at 3am as at noon.
//
// The cost is wake-up latency: a departure is invisible until the next idle
// poll, so up to two minutes by day and five overnight. That is an accepted
// trade for a situational-awareness display, not an oversight.
const (
	defaultIdleInterval = 2 * time.Minute
	defaultIdleTimeout  = 10 * time.Minute
	defaultQuietIdle    = 5 * time.Minute
)

// defaultQuietHours slows the *idle* rate overnight. 12:00-20:00 UTC is
// 22:00-06:00 AEST, chosen in UTC so the window does not shift when daylight
// saving starts.
func defaultQuietHours() QuietHours {
	return QuietHours{
		From:         12 * 60,
		To:           20 * 60,
		IdleInterval: duration{defaultQuietIdle},
	}
}

// DefaultProviders is deliberately more than one. The dominant failure mode of
// this tool is not knowing where an aircraft is because no volunteer receiver
// could hear it; the two networks have different feeder populations, so the
// union of their coverage is strictly better than either alone. At fleet scale
// the redundancy costs one extra request every few seconds, and it halves the
// effective refresh interval as a side effect of staggering.
//
// All three speak the same schema, so adding one costs a line.
//
// adsb.fi is "personal, non-commercial use only" per its documented terms, and
// uses /icao/ rather than /hex/ for a comma-separated list -- that is the form
// its README documents for multiple aircraft.
func DefaultProviders() []Provider {
	return []Provider{
		{Name: "adsb.lol", URL: "https://api.adsb.lol/v2/hex/"},
		{Name: "adsb.fi", URL: "https://opendata.adsb.fi/api/v2/icao/"},
	}
}

// airplanesLive is absent from DefaultProviders because, as of August 2026,
// there is no free API to poll.
//
//	HTTP 403 {"error": "please contact us at contact@airplanes.live"}
//
// This is not an IP block. Airplanes.live withdrew free API access for everyone,
// citing bot and AI-agent abuse against 2 billion requests a week, hosting costs
// up roughly 300% in eighteen months, and a month's egress allowance consumed in
// four days. Access now expects a feeder plus sponsorship (25 or 50 USD/month),
// after which they will clear a static IP or user-agent on their firewall.
//
// Restore it by putting it back in the list above once sponsored -- nothing else
// changes, the merge treats any number of providers alike.
var airplanesLive = Provider{Name: "airplanes.live", URL: "https://api.airplanes.live/v2/hex/"}

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
		// Keep a little of the body: both providers explain refusals there, and
		// "http 403" alone does not distinguish a rate limit from a block.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, &httpError{
			Provider:   p.Name,
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfter(resp.Header, recv),
			Body:       strings.TrimSpace(string(body)),
		}
	}
	fixes, err := decode(resp.Body, p.Name, recv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	return fixes, nil
}

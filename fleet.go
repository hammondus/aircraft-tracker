package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Member is one aircraft we track. Hex is the ICAO 24-bit address, lowercase,
// and is the key every provider response is joined on.
type Member struct {
	Hex  string `json:"hex,omitempty"` // derived from Rego when omitted
	Rego string `json:"rego"`
	Type string `json:"type,omitempty"` // ICAO type code, e.g. B190
	Desc string `json:"desc,omitempty"` // human readable, e.g. "Beech 1900C-1"
	// Verified records that a person who knows the aircraft has reconciled Desc
	// against the register and stands by it. It silences -casa for this entry.
	//
	// Sometimes that is because the register is wrong; more often it is because
	// the difference does not matter here. The three Commanders are recorded as
	// "Aero Commander 500 Shrike" while the register splits them by variant --
	// a distinction that means something to an engineer and nothing to a map.
	// Either way, a check that nags forever about a difference already settled
	// teaches you to ignore it.
	Verified bool `json:"verified,omitempty"`
}

// duration lets the config file say "1s" instead of a nanosecond count.
type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// clockTime is a time of day in UTC, minutes since midnight. Written "15:04"
// in config. UTC deliberately: the alternative is a local window that shifts by
// an hour twice a year when Australian daylight saving starts and stops, which
// is a silent behaviour change nobody would connect to the clocks going back.
type clockTime int

func (c *clockTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return fmt.Errorf("time %q: want HH:MM in UTC", s)
	}
	*c = clockTime(t.Hour()*60 + t.Minute())
	return nil
}

func (c clockTime) String() string { return fmt.Sprintf("%02d:%02dZ", int(c)/60, int(c)%60) }

// QuietHours is a UTC window with its own idle rate. It applies only while the
// fleet is idle -- an aircraft detected at 3am is tracked at the full rate like
// any other, because the reason to watch it is the same.
type QuietHours struct {
	From clockTime `json:"from"`
	To   clockTime `json:"to"`
	// IdleInterval replaces Config.IdleInterval inside the window.
	IdleInterval duration `json:"idle_interval"`
}

// active reports whether t falls in [From, To). Windows that wrap midnight
// (e.g. 20:00-06:00) are handled, since that is the natural way to express a
// night for most of the world -- ours happens not to wrap.
func (q QuietHours) active(t time.Time) bool {
	if q.IdleInterval.Duration <= 0 || q.From == q.To {
		return false // disabled
	}
	u := t.UTC()
	m := clockTime(u.Hour()*60 + u.Minute())
	if q.From < q.To {
		return m >= q.From && m < q.To
	}
	return m >= q.From || m < q.To
}

type Config struct {
	Listen string `json:"listen"`
	// PollInterval is the default upstream poll rate, used for any provider
	// that does not set its own.
	PollInterval duration `json:"poll_interval"`
	// BroadcastInterval is how often connected clients receive an update. It is
	// deliberately independent of PollInterval: upstream rate limits differ per
	// provider and can force a slow poll, but clients should still get a
	// steady, predictable stream.
	BroadcastInterval duration `json:"broadcast_interval"`
	// IdleInterval is the poll rate while no fleet member has been seen for
	// IdleTimeout. Most of the day nothing is flying, and there is no reason to
	// spend a free service's capacity confirming that ten times a minute.
	IdleInterval duration `json:"idle_interval"`
	// IdleTimeout is how long after the last detection we drop to IdleInterval.
	IdleTimeout duration `json:"idle_timeout"`
	// QuietHours overrides IdleInterval overnight. Set its idle_interval to
	// "0s" to disable the window.
	QuietHours QuietHours `json:"quiet_hours"`
	Providers  []Provider `json:"providers"`
	// Fleet lists the aircraft to track, inline. Fine for one or two.
	Fleet []Member `json:"fleet"`
	// FleetFile is a path to a JSON array of the same objects, resolved relative
	// to the config file, and mutually exclusive with Fleet.
	//
	// Worth the extra concept because the two have opposite handling: the fleet
	// is hand-curated, grows over time and deserves version history, whereas
	// config.json can never be committed since it holds the password hash.
	// Keeping the list hostage to a gitignored file would lose exactly the part
	// worth keeping.
	FleetFile string `json:"fleet_file"`

	// PasswordHash is the shared login password, encoded by `-hashpw`. Empty
	// disables authentication, which is refused unless -insecure is also given
	// -- an accidentally unauthenticated deployment should not be one typo away.
	PasswordHash string   `json:"password_hash"`
	SessionTTL   duration `json:"session_ttl"`
	// TilesPath is the Protomaps archive built by `make tiles`.
	TilesPath string `json:"tiles_path"`
	// HistoryPath is the SQLite position archive. Nothing is ever pruned; at
	// this fleet size it grows by a few hundred MB a year. Use ":memory:" to
	// record nothing.
	HistoryPath string `json:"history_path"`
}

// vhToHex derives an ICAO 24-bit address from an Australian VH- registration.
// Australia allocates addresses algorithmically from the three-letter suffix:
//
//	hex = 0x7C0000 + L1*1296 + L2*36 + L3      (A=0 … Z=25)
//
// Verified against 16 aircraft: three observed live from the providers
// (VH-BYG, VH-YID, VH-PVQ) and the 13 in fleet.json cross-checked against
// adsbdb.com. Every one an exact match.
//
// This matters because the providers' /registration/ endpoint is a *live*
// query -- it only answers while the aircraft is airborne and being received.
// Deriving offline means the fleet can be configured at any time, including for
// an aircraft that is parked or newly acquired.
//
// Only valid for three-letter VH- registrations; anything else must set Hex
// explicitly in the config.
func vhToHex(rego string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(rego))
	s = strings.TrimPrefix(s, "VH-")
	if len(s) != 3 {
		return "", fmt.Errorf("registration %q is not a three-letter VH- rego; set hex explicitly", rego)
	}
	var n uint32
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return "", fmt.Errorf("registration %q contains non-letter %q; set hex explicitly", rego, c)
		}
		n = n*36 + uint32(c-'A')
	}
	return fmt.Sprintf("%06x", 0x7C0000+n), nil
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // a typo'd key silently doing nothing is worse than a startup failure
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.loadFleetFile(path); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.resolvePaths(filepath.Dir(path))
	if err := c.normalise(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// loadFleetFile resolves and reads FleetFile, if set. Done here rather than in
// normalise because only LoadConfig knows the config's own path, which the
// fleet path is relative to.
func (c *Config) loadFleetFile(configPath string) error {
	if c.FleetFile == "" {
		return nil
	}
	if len(c.Fleet) > 0 {
		return fmt.Errorf("both fleet and fleet_file are set; use one or the other")
	}
	p := c.FleetFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(configPath), p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("fleet_file: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // "reg" instead of "rego" should not silently vanish
	if err := dec.Decode(&c.Fleet); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

// resolvePaths makes the tile and history paths relative to the config file,
// exactly as fleet_file already is.
//
// Without this they resolve against the process's working directory, which in
// the container is /data -- so the default "history.db" lands at
// /data/history.db, inside the image and unwritable, rather than in the mounted
// /data/history. That produced a restart loop whose message pointed at
// permissions when the real fault was the path. Paths in a config file should
// mean what they look like they mean: relative to that file.
func (c *Config) resolvePaths(dir string) {
	for _, p := range []*string{&c.TilesPath, &c.HistoryPath} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
}

func (c *Config) normalise() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.PollInterval.Duration <= 0 {
		c.PollInterval.Duration = defaultPollInterval
	}
	if c.BroadcastInterval.Duration <= 0 {
		c.BroadcastInterval.Duration = time.Second
	}
	if c.SessionTTL.Duration <= 0 {
		c.SessionTTL.Duration = defaultSessionTTL
	}
	if c.TilesPath == "" {
		c.TilesPath = defaultTilesPath
	}
	if c.HistoryPath == "" {
		c.HistoryPath = defaultHistoryPath
	}
	if c.IdleInterval.Duration <= 0 {
		c.IdleInterval.Duration = defaultIdleInterval
	}
	if c.IdleTimeout.Duration <= 0 {
		c.IdleTimeout.Duration = defaultIdleTimeout
	}
	// A zero QuietHours means "unset", so apply the defaults. Disabling it is
	// spelled explicitly with an idle_interval of "0s", or an equal from/to.
	if c.QuietHours == (QuietHours{}) {
		c.QuietHours = defaultQuietHours()
	}
	// Idle rates may only ever be slower than the active rate. Otherwise
	// "idle" becomes a way to poll harder while nothing is happening, which is
	// precisely backwards.
	if c.IdleInterval.Duration < c.PollInterval.Duration {
		return fmt.Errorf("idle_interval %s is faster than poll_interval %s; idling may only slow polling",
			c.IdleInterval.Duration, c.PollInterval.Duration)
	}
	if q := c.QuietHours.IdleInterval.Duration; q > 0 && q < c.IdleInterval.Duration {
		return fmt.Errorf("quiet_hours idle_interval %s is faster than idle_interval %s; quiet hours may only slow polling",
			q, c.IdleInterval.Duration)
	}
	if len(c.Providers) == 0 {
		c.Providers = DefaultProviders()
	}
	for i, p := range c.Providers {
		if p.URL == "" {
			return fmt.Errorf("provider %q has no url", p.Name)
		}
		if p.Name == "" {
			return fmt.Errorf("provider %d has no name", i)
		}
	}
	if len(c.Fleet) == 0 {
		return fmt.Errorf("fleet is empty")
	}

	seen := make(map[string]string, len(c.Fleet))
	for i := range c.Fleet {
		m := &c.Fleet[i]
		if m.Hex == "" {
			h, err := vhToHex(m.Rego)
			if err != nil {
				return err
			}
			m.Hex = h
		}
		m.Hex = strings.ToLower(m.Hex)
		if prev, dup := seen[m.Hex]; dup {
			return fmt.Errorf("hex %s claimed by both %s and %s", m.Hex, prev, m.Rego)
		}
		seen[m.Hex] = m.Rego
	}
	return nil
}

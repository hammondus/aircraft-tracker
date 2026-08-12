package main

import (
	"encoding/json"
	"fmt"
	"os"
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

type Config struct {
	Listen string `json:"listen"`
	// PollInterval is the default upstream poll rate, used for any provider
	// that does not set its own.
	PollInterval duration `json:"poll_interval"`
	// BroadcastInterval is how often connected clients receive an update. It is
	// deliberately independent of PollInterval: upstream rate limits differ per
	// provider and can force a slow poll, but clients should still get a
	// steady, predictable stream.
	BroadcastInterval duration   `json:"broadcast_interval"`
	Providers         []Provider `json:"providers"`
	Fleet             []Member   `json:"fleet"`
}

// vhToHex derives an ICAO 24-bit address from an Australian VH- registration.
// Australia allocates addresses algorithmically from the three-letter suffix:
//
//	hex = 0x7C0000 + L1*1296 + L2*36 + L3      (A=0 … Z=25)
//
// Verified against VH-BYG/7c0876, VH-YID/7c7aa3, VH-PVQ/7c4ef4, VH-YSO/7c7c16
// and VH-TAV/7c6045.
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
	if err := c.normalise(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
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

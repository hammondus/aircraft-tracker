package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigDerivesHexes(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{
		"poll_interval": "2s",
		"fleet": [
			{"rego": "VH-YSO", "type": "B190", "desc": "Beech 1900C-1"},
			{"rego": "VH-TAV", "type": "P68"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Fleet[0].Hex != "7c7c16" || c.Fleet[1].Hex != "7c6045" {
		t.Errorf("hexes not derived: %+v", c.Fleet)
	}
	if c.PollInterval.Duration != 2*time.Second {
		t.Errorf("poll_interval = %v", c.PollInterval.Duration)
	}
	if c.Listen != ":8080" {
		t.Errorf("listen default = %q", c.Listen)
	}
	if len(c.Providers) != 2 {
		t.Errorf("expected both default providers, got %+v", c.Providers)
	}
}

// Non-VH aircraft can't be derived, so an explicit hex must be honoured.
func TestLoadConfigExplicitHex(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{
		"fleet": [{"rego": "N123AB", "hex": "A1B2C3"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Fleet[0].Hex != "a1b2c3" {
		t.Errorf("hex = %q, want lowercased a1b2c3", c.Fleet[0].Hex)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	for name, body := range map[string]string{
		"empty fleet":      `{"fleet": []}`,
		"underivable rego": `{"fleet": [{"rego": "N123AB"}]}`,
		"bad duration":     `{"poll_interval": "soon", "fleet": [{"rego":"VH-YSO"}]}`,
		// A typo'd key silently doing nothing is worse than failing at startup.
		"unknown field": `{"poll_intervall": "1s", "fleet": [{"rego":"VH-YSO"}]}`,
		// Two entries resolving to one hex would make the fleet map lossy.
		"duplicate hex": `{"fleet": [{"rego":"VH-YSO"},{"rego":"VH-YSO"}]}`,
	} {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// The shipped example must actually load, or it is worse than no example.
func TestExampleConfigIsValid(t *testing.T) {
	c, err := LoadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Fleet) != 2 {
		t.Errorf("got %d aircraft", len(c.Fleet))
	}
}

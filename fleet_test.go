package main

import (
	"encoding/json"
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
	if len(c.Providers) != len(DefaultProviders()) {
		t.Errorf("expected the default providers, got %+v", c.Providers)
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

// The shipped example must actually load, or it is worse than no example. It
// must also demonstrate the pattern the project actually uses: pointing at
// fleet.json rather than repeating a fleet inline, so that copying it does not
// leave two places claiming to hold the aircraft list.
func TestExampleConfigIsValid(t *testing.T) {
	c, err := LoadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.FleetFile == "" {
		t.Error("the example should use fleet_file, not an inline fleet")
	}
	if len(c.Fleet) == 0 {
		t.Error("the example resolved to an empty fleet")
	}
}

// The example config must not pin intervals: the measured-safe defaults live in
// code, and duplicating them here means two places to update and one to forget.
// That drift already happened once -- the example pinned 2s after the default
// moved to 5s, so the app quietly ran at a rate that draws rate limits.
func TestExampleConfigDoesNotPinIntervals(t *testing.T) {
	b, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"poll_interval", "broadcast_interval", "providers",
		"idle_interval", "idle_timeout", "quiet_hours",
	} {
		if _, ok := raw[k]; ok {
			t.Errorf("example config pins %q; leave it to the defaults", k)
		}
	}

	c, err := LoadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval.Duration != defaultPollInterval {
		t.Errorf("example resolves to %v, want the measured default %v",
			c.PollInterval.Duration, defaultPollInterval)
	}
}

// The fleet lives in its own file so it can be version-controlled: config.json
// holds the password hash and can never be committed, but the aircraft list is
// hand-curated, grows over time, and is exactly the part worth keeping.
func TestFleetFileIsLoaded(t *testing.T) {
	dir := t.TempDir()
	fleet := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(fleet, []byte(`[
		{"rego": "VH-YSO", "type": "B190", "desc": "Beech 1900C-1"},
		{"rego": "VH-WAM", "type": "AC50"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"fleet_file": "fleet.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Fleet) != 2 {
		t.Fatalf("got %d aircraft, want 2", len(c.Fleet))
	}
	// Hexes still derive, exactly as for an inline fleet.
	if c.Fleet[0].Hex != "7c7c16" || c.Fleet[1].Hex != "7c6f6c" {
		t.Errorf("hexes not derived: %+v", c.Fleet)
	}
	if c.Fleet[0].Desc != "Beech 1900C-1" {
		t.Errorf("desc lost: %+v", c.Fleet[0])
	}
}

func TestFleetFileRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("good.json", `[{"rego":"VH-YSO"}]`)
	// A typo'd key must fail loudly rather than silently produce a nameless
	// aircraft, which is the same reasoning as DisallowUnknownFields on config.
	write("typo.json", `[{"reg":"VH-YSO"}]`)
	write("notarray.json", `{"rego":"VH-YSO"}`)
	write("empty.json", `[]`)

	for name, cfg := range map[string]string{
		"missing file":     `{"fleet_file": "nope.json"}`,
		"typo'd key":       `{"fleet_file": "typo.json"}`,
		"object not array": `{"fleet_file": "notarray.json"}`,
		"empty fleet":      `{"fleet_file": "empty.json"}`,
		// Two sources of truth for the same list is a configuration bug.
		"both fleet and fleet_file": `{"fleet_file": "good.json", "fleet": [{"rego":"VH-TAV"}]}`,
	} {
		p := write("cfg.json", cfg)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The committed fleet must actually load, and every registration in it must
// resolve to a distinct aircraft.
func TestShippedFleetFileIsValid(t *testing.T) {
	b, err := os.ReadFile("fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	var members []Member
	if err := json.Unmarshal(b, &members); err != nil {
		t.Fatal(err)
	}
	if len(members) == 0 {
		t.Fatal("fleet.json is empty")
	}
	seen := map[string]string{}
	for _, m := range members {
		hex, err := vhToHex(m.Rego)
		if err != nil {
			t.Errorf("%s: %v", m.Rego, err)
			continue
		}
		if prev, dup := seen[hex]; dup {
			t.Errorf("%s and %s both resolve to %s", prev, m.Rego, hex)
		}
		seen[hex] = m.Rego
		if m.Type == "" {
			t.Errorf("%s has no type", m.Rego)
		}
	}
}

// Paths in a config file should mean what they look like they mean: relative to
// that file, exactly as fleet_file already is. Resolving them against the
// process's working directory instead put the database inside the container
// image rather than the mounted volume, and produced a restart loop whose
// message blamed permissions.
func TestPathsResolveRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.json"), []byte(`[{"rego":"VH-YSO"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"fleet_file": "fleet.json",
		"tiles_path": "tiles/australia.pmtiles",
		"history_path": "history/history.db"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "tiles/australia.pmtiles"); c.TilesPath != want {
		t.Errorf("tiles_path = %q, want %q", c.TilesPath, want)
	}
	if want := filepath.Join(dir, "history/history.db"); c.HistoryPath != want {
		t.Errorf("history_path = %q, want %q", c.HistoryPath, want)
	}
}

// An absolute path is taken as given, so existing deployments keep working.
func TestAbsolutePathsAreLeftAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.json"), []byte(`[{"rego":"VH-YSO"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{
		"fleet_file": "f.json",
		"tiles_path": "/data/tiles/australia.pmtiles",
		"history_path": "/data/history/history.db"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TilesPath != "/data/tiles/australia.pmtiles" || c.HistoryPath != "/data/history/history.db" {
		t.Errorf("absolute paths were rewritten: %q, %q", c.TilesPath, c.HistoryPath)
	}
}

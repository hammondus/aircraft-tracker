package main

import (
	"testing"
	"time"
)

func utc(hour, min int) time.Time {
	return time.Date(2026, 8, 13, hour, min, 0, 0, time.UTC)
}

// testIdlePoller builds a poller with the shipped defaults and no sightings.
func testIdlePoller(t *testing.T) *Poller {
	t.Helper()
	c := &Config{Fleet: []Member{{Rego: "VH-YSO"}, {Rego: "VH-TAV"}}}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	return NewPoller(c)
}

func TestQuietHoursWindow(t *testing.T) {
	q := defaultQuietHours() // 12:00-20:00 UTC

	for _, tc := range []struct {
		at   time.Time
		want bool
		why  string
	}{
		{utc(11, 59), false, "just before the window"},
		{utc(12, 0), true, "start is inclusive"},
		{utc(16, 0), true, "middle"},
		{utc(19, 59), true, "last minute"},
		{utc(20, 0), false, "end is exclusive"},
		{utc(0, 0), false, "midnight UTC is daytime in Australia"},
		{utc(23, 30), false, "late UTC is mid-morning in Australia"},
	} {
		if got := q.active(tc.at); got != tc.want {
			t.Errorf("%s (%s): active = %v, want %v", tc.why, tc.at.Format("15:04"), got, tc.want)
		}
	}
}

// The window is evaluated in UTC regardless of the machine's zone, which is the
// entire point of specifying it that way -- an Australian local window would
// shift an hour when daylight saving starts.
func TestQuietHoursIgnoresLocalZone(t *testing.T) {
	q := defaultQuietHours()
	syd, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	inWindow := utc(16, 0)
	if !q.active(inWindow) || !q.active(inWindow.In(syd)) {
		t.Error("same instant judged differently in two zones")
	}
	// A summer instant, when Sydney is UTC+11 rather than +10.
	summer := time.Date(2026, 1, 15, 16, 0, 0, 0, time.UTC)
	if !q.active(summer) || !q.active(summer.In(syd)) {
		t.Error("daylight saving changed the window")
	}
}

// Windows that wrap midnight must work, even though ours does not.
func TestQuietHoursWrappingMidnight(t *testing.T) {
	q := QuietHours{From: 20 * 60, To: 6 * 60, IdleInterval: duration{time.Minute}}
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{utc(19, 59), false}, {utc(20, 0), true}, {utc(23, 59), true},
		{utc(0, 0), true}, {utc(5, 59), true}, {utc(6, 0), false}, {utc(12, 0), false},
	} {
		if got := q.active(tc.at); got != tc.want {
			t.Errorf("%s: active = %v, want %v", tc.at.Format("15:04"), got, tc.want)
		}
	}
}

func TestQuietHoursDisabled(t *testing.T) {
	for name, q := range map[string]QuietHours{
		"zero value":    {},
		"zero interval": {From: 12 * 60, To: 20 * 60},
		"empty window":  {From: 12 * 60, To: 12 * 60, IdleInterval: duration{time.Minute}},
	} {
		if q.active(utc(16, 0)) {
			t.Errorf("%s: should be inactive", name)
		}
	}
}

// The core of the scheme: an idle fleet is polled slowly, and any detection at
// any hour restores the full rate.
func TestPollModeFollowsFleetActivity(t *testing.T) {
	p := testIdlePoller(t)
	day, night := utc(6, 0), utc(16, 0)

	// Nothing ever seen.
	if mode, iv := p.modeAt(day); mode != modeIdle || iv != defaultIdleInterval {
		t.Errorf("idle daytime: mode=%s interval=%v, want %s/%v", mode, iv, modeIdle, defaultIdleInterval)
	}
	if mode, iv := p.modeAt(night); mode != modeIdleQuiet || iv != defaultQuietIdle {
		t.Errorf("idle overnight: mode=%s interval=%v, want %s/%v", mode, iv, modeIdleQuiet, defaultQuietIdle)
	}

	// An aircraft appears. Time of day stops mattering.
	p.merge([]Fix{{Hex: p.members[0].Hex, At: day}})
	if mode, _ := p.modeAt(day); mode != modeActive {
		t.Errorf("after a fix in daytime: mode = %s, want %s", mode, modeActive)
	}
	p.merge([]Fix{{Hex: p.members[0].Hex, At: night}})
	if mode, _ := p.modeAt(night); mode != modeActive {
		t.Errorf("a 3am flight must still be tracked at full rate: mode = %s", mode)
	}
}

func TestIdleTimeoutBoundary(t *testing.T) {
	p := testIdlePoller(t)
	seen := utc(6, 0)
	p.merge([]Fix{{Hex: p.members[0].Hex, At: seen}})

	for _, tc := range []struct {
		after time.Duration
		want  pollMode
	}{
		{0, modeActive},
		{defaultIdleTimeout - time.Second, modeActive},
		{defaultIdleTimeout, modeActive}, // boundary inclusive
		{defaultIdleTimeout + time.Second, modeIdle},
		{time.Hour, modeIdle},
	} {
		if mode, _ := p.modeAt(seen.Add(tc.after)); mode != tc.want {
			t.Errorf("%v after last fix: mode = %s, want %s", tc.after, mode, tc.want)
		}
	}
}

// One aircraft flying keeps the whole fleet at the fast rate -- the rate is a
// property of the poller, not of an individual aircraft.
func TestOneActiveAircraftKeepsFleetFast(t *testing.T) {
	p := testIdlePoller(t)
	now := utc(6, 0)
	p.merge([]Fix{
		{Hex: p.members[0].Hex, At: now.Add(-time.Hour)}, // long gone
		{Hex: p.members[1].Hex, At: now},                 // airborne
	})
	if mode, _ := p.modeAt(now); mode != modeActive {
		t.Errorf("mode = %s, want %s", mode, modeActive)
	}
}

// An aircraft parked with its transponder on counts as active: it is a strong
// signal of an imminent departure, which is when the fast rate earns its keep.
func TestOnGroundCountsAsActive(t *testing.T) {
	p := testIdlePoller(t)
	now := utc(6, 0)
	p.merge([]Fix{{Hex: p.members[0].Hex, At: now, OnGround: true}})
	if mode, _ := p.modeAt(now); mode != modeActive {
		t.Errorf("a powered aircraft on the ground should keep us active: mode = %s", mode)
	}
}

func TestWaitForCombinesModeAndCooldown(t *testing.T) {
	p := testIdlePoller(t)
	s := p.sources[0]
	day, night := utc(6, 0), utc(16, 0)

	if got := p.waitFor(s, day); got != defaultIdleInterval {
		t.Errorf("idle daytime: %v, want %v", got, defaultIdleInterval)
	}
	if got := p.waitFor(s, night); got != defaultQuietIdle {
		t.Errorf("idle overnight: %v, want %v", got, defaultQuietIdle)
	}

	p.merge([]Fix{{Hex: p.members[0].Hex, At: day}})
	if got := p.waitFor(s, day); got != defaultPollInterval {
		t.Errorf("active: %v, want %v", got, defaultPollInterval)
	}

	// The adaptive cooldown stacks on whichever base applies.
	s.cooldown = 30 * time.Second
	if got, want := p.waitFor(s, day), defaultPollInterval+30*time.Second; got != want {
		t.Errorf("active with cooldown: %v, want %v", got, want)
	}
	if got, want := p.waitFor(s, night.Add(time.Hour)), defaultQuietIdle+30*time.Second; got != want {
		t.Errorf("idle with cooldown: %v, want %v", got, want)
	}
}

// A provider configured slower than the idle rate keeps its own rate.
func TestPerProviderIntervalSurvivesIdling(t *testing.T) {
	c := &Config{
		Fleet:     []Member{{Rego: "VH-YSO"}},
		Providers: []Provider{{Name: "slow", URL: "http://slow/", Interval: duration{10 * time.Minute}}},
	}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	if got := p.waitFor(p.sources[0], utc(6, 0)); got != 10*time.Minute {
		t.Errorf("wait = %v, want the provider's own 10m", got)
	}
}

func TestConfigRejectsIdleFasterThanActive(t *testing.T) {
	for name, body := range map[string]string{
		"idle faster than poll": `{"poll_interval":"10s","idle_interval":"5s","fleet":[{"rego":"VH-YSO"}]}`,
		"quiet faster than idle": `{"poll_interval":"10s","idle_interval":"2m",
			"quiet_hours":{"from":"12:00","to":"20:00","idle_interval":"30s"},
			"fleet":[{"rego":"VH-YSO"}]}`,
	} {
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestConfigParsesQuietHours(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{
		"idle_interval": "3m",
		"idle_timeout": "20m",
		"quiet_hours": {"from": "21:30", "to": "05:45", "idle_interval": "9m"},
		"fleet": [{"rego": "VH-YSO"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.QuietHours.From, clockTime(21*60+30); got != want {
		t.Errorf("from = %v, want %v", got, want)
	}
	if got, want := c.QuietHours.To, clockTime(5*60+45); got != want {
		t.Errorf("to = %v, want %v", got, want)
	}
	if got := c.IdleTimeout.Duration; got != 20*time.Minute {
		t.Errorf("idle_timeout = %v", got)
	}
	if got, want := c.QuietHours.From.String(), "21:30Z"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestConfigRejectsBadClockTime(t *testing.T) {
	for name, v := range map[string]string{
		"not a time": `"lunchtime"`,
		"bad hour":   `"25:00"`,
		"bad minute": `"12:61"`,
		"seconds":    `"12:00:00"`,
		"not string": `1200`,
		"empty":      `""`,
	} {
		body := `{"quiet_hours": {"from": ` + v + `, "to": "20:00", "idle_interval": "5m"}, "fleet": [{"rego":"VH-YSO"}]}`
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error for from=%s", name, v)
		}
	}
}

// The shipped defaults must be the ones agreed: 10s when anything is flying,
// 2m when idle, 5m when idle overnight, falling back after 10m of quiet.
func TestShippedDefaults(t *testing.T) {
	c, err := LoadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"active", c.PollInterval.Duration, 10 * time.Second},
		{"idle", c.IdleInterval.Duration, 2 * time.Minute},
		{"idle timeout", c.IdleTimeout.Duration, 10 * time.Minute},
		{"idle overnight", c.QuietHours.IdleInterval.Duration, 5 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if c.QuietHours.From != 12*60 || c.QuietHours.To != 20*60 {
		t.Errorf("quiet window = %s-%s, want 12:00Z-20:00Z", c.QuietHours.From, c.QuietHours.To)
	}
}

// An airliner is always flying somewhere, so counting reference aircraft as
// activity would pin the poller at its fast rate forever and undo the entire
// idle scheme.
func TestReferenceAircraftDoNotDefeatIdling(t *testing.T) {
	c := &Config{Fleet: []Member{
		{Rego: "VH-YSO"},
		{Rego: "VH-VXA", Reference: true},
	}}
	if err := c.normalise(); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c)
	now := utc(6, 0)

	// The airliner is up, ours is not.
	p.merge([]Fix{{Hex: p.members[1].Hex, At: now}})
	if mode, _ := p.modeAt(now); mode != modeIdle {
		t.Errorf("a reference aircraft alone put the poller in %s, want idle", mode)
	}

	// Ours appears, and only then does the rate rise.
	p.merge([]Fix{{Hex: p.members[0].Hex, At: now}})
	if mode, _ := p.modeAt(now); mode != modeActive {
		t.Errorf("our own aircraft did not restore the fast rate: %s", mode)
	}
}

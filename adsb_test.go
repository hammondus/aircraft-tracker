package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestVHToHex(t *testing.T) {
	// Observed live from the providers, or confirmed against adsbdb.com.
	ok := map[string]string{
		"VH-BYG":   "7c0876",
		"VH-YID":   "7c7aa3",
		"VH-PVQ":   "7c4ef4",
		"VH-YSO":   "7c7c16",
		"VH-TAV":   "7c6045",
		"vh-yso":   "7c7c16", // case insensitive
		"YSO":      "7c7c16", // prefix optional
		" VH-TAV ": "7c6045",
	}
	for rego, want := range ok {
		got, err := vhToHex(rego)
		if err != nil {
			t.Errorf("vhToHex(%q) unexpected error: %v", rego, err)
			continue
		}
		if got != want {
			t.Errorf("vhToHex(%q) = %s, want %s", rego, got, want)
		}
	}

	// Anything not a three-letter VH- rego must fail loudly rather than
	// silently produce a plausible-looking wrong address.
	for _, bad := range []string{"N12345", "VH-AB", "VH-ABCD", "VH-12A", "", "VH-"} {
		if got, err := vhToHex(bad); err == nil {
			t.Errorf("vhToHex(%q) = %s, want error", bad, got)
		}
	}
}

func TestAltBaroUnmarshal(t *testing.T) {
	var a altBaro
	if err := a.UnmarshalJSON([]byte(`27100`)); err != nil {
		t.Fatal(err)
	}
	if a.Feet != 27100 || a.OnGround {
		t.Errorf("numeric: got %+v", a)
	}

	a = altBaro{}
	if err := a.UnmarshalJSON([]byte(`"ground"`)); err != nil {
		t.Fatal(err)
	}
	if !a.OnGround || a.Feet != 0 {
		t.Errorf(`"ground": got %+v`, a)
	}
}

// TestDecodeRealResponse runs against a response captured from adsb.lol,
// deliberately containing aircraft on the ground (alt_baro is the string
// "ground") and an MLAT-derived position.
func TestDecodeRealResponse(t *testing.T) {
	f, err := os.Open("testdata/response.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	recv := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	fixes, err := decode(f, "adsb.lol", recv)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(fixes) == 0 {
		t.Fatal("no fixes decoded")
	}

	byHex := map[string]Fix{}
	for _, fx := range fixes {
		byHex[fx.Hex] = fx
		if fx.Source != "adsb.lol" {
			t.Errorf("%s: source = %q", fx.Hex, fx.Source)
		}
		if fx.Lat == 0 || fx.Lon == 0 {
			t.Errorf("%s: zero coordinate %v,%v", fx.Hex, fx.Lat, fx.Lon)
		}
		// seen_pos is an age, so a fix can never be stamped in the future.
		if fx.At.After(recv) {
			t.Errorf("%s: fix time %v is after receipt %v", fx.Hex, fx.At, recv)
		}
		if strings.ContainsAny(fx.Flight, " ") {
			t.Errorf("%s: flight %q not trimmed", fx.Hex, fx.Flight)
		}
	}

	if g, ok := byHex["7c78aa"]; !ok {
		t.Error("7c78aa missing")
	} else if !g.OnGround || g.AltFt != 0 {
		t.Errorf("7c78aa should be on ground: %+v", g)
	}
	if m, ok := byHex["7c1468"]; !ok {
		t.Error("7c1468 missing")
	} else if !m.MLAT {
		t.Error("7c1468 should be flagged MLAT")
	}
	if a, ok := byHex["7c4319"]; !ok {
		t.Error("7c4319 missing")
	} else if a.OnGround || a.AltFt != 29000 {
		t.Errorf("7c4319 should be airborne at 29000: %+v", a)
	}
}

// An aircraft can be heard without its position being resolved. Those entries
// must be dropped rather than plotted at 0,0 in the Gulf of Guinea.
func TestDecodeSkipsPositionlessAircraft(t *testing.T) {
	body := strings.NewReader(`{"ac":[
		{"hex":"7c7c16","flight":"XYZ     ","alt_baro":"ground"},
		{"hex":"7c6045","lat":-33.5,"lon":151.2,"alt_baro":1200,"seen_pos":2.5}
	]}`)
	recv := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	fixes, err := decode(body, "test", recv)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 1 {
		t.Fatalf("got %d fixes, want 1: %+v", len(fixes), fixes)
	}
	if fixes[0].Hex != "7c6045" {
		t.Errorf("kept the wrong aircraft: %+v", fixes[0])
	}
	// seen_pos of 2.5s means the fix is 2.5s older than the response.
	if want := recv.Add(-2500 * time.Millisecond); !fixes[0].At.Equal(want) {
		t.Errorf("At = %v, want %v", fixes[0].At, want)
	}
}

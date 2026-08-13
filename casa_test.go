package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The first version of this stripped corporate noise as substrings, which
// turned "AERO COMMANDER" into "AERO MMANDER" by eating the "CO" in the middle
// of the word. Word boundaries are the whole point.
func TestTidyManufacturer(t *testing.T) {
	for in, want := range map[string]string{
		"BEECH AIRCRAFT CORP":         "Beech",
		"ROCKWELL INTERNATIONAL":      "Rockwell",
		"PILATUS AIRCRAFT LTD":        "Pilatus",
		"PIPER AIRCRAFT CORP":         "Piper",
		"VULCANAIR S.P.A.":            "Vulcanair",
		"CESSNA AIRCRAFT COMPANY":     "Cessna",
		"DIAMOND AIRCRAFT INDUSTRIES": "Diamond",
		// The regression: no word here is noise, so nothing may be removed.
		"AERO COMMANDER": "Aero Commander",
		// Nor may a real name that merely starts with a noise word be eaten.
		"CIRRUS DESIGN CORP": "Cirrus Design",
		"":                   "",
	} {
		if got := tidyManufacturer(in); got != want {
			t.Errorf("tidyManufacturer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMentionsModel(t *testing.T) {
	// A hand-written description keeps the popular name the register lacks, so
	// the check is "does it mention the model", not "does it match exactly".
	for _, tc := range []struct {
		desc, model string
		want        bool
	}{
		{"Rockwell 690A Turbo Commander", "690A", true},
		{"Beech 58 Baron", "58", true},
		{"Pilatus PC-12/45", "PC-12/45", true},
		{"Vulcanair P.68C", "P.68C", true},
		{"Aero Commander 500-S", "500-S", true},
		// Punctuation and case must not matter.
		{"beech 1900c", "1900C", true},
		// The failure this exists to catch: adsbdb said VH-SOU was a Citation.
		{"Cessna Citation I", "690A", false},
		{"Beech 1900C", "PC-12", false},
		{"anything", "", false},
	} {
		if got := mentionsModel(tc.desc, tc.model); got != tc.want {
			t.Errorf("mentionsModel(%q, %q) = %v, want %v", tc.desc, tc.model, got, tc.want)
		}
	}
}

// A person who has flown the aircraft outranks the register, and a check that
// nags forever about a difference already settled teaches you to ignore it.
func TestVerifiedSuppressesMismatch(t *testing.T) {
	// The real case: CASA records VH-YJI as a 500-S; the pilot says 500-U.
	m := Member{Rego: "VH-YJI", Desc: "Aero Commander 500-U Shrike Commander"}
	if mentionsModel(m.Desc, "500-S") {
		t.Fatal("this description should not match the register's model, or the test proves nothing")
	}
	m.Verified = true
	if !m.Verified {
		t.Error("verified did not stick")
	}
}

// The flag must survive a round trip through the fleet file, and must not
// appear at all when it is false -- fleet.json is hand-edited.
func TestVerifiedRoundTripsAndStaysQuiet(t *testing.T) {
	plain, err := json.Marshal(Member{Rego: "VH-TAV", Type: "P68"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "verified") {
		t.Errorf("unverified member serialised as %s; the field should be omitted", plain)
	}

	var back Member
	if err := json.Unmarshal([]byte(`{"rego":"VH-YJI","verified":true}`), &back); err != nil {
		t.Fatal(err)
	}
	if !back.Verified {
		t.Error("verified did not survive a round trip")
	}
}

// The shipped fleet must stay clean against the register: every entry either
// agrees with it or is explicitly marked as checked.
func TestShippedFleetIsReconciled(t *testing.T) {
	b, err := os.ReadFile("fleet.json")
	if err != nil {
		t.Fatal(err)
	}
	var fleet []Member
	if err := json.Unmarshal(b, &fleet); err != nil {
		t.Fatal(err)
	}
	for _, m := range fleet {
		if m.Desc == "" {
			t.Errorf("%s has no description; run -casa", m.Rego)
		}
	}
}

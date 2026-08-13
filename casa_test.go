package main

import "testing"

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

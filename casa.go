package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// casaRegisterURL is the Australian civil aircraft register, published by CASA
// as a CSV for exactly this purpose.
//
// Not the search page at casa.gov.au/search-centre/aircraft-register: that sits
// behind an Akamai Bot Manager interstitial with a proof-of-work challenge, so
// it is deliberately closed to automation. It is the right tool for a person
// looking up one aircraft in a browser, and the wrong one for a program. This
// file is the sanctioned route, and one request covers the whole fleet.
const casaRegisterURL = "https://services.casa.gov.au/CSV/acrftreg.csv"

// casaRecord is the handful of register columns worth having.
type casaRecord struct {
	Mark   string
	Manu   string
	Model  string
	Holder string
}

func fetchCASARegister() (map[string]casaRecord, error) {
	req, err := http.NewRequest(http.MethodGet, casaRegisterURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	// The file is UTF-8 with a byte order mark, which would otherwise become
	// part of the first column's name and make every lookup of it fail.
	br := bufio.NewReader(resp.Body)
	if r, _, err := br.ReadRune(); err == nil && r != '\ufeff' {
		br.UnreadRune()
	}

	r := csv.NewReader(br)
	r.FieldsPerRecord = -1 // the register is not perfectly rectangular
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	get := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	out := map[string]casaRecord{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // a malformed row is not worth abandoning the register for
		}
		mark := strings.ToUpper(get(rec, "Mark"))
		if mark == "" {
			continue
		}
		out[mark] = casaRecord{
			Mark:   mark,
			Manu:   get(rec, "Manu"),
			Model:  get(rec, "Model"),
			Holder: get(rec, "regholdname"),
		}
	}
	return out, nil
}

// corporateNoise is the boilerplate in manufacturer names that carries no
// information: "BEECH AIRCRAFT CORP" is just Beech.
//
// Anchored on word boundaries, not substrings. Plain replacement turns
// "AERO COMMANDER" into "AERO MMANDER" by eating the "CO" in the middle of it,
// which is exactly what happened the first time this was written.
var corporateNoise = regexp.MustCompile(
	`(?i)\b(AIRCRAFT|CORPORATION|CORP|COMPANY|CO|INTERNATIONAL|INDUSTRIES|` +
		`INDUSTRIE|PTY|LTD|LIMITED|INCORPORATED|INC|GMBH|S\.?P\.?A|A\.?G|N\.?V|B\.?V)\b\.?`)

func tidyManufacturer(s string) string {
	s = corporateNoise.ReplaceAllString(strings.TrimSpace(s), " ")
	words := strings.Fields(s)
	// "THE BOEING COMPANY" loses its suffix above and would otherwise read as
	// "The Boeing".
	if len(words) > 1 && strings.EqualFold(words[0], "the") {
		words = words[1:]
	}
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

// mentionsModel reports whether a description already refers to the register's
// model, ignoring case, spaces and punctuation, so "Rockwell 690A Turbo
// Commander" is recognised as describing model "690A".
func mentionsModel(desc, model string) bool {
	squash := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	m := squash(model)
	return m != "" && strings.Contains(squash(desc), m)
}

// checkFleetAgainstCASA compares fleet.json with the register, fills in any
// missing description, and reports everything else rather than overwriting.
//
// Descriptions are deliberately not rewritten wholesale: hand-written ones
// carry the popular name -- "Beech 58 Baron", "Turbo Commander" -- which the
// register does not have, and clobbering them would trade information for
// consistency. Nor is the ICAO type code touched: CASA does not publish it.
func checkFleetAgainstCASA(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fleet []Member
	if err := json.Unmarshal(b, &fleet); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	fmt.Fprintf(os.Stderr, "fetching %s\n", casaRegisterURL)
	reg, err := fetchCASARegister()
	if err != nil {
		return fmt.Errorf("casa register: %w", err)
	}
	fmt.Fprintf(os.Stderr, "register holds %d currently-registered aircraft\n\n", len(reg))

	var changed, problems int
	for i := range fleet {
		m := &fleet[i]
		mark := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(m.Rego), "VH-"))
		rec, ok := reg[mark]
		if !ok {
			// CASA publishes only current registrations, so this means sold
			// overseas, scrapped, or the mark lapsed -- not that it never existed.
			fmt.Printf("%-8s NOT REGISTERED  (deregistered, or the mark has lapsed)\n", m.Rego)
			problems++
			continue
		}
		full := strings.TrimSpace(tidyManufacturer(rec.Manu) + " " + rec.Model)
		switch {
		case m.Desc == "":
			m.Desc = full
			changed++
			fmt.Printf("%-8s filled in       %s\n", m.Rego, full)
		case m.Verified:
			fmt.Printf("%-8s verified        %s (register says %q)\n", m.Rego, m.Desc, full)
		case !mentionsModel(m.Desc, rec.Model):
			problems++
			fmt.Printf("%-8s MISMATCH        have %q, register says %q (%s)\n"+
				"%-8s                 set \"verified\": true if you know better than the register\n",
				m.Rego, m.Desc, full, rec.Holder, "")
		default:
			fmt.Printf("%-8s ok              %s — %s\n", m.Rego, full, rec.Holder)
		}
	}

	if changed > 0 {
		out, err := json.MarshalIndent(fleet, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nfilled in %d description(s) in %s\n", changed, path)
	}
	if problems > 0 {
		fmt.Fprintf(os.Stderr, "\n%d aircraft need a look. Mismatches are not corrected "+
			"automatically: the register has no popular names, so overwriting would "+
			"lose more than it fixes.\n", problems)
	}
	return nil
}

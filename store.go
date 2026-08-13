package main

import (
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Recording thresholds.
const (
	// minMovement is how far an aircraft must move before a new fix is worth
	// keeping. An aircraft parked with its transponder powered reports the same
	// position indefinitely; without this the bulk of the archive would be
	// thousands of identical rows a day per aircraft, recording nothing.
	minMovement = 50.0 // metres
	// heartbeat forces a row even when nothing has moved, so the archive can
	// still answer "was it there at all?" rather than going silent.
	heartbeat = time.Minute
	// flightGap is how long out of contact ends a flight, and how long on the
	// ground ends one. See DESIGN-DECISIONS.md §7: this is inferred, and a
	// coverage gap is indistinguishable from a landing.
	flightGap = 10 * time.Minute
	// minFlightFixes discards specks -- a couple of stray fixes is not a flight.
	minFlightFixes = 5
)

const schema = `
CREATE TABLE IF NOT EXISTS fix (
    hex       TEXT    NOT NULL,
    at        INTEGER NOT NULL,   -- unix milliseconds
    lat       REAL    NOT NULL,
    lon       REAL    NOT NULL,
    alt_ft    INTEGER NOT NULL,
    on_ground INTEGER NOT NULL,
    speed_kt  REAL    NOT NULL,
    track_deg REAL    NOT NULL,
    vert_fpm  INTEGER NOT NULL,
    squawk    TEXT,
    mlat      INTEGER NOT NULL,
    source    TEXT    NOT NULL,
    callsign  TEXT
);

-- Both the dedupe mechanism and the query index. Restarts and overlapping
-- providers re-offer fixes we already hold; INSERT OR IGNORE makes that free.
CREATE UNIQUE INDEX IF NOT EXISTS fix_hex_at ON fix(hex, at);
`

// Store is the position archive.
//
// There is deliberately no flights table. Flight segmentation is inferred and
// imperfect, so materialising it would bake a guess into the only copy of the
// data. Flights are derived on query instead: one aircraft over a bounded date
// range is an indexed range scan, which is cheap, and the heuristic can change
// whenever without a migration or a rebuild. If that ever gets slow, cache it
// then -- the fixes remain the truth either way.
type Store struct {
	db *sql.DB

	mu sync.Mutex
	// last is the most recent fix written per aircraft, for thinning. Empty at
	// startup, so the first fix after a restart is always kept -- which is the
	// interesting one anyway.
	last map[string]Fix
}

func OpenStore(path string) (*Store, error) {
	// WAL so the history UI can read while the recorder writes. NORMAL trades a
	// theoretical loss of the last commits on power failure for far fewer
	// fsyncs, which is the right trade for position history.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db, last: map[string]Fix{}}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// worthStoring decides whether a fix earns a row.
func worthStoring(prev *Fix, f Fix) bool {
	if prev == nil {
		return true
	}
	if !f.At.After(prev.At) {
		return false // same fix seen again, or an older one from a slower provider
	}
	if f.At.Sub(prev.At) >= heartbeat {
		return true
	}
	return metresBetween(prev.Lat, prev.Lon, f.Lat, f.Lon) >= minMovement
}

// Record writes whichever fixes in a snapshot are worth keeping. It is the
// Poller's OnUpdate hook, alongside the SSE broadcast.
//
// A failure here is logged by the caller and otherwise ignored: losing history
// is bad, but taking the live display down with it would be worse.
func (s *Store) Record(states []State) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keep []Fix
	for _, st := range states {
		if st.Fix == nil || st.Status == StatusNoContact {
			continue
		}
		prev, ok := s.last[st.Hex]
		var prevp *Fix
		if ok {
			prevp = &prev
		}
		if worthStoring(prevp, *st.Fix) {
			keep = append(keep, *st.Fix)
		}
	}
	if len(keep) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO fix
		(hex, at, lat, lon, alt_ft, on_ground, speed_kt, track_deg, vert_fpm, squawk, mlat, source, callsign)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, f := range keep {
		if _, err := stmt.Exec(f.Hex, f.At.UnixMilli(), f.Lat, f.Lon, f.AltFt,
			f.OnGround, f.SpeedKt, f.TrackDeg, f.VertFPM, f.Squawk, f.MLAT,
			f.Source, f.Flight); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Only after a successful commit, or a failed write would suppress the next
	// attempt at the same position.
	for _, f := range keep {
		s.last[f.Hex] = f
	}
	return len(keep), nil
}

// Track returns stored fixes for one aircraft over a period, oldest first.
func (s *Store) Track(hex string, from, to time.Time) ([]Fix, error) {
	rows, err := s.db.Query(`SELECT hex, at, lat, lon, alt_ft, on_ground, speed_kt,
		track_deg, vert_fpm, squawk, mlat, source, callsign
		FROM fix WHERE hex = ? AND at >= ? AND at <= ? ORDER BY at`,
		hex, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Fix
	for rows.Next() {
		var f Fix
		var ms int64
		var squawk, callsign sql.NullString
		if err := rows.Scan(&f.Hex, &ms, &f.Lat, &f.Lon, &f.AltFt, &f.OnGround,
			&f.SpeedKt, &f.TrackDeg, &f.VertFPM, &squawk, &f.MLAT, &f.Source, &callsign); err != nil {
			return nil, err
		}
		f.At = time.UnixMilli(ms).UTC()
		f.Squawk, f.Flight = squawk.String, callsign.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// Flight is an inferred flight: a run of fixes bounded by a gap in contact or a
// spell on the ground. It is not a record of a flight, and must not be used as
// one -- see DESIGN-DECISIONS.md §7.
type Flight struct {
	Hex        string    `json:"hex"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended"`
	Fixes      int       `json:"fixes"`
	MaxAltFt   int       `json:"max_alt_ft"`
	DistanceNM float64   `json:"distance_nm"`
}

func (f Flight) Duration() time.Duration { return f.Ended.Sub(f.Started) }

// Flights derives flights for one aircraft over a period.
func (s *Store) Flights(hex string, from, to time.Time) ([]Flight, error) {
	fixes, err := s.Track(hex, from, to)
	if err != nil {
		return nil, err
	}
	return segmentFlights(hex, fixes), nil
}

// segmentFlights splits a fix sequence into flights.
//
// Two things end a flight: a gap in contact longer than flightGap, and sitting
// on the ground for longer than flightGap. The second rule is needed because
// stationary aircraft still emit a heartbeat fix every minute, so a parked
// aeroplane leaves no gap to detect.
//
// Runs that never leave the ground are dropped -- an aircraft with its
// transponder on in a hangar is not a flight.
func segmentFlights(hex string, fixes []Fix) []Flight {
	var (
		out         []Flight
		cur         []Fix
		groundSince time.Time
	)
	flush := func() {
		if f, ok := summarise(hex, cur); ok {
			out = append(out, f)
		}
		cur = nil
		groundSince = time.Time{}
	}

	for i, f := range fixes {
		if i > 0 {
			prev := fixes[i-1]
			if f.At.Sub(prev.At) > flightGap {
				flush()
			}
		}
		if f.OnGround {
			if groundSince.IsZero() {
				groundSince = f.At
			} else if f.At.Sub(groundSince) > flightGap {
				flush()
				groundSince = f.At
			}
		} else {
			groundSince = time.Time{}
		}
		cur = append(cur, f)
	}
	flush()
	return out
}

func summarise(hex string, fixes []Fix) (Flight, bool) {
	if len(fixes) < minFlightFixes {
		return Flight{}, false
	}
	f := Flight{
		Hex:     hex,
		Started: fixes[0].At,
		Ended:   fixes[len(fixes)-1].At,
		Fixes:   len(fixes),
	}
	airborne := false
	var metres float64
	for i, fx := range fixes {
		if !fx.OnGround {
			airborne = true
		}
		if fx.AltFt > f.MaxAltFt {
			f.MaxAltFt = fx.AltFt
		}
		if i > 0 {
			metres += metresBetween(fixes[i-1].Lat, fixes[i-1].Lon, fx.Lat, fx.Lon)
		}
	}
	if !airborne {
		return Flight{}, false // never left the ground
	}
	f.DistanceNM = metres / 1852
	return f, true
}

// metresBetween is the great-circle distance between two points. Haversine
// rather than an equirectangular approximation: the cost is irrelevant at these
// volumes and it is correct everywhere, including across the antimeridian.
func metresBetween(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000.0
	rad := math.Pi / 180
	p1, p2 := lat1*rad, lat2*rad
	dp, dl := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * earthRadius * math.Asin(math.Min(1, math.Sqrt(a)))
}

// Stats reports what the archive holds, for the startup log.
func (s *Store) Stats() (fixes int64, oldest time.Time, err error) {
	var ms sql.NullInt64
	err = s.db.QueryRow(`SELECT count(*), min(at) FROM fix`).Scan(&fixes, &ms)
	if ms.Valid {
		oldest = time.UnixMilli(ms.Int64).UTC()
	}
	return fixes, oldest, err
}

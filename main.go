package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// userAgent identifies us to the providers. Both are free community services
// with no SLA and no API key, so being identifiable is the least we can do if
// our polling ever needs discussing.
const userAgent = "aircraft-tracker/0.1 (+https://blueskyflying.com.au)"

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	verbose := flag.Bool("v", false, "log every position fix, not just status changes")
	hashpw := flag.Bool("hashpw", false, "read a password from stdin and print its hash for config.json")
	insecure := flag.Bool("insecure", false, "run without authentication (password_hash empty)")
	casa := flag.String("casa", "", "check a fleet file against the CASA aircraft register and exit")
	flag.Parse()

	if *casa != "" {
		if err := checkFleetAgainstCASA(*casa); err != nil {
			log.Fatalf("casa: %v", err)
		}
		return
	}
	if *hashpw {
		if err := printPasswordHash(); err != nil {
			log.Fatalf("hashpw: %v", err)
		}
		return
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	// An unauthenticated deployment should not be one missing config key away.
	if cfg.PasswordHash == "" && !*insecure {
		log.Fatalf("config: password_hash is empty. Set one with:\n"+
			"    printf '%%s' 'your-password' | %s -hashpw\n"+
			"or pass -insecure to run without authentication deliberately.",
			os.Args[0])
	}
	if cfg.PasswordHash == "" {
		log.Print("WARNING: running without authentication (-insecure)")
	}

	log.Printf("tracking %d aircraft; broadcast every %s",
		len(cfg.Fleet), cfg.BroadcastInterval.Duration)
	for _, m := range cfg.Fleet {
		log.Printf("  %s  %s  %s", m.Hex, m.Rego, m.Desc)
	}
	for _, pr := range cfg.Providers {
		iv := pr.Interval.Duration
		if iv <= 0 {
			iv = cfg.PollInterval.Duration
		}
		log.Printf("  provider %-15s every %s", pr.Name, iv)
	}
	log.Printf("  idle after %s: every %s", cfg.IdleTimeout.Duration, cfg.IdleInterval.Duration)
	if q := cfg.QuietHours; q.IdleInterval.Duration > 0 {
		log.Printf("  idle during %s-%s: every %s", q.From, q.To, q.IdleInterval.Duration)
	}

	store, err := OpenStore(cfg.HistoryPath)
	if err != nil {
		log.Fatalf("history: %v", err)
	}
	defer store.Close()
	if n, oldest, err := store.Stats(); err != nil {
		log.Printf("history: %v", err)
	} else if n == 0 {
		log.Printf("  history %s (empty; recording starts now)", cfg.HistoryPath)
	} else {
		log.Printf("  history %s (%d fixes since %s)", cfg.HistoryPath, n, oldest.Format("2006-01-02"))
	}

	hub := NewHub()
	logStates := stateLogger(*verbose)

	p := NewPoller(cfg)
	p.OnUpdate = func(states []State) {
		hub.Broadcast(states)
		logStates(states)
		// Losing history is bad; taking the live display down with it would be
		// worse, so a write failure is reported and otherwise survived.
		if _, err := store.Record(states); err != nil {
			log.Printf("history: %v", err)
		}
	}

	web, err := newServer(cfg, hub, p, store)
	if err != nil {
		log.Fatalf("web: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately no WriteTimeout: SSE responses are long-lived and a
		// global deadline would sever every stream on a timer. The SSE handler
		// sets a per-write deadline instead.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Go(func() { p.Run(ctx) })
	wg.Go(func() {
		log.Printf("listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
			stop()
		}
	})

	<-ctx.Done()
	log.Print("shutting down")

	// Bounded: connected SSE clients will not close on their own, so waiting
	// for them indefinitely would hang every restart.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	wg.Wait()
}

// stateLogger reports fleet changes to the log. The SSE stream now carries the
// data, so by default this logs only status transitions -- the thing worth
// noticing overnight. -v restores per-fix logging for debugging.
func stateLogger(verbose bool) func([]State) {
	last := map[string]string{}
	return func(states []State) {
		for _, s := range states {
			key := string(s.Status)
			line := key
			if s.Fix != nil && s.Status != StatusNoContact {
				src := s.Fix.Source
				if s.Fix.MLAT {
					src += " mlat"
				}
				alt := "ground"
				if !s.Fix.OnGround {
					alt = fmt.Sprintf("%dft", s.Fix.AltFt)
				}
				line = fmt.Sprintf("%-5s %9.5f,%10.5f %8s %3.0fkt %03.0f° age=%4.1fs via %s",
					s.Status, s.Fix.Lat, s.Fix.Lon, alt,
					s.Fix.SpeedKt, s.Fix.TrackDeg, s.AgeSec, src)
				if verbose {
					key = line
				}
			}
			if last[s.Hex] == key {
				continue
			}
			last[s.Hex] = key
			log.Printf("%-7s %s", s.Rego, line)
		}
	}
}

// printPasswordHash reads a password from stdin and prints the encoded hash to
// paste into config.json.
//
// Stdin rather than a flag: an argument would land in shell history and in the
// process list. Reading it does not disable terminal echo -- doing so without a
// dependency means shelling out to stty, which is not worth it for a one-off
// local command. Pipe it instead:
//
//	printf '%s' 'your-password' | ./aircraft-tracker -hashpw
func printPasswordHash() error {
	in, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return err
	}
	pw := strings.TrimRight(string(in), "\r\n")
	if pw == "" {
		return fmt.Errorf("no password on stdin; try: printf '%%s' 'secret' | %s -hashpw", os.Args[0])
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", hash)
	fmt.Fprintln(os.Stderr, "\nAdd to config.json as:\n  \"password_hash\": \""+hash+"\"")
	return nil
}

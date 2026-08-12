package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// userAgent identifies us to the providers. Both are free community services
// with no SLA and no API key, so being identifiable is the least we can do if
// our polling ever needs discussing.
const userAgent = "aircraft-tracker/0.1 (+https://blueskyflying.com.au)"

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
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

	p := NewPoller(cfg)
	p.OnUpdate = logTick()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p.Run(ctx)
	log.Print("shutting down")
}

// logTick is a placeholder consumer until the SSE hub and history writer exist.
//
// It dedupes on position and status rather than on the rendered line, because
// the line carries a fix age that changes every tick -- keying on it would
// print once a second for an aircraft that has been sitting stale for an hour.
func logTick() func([]State) {
	last := map[string]string{}
	return func(states []State) {
		for _, s := range states {
			key, line := string(s.Status), string(s.Status)
			if s.Fix != nil && s.Status != StatusNoContact {
				src := s.Fix.Source
				if s.Fix.MLAT {
					src += " mlat"
				}
				alt := fmt.Sprintf("%dft", s.Fix.AltFt)
				if s.Fix.OnGround {
					alt = "ground"
				}
				key = fmt.Sprintf("%s %f %f %s", s.Status, s.Fix.Lat, s.Fix.Lon, src)
				line = fmt.Sprintf("%-5s %9.5f,%10.5f %8s %3.0fkt %03.0f° age=%4.1fs via %s",
					s.Status, s.Fix.Lat, s.Fix.Lon, alt, s.Fix.SpeedKt, s.Fix.TrackDeg, s.AgeSec, src)
			}
			if last[s.Hex] == key {
				continue
			}
			last[s.Hex] = key
			log.Printf("%-7s %s", s.Rego, line)
		}
	}
}

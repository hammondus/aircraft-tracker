package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Status is how much we trust an aircraft's last known position right now.
//
// "Not currently visible" has to be a first-class state rather than an absence.
// An aircraft the receiver network cannot hear is simply missing from the API
// response, and a map that silently omits it looks identical to a map showing
// it parked at its last known position. For an ops display that is actively
// misleading, so every fleet member is always reported, carrying its status.
type Status string

const (
	StatusLive      Status = "live"
	StatusStale     Status = "stale"
	StatusNoContact Status = "no_contact"
)

const (
	// liveFor bounds dead reckoning as well as the badge. Past this the client
	// stops extrapolating: an aircraft gliding across the map on a stale
	// velocity vector invents data, which is worse than one that visibly stops.
	liveFor = 15 * time.Second
	// staleFor is how long a last known position is still worth plotting.
	staleFor = 15 * time.Minute
	// requestTimeout is deliberately short. A provider slower than this is not
	// useful at our poll rate, and a long timeout would let requests overlap.
	requestTimeout = 5 * time.Second
	// maxBackoff caps the retry interval. These are free community services
	// with no SLA; when one is unwell we keep a slow heartbeat going rather
	// than hammering it or giving up on it entirely.
	maxBackoff = 2 * time.Minute
	// blockedBackoff applies to an outright access refusal (403/401) rather
	// than a rate limit. A block is cleared by a human, not by waiting, so we
	// check back rarely -- often enough to notice restored access, rarely
	// enough not to be part of the problem.
	blockedBackoff = 15 * time.Minute
)

// State is one fleet member as the UI should render it.
type State struct {
	Member
	Status Status  `json:"status"`
	AgeSec float64 `json:"age_sec,omitempty"`
	Fix    *Fix    `json:"fix,omitempty"` // last known, even when no_contact
}

// source is a provider plus the retry state of its own polling goroutine. All
// of this state is only ever touched by that goroutine, so it needs no lock.
type source struct {
	Provider
	interval      time.Duration // configured floor, used when the fleet is active
	fails         int           // consecutive failures, for logging and backoff
	warnedBlocked bool          // so an access block is reported once, not every retry
	// cooldown is an adaptive penalty added to interval. It persists across
	// recovery and decays only after sustained success.
	//
	// Without it a rate-limited provider oscillates: back off, recover, snap
	// straight back to the base interval, trip the limit again, forever. That
	// was observed against airplanes.live, which tolerated roughly one request
	// per 15-20s while its quota was depleted but was polled every 5s. The
	// cooldown lets each source settle at whatever rate the provider currently
	// tolerates, then speed back up when it relents -- which matters because
	// the sustainable rate is evidently not a fixed number.
	cooldown  time.Duration
	successes int
}

// cooldownDecayAfter is how many consecutive good responses are needed before
// easing the penalty. One good response after a rate limit is not evidence the
// limit has lifted -- that is exactly what the oscillation looked like.
const cooldownDecayAfter = 3

// pollMode is why we are polling at the rate we are, for logging.
type pollMode string

const (
	modeActive    pollMode = "active"
	modeIdle      pollMode = "idle"
	modeIdleQuiet pollMode = "idle, quiet hours"
)

// base is this source's own schedule: its configured interval plus any adaptive
// penalty, ignoring fleet activity and time of day. Poller.waitFor layers those
// on top.
func (s *source) base() time.Duration { return s.interval + s.cooldown }

// noteSuccess eases the penalty and reports how many failures preceded this
// success, so the caller can log a recovery.
func (s *source) noteSuccess() int {
	failed := s.fails
	s.fails = 0
	if s.cooldown > 0 {
		s.successes++
		if s.successes >= cooldownDecayAfter {
			s.successes = 0
			s.cooldown /= 2
			if s.cooldown < s.interval/2 {
				s.cooldown = 0 // close enough to base; stop creeping
			}
		}
	}
	return failed
}

type Poller struct {
	client       *http.Client
	members      []Member
	hexes        string // precomputed comma-separated list
	sources      []*source
	broadcast    time.Duration
	idleInterval time.Duration
	idleTimeout  time.Duration
	quiet        QuietHours

	// mode is the last announced polling mode, owned by the broadcast
	// goroutine so transitions are logged once rather than once per provider.
	mode pollMode

	mu     sync.RWMutex
	latest map[string]Fix

	// OnUpdate fires on the broadcast ticker with a fresh snapshot. It drives
	// the SSE fan-out and the history writer.
	OnUpdate func([]State)
}

func NewPoller(c *Config) *Poller {
	hx := make([]string, len(c.Fleet))
	for i, m := range c.Fleet {
		hx[i] = m.Hex
	}
	srcs := make([]*source, len(c.Providers))
	for i, p := range c.Providers {
		iv := p.Interval.Duration
		if iv <= 0 {
			iv = c.PollInterval.Duration
		}
		srcs[i] = &source{Provider: p, interval: iv}
	}
	return &Poller{
		client:  &http.Client{Timeout: requestTimeout},
		members: c.Fleet,
		// One request covers the whole fleet, and hex queries are not
		// geographically constrained -- verified with a single request that
		// returned aircraft near Sydney and Melbourne together. This is why
		// there is no viewport tracking or circle stitching anywhere here.
		hexes:        strings.Join(hx, ","),
		sources:      srcs,
		broadcast:    c.BroadcastInterval.Duration,
		idleInterval: c.IdleInterval.Duration,
		idleTimeout:  c.IdleTimeout.Duration,
		quiet:        c.QuietHours,
		latest:       make(map[string]Fix, len(c.Fleet)),
	}
}

// active reports whether any fleet member has been detected within idleTimeout.
//
// Being on the ground counts. An aircraft sitting with its transponder powered
// is a strong signal of an imminent departure, which is exactly when the fast
// rate earns its keep -- and the cost of being wrong is polling a free API well
// inside its documented limit.
func (p *Poller) active(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, f := range p.latest {
		if now.Sub(f.At) <= p.idleTimeout {
			return true
		}
	}
	return false
}

// modeAt reports the polling mode and its base interval.
//
// Time of day only matters while idle: an aircraft detected at 3am is tracked
// at the full rate like any other, because the reason to watch it is the same.
func (p *Poller) modeAt(now time.Time) (pollMode, time.Duration) {
	if p.active(now) {
		return modeActive, 0 // 0 means "the source's own interval"
	}
	if p.quiet.active(now) {
		return modeIdleQuiet, p.quiet.IdleInterval.Duration
	}
	return modeIdle, p.idleInterval
}

// waitFor is the interval before a source's next poll, including any adaptive
// penalty. Idle rates may only ever slow a source down, never speed it up.
func (p *Poller) waitFor(s *source, now time.Time) time.Duration {
	_, idle := p.modeAt(now)
	return max(s.interval, idle) + s.cooldown
}

// announceMode logs mode changes. Called only from the broadcast goroutine, so
// it needs no lock of its own and a transition is reported once rather than
// once per provider.
//
// Worth logging because several minutes of silence between polls is otherwise
// indistinguishable from a wedged poller at three in the morning.
func (p *Poller) announceMode(now time.Time) {
	mode, idle := p.modeAt(now)
	if mode == p.mode {
		return
	}
	first := p.mode == ""
	p.mode = mode
	switch {
	case first:
		log.Printf("poll: starting %s", mode)
	case mode == modeActive:
		log.Printf("poll: aircraft detected, resuming full rate")
	default:
		log.Printf("poll: nothing seen for %s, dropping to %s (every %s)",
			p.idleTimeout, mode, idle)
	}
}

// Run polls every provider on its own schedule and broadcasts on another.
//
// The two are deliberately decoupled. Upstream rate limits differ per provider
// and can force a slow poll; clients should still receive a steady, predictable
// stream. Separating them also means staggered providers compose: two sources
// at 5s offset by 2.5s refresh fleet state every 2.5s, while neither sees more
// than one request every five seconds.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i, s := range p.sources {
		offset := p.startOffset(i)
		wg.Go(func() { p.runSource(ctx, s, offset) })
	}
	wg.Go(func() { p.runBroadcast(ctx) })
	wg.Wait()
}

// startOffset spreads sources evenly across their interval so they interleave
// rather than firing together. This is what stops a conservative per-provider
// interval becoming conservative staleness: two providers at 5s, offset by
// 2.5s, refresh fleet state every 2.5s while neither exceeds 0.2 req/s.
func (p *Poller) startOffset(i int) time.Duration {
	if len(p.sources) < 2 {
		return 0
	}
	return time.Duration(i) * p.sources[i].interval / time.Duration(len(p.sources))
}

func (p *Poller) runSource(ctx context.Context, s *source, offset time.Duration) {
	if offset > 0 && !sleepCtx(ctx, offset) {
		return
	}
	for {
		wait := p.waitFor(s, time.Now())

		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		fixes, err := s.poll(reqCtx, p.client, p.hexes)
		cancel()

		switch {
		case err == nil:
			failed := s.noteSuccess()
			p.merge(fixes) // merge before recomputing: these fixes may end the idle period
			wait = p.waitFor(s, time.Now())
			if failed > 0 {
				log.Printf("poll: %s recovered after %d failures (now every %s)",
					s.Name, failed, wait.Round(time.Second))
			}
		case ctx.Err() != nil:
			return // shutting down; the error is just the cancelled request
		default:
			// Whichever is longer: the backoff, or the rate we would be polling
			// at anyway. A failing provider must not end up polled *faster*
			// than a healthy one just because the fleet went idle.
			wait = max(s.retryDelay(err), p.waitFor(s, time.Now()))
			// One provider failing is expected and survivable -- the point of
			// polling two is that either can carry the fleet alone.
			log.Printf("poll: %v (retry in %s)", err, wait.Round(time.Second))
			// A block is a different thing from a rate limit and needs a human,
			// so say so plainly -- once, not on every retry.
			var he *httpError
			if errors.As(err, &he) && he.Blocked() && !s.warnedBlocked {
				s.warnedBlocked = true
				log.Printf("poll: %s is REFUSING ACCESS, not rate limiting. "+
					"This is usually an IP block and will not clear by waiting. "+
					"Retrying every %s; resolve it with the provider.",
					s.Name, wait.Round(time.Minute))
			}
		}

		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// retryDelay records a failure and returns how long to wait before retrying.
//
// Growth is exponential via the cooldown, which doubles per consecutive
// failure, so a single mechanism serves both immediate backoff and the
// longer-lived penalty. Retry-After wins when the server asks for longer, but
// never shortens the wait: a provider that just refused us is not one to push
// harder.
func (s *source) retryDelay(err error) time.Duration {
	s.fails++
	s.successes = 0

	var he *httpError
	isHTTP := errors.As(err, &he)

	// An access block will not clear on a two-minute timer, and hammering a
	// provider that has just refused us is the surest way to stay refused.
	if isHTTP && he.Blocked() {
		s.cooldown = blockedBackoff
		return blockedBackoff
	}

	s.cooldown = min(max(2*s.cooldown, s.interval), maxBackoff)
	d := s.base()
	if isHTTP && he.RetryAfter > d {
		d = he.RetryAfter
	}
	return min(d, maxBackoff)
}

func (p *Poller) runBroadcast(ctx context.Context) {
	t := time.NewTicker(p.broadcast)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.announceMode(time.Now())
			// Ticker drops ticks rather than queueing them, so a slow consumer
			// costs an update but never builds a backlog.
			if p.OnUpdate != nil {
				p.OnUpdate(p.Snapshot())
			}
		}
	}
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// merge keeps the freshest fix per aircraft. Providers routinely disagree by a
// second or two, and either may return a fix older than one we already hold, so
// this compares against stored state rather than just across one response.
func (p *Poller) merge(fixes []Fix) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range fixes {
		if cur, ok := p.latest[f.Hex]; ok && !f.At.After(cur.At) {
			continue
		}
		p.latest[f.Hex] = f
	}
}

// Snapshot reports every fleet member, including those never seen.
func (p *Poller) Snapshot() []State { return p.snapshotAt(time.Now()) }

// snapshotAt computes status against an explicit clock. Status decays with wall
// time rather than being fixed at merge, so a snapshot taken between polls
// still ages correctly.
func (p *Poller) snapshotAt(now time.Time) []State {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]State, 0, len(p.members))
	for _, m := range p.members {
		s := State{Member: m, Status: StatusNoContact}
		if f, ok := p.latest[m.Hex]; ok {
			age := now.Sub(f.At)
			switch {
			case age <= liveFor:
				s.Status = StatusLive
			case age <= staleFor:
				s.Status = StatusStale
			}
			fix := f
			s.Fix = &fix
			s.AgeSec = age.Seconds()
		}
		out = append(out, s)
	}
	return out
}

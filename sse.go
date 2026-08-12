package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// sseWriteTimeout bounds a single event write. SSE responses live for hours, so
// the server must not carry a global WriteTimeout; a per-write deadline gives
// the same protection against a stalled client without killing healthy streams.
const sseWriteTimeout = 10 * time.Second

// sseRetry is the reconnect delay advertised to EventSource, in milliseconds.
// The browser default is 3s; 2s is a little friendlier for a live map without
// hammering us if we are restarting.
const sseRetry = 2000

// Hub fans fleet snapshots out to connected browsers over Server-Sent Events.
//
// SSE rather than WebSocket because the traffic is one-directional: the client
// never sends anything. EventSource reconnects automatically, it is plain HTTP
// so it passes through nginx proxy manager unmodified, and this file is the
// entire implementation.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	// last is the most recent payload, replayed to each new subscriber so a
	// client connecting between ticks sees the fleet immediately rather than
	// staring at an empty map for up to a broadcast interval.
	last []byte
}

func NewHub() *Hub {
	return &Hub{clients: make(map[chan []byte]struct{})}
}

// Broadcast encodes a snapshot and queues it for every connected client. It is
// the Poller's OnUpdate hook.
//
// Never blocks: a wedged client must not stall the broadcast goroutine, and
// through it every other viewer.
func (h *Hub) Broadcast(states []State) {
	payload, err := json.Marshal(states)
	if err != nil {
		// Can only happen if State grows an unmarshalable field, but silently
		// serving a frozen map would be a miserable way to discover that.
		log.Printf("sse: encode snapshot: %v", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = payload
	for ch := range h.clients {
		// Discard any frame this client has not collected yet, then queue the
		// current one. These are full-state snapshots, so a newer frame wholly
		// supersedes an older one -- queueing both would only deliver stale
		// positions late. Single producer under the lock, so after the drain
		// there is always room and this cannot block.
		select {
		case <-ch:
		default:
		}
		ch <- payload
	}
}

func (h *Hub) subscribe() (chan []byte, []byte) {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[ch] = struct{}{}
	return ch, h.last
}

// unsubscribe removes a client. The channel is deliberately not closed:
// Broadcast holds the same lock, so once this returns nothing can send on it,
// and closing a channel a producer might still hold is how panics happen.
func (h *Hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, ch)
}

// Clients reports the number of connected browsers.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	// Live data, never reusable. See DESIGN-DECISIONS.md §11.
	w.Header().Set("Cache-Control", "no-store")
	// Ask nginx not to buffer this response. Without it -- or proxy_buffering
	// off in the site config -- events accumulate in the proxy and arrive in
	// bursts, which looks exactly like a broken feed. Setting it here means the
	// deployment works without anyone remembering the nginx side.
	w.Header().Set("X-Accel-Buffering", "no")

	ch, last := h.subscribe()
	defer h.unsubscribe(ch)

	rc := http.NewResponseController(w)
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetry); err != nil {
		return
	}
	if last != nil && !writeEvent(w, rc, last) {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	log.Printf("sse: %s connected (%d clients)", r.RemoteAddr, h.Clients())
	defer func() { log.Printf("sse: %s disconnected", r.RemoteAddr) }()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-ch:
			if !writeEvent(w, rc, payload) {
				return
			}
		}
	}
}

// writeEvent emits one SSE frame, reporting false if the client has gone.
//
// No heartbeat is needed: the broadcast ticker fires whether or not anything
// changed, so the stream is never idle long enough for a proxy to time it out.
func writeEvent(w io.Writer, rc *http.ResponseController, data []byte) bool {
	// Not all ResponseWriters support deadlines; an unsupported deadline is not
	// a reason to drop the client.
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprintf(w, "event: fleet\ndata: %s\n\n", data); err != nil {
		return false
	}
	return rc.Flush() == nil
}

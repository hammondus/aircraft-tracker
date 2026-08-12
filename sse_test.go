package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testStates(rego string, status Status) []State {
	return []State{{
		Member: Member{Hex: "7c7c16", Rego: rego},
		Status: status,
		Fix:    &Fix{Hex: "7c7c16", Lat: -33.5, Lon: 151.2, AltFt: 24000, Source: "test"},
	}}
}

// readEvent reads one SSE frame, returning its event name and data.
func readEvent(t *testing.T, sc *bufio.Scanner) (event, data string) {
	t.Helper()
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && data != "":
			return event, data // blank line terminates the frame
		}
	}
	t.Fatalf("stream ended before a complete event: %v", sc.Err())
	return "", ""
}

func TestHubStreamsSnapshots(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	// Live data must never be cached, and nginx must not buffer it.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	// Wait for the handler to register before broadcasting, otherwise the test
	// races the subscription.
	waitForClients(t, h, 1)
	h.Broadcast(testStates("VH-YSO", StatusLive))

	sc := bufio.NewScanner(resp.Body)
	event, data := readEvent(t, sc)
	if event != "fleet" {
		t.Errorf("event = %q, want fleet", event)
	}
	var got []State
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, data)
	}
	if len(got) != 1 || got[0].Rego != "VH-YSO" || got[0].Status != StatusLive {
		t.Errorf("decoded %+v", got)
	}
	if got[0].Fix == nil || got[0].Fix.Lat != -33.5 {
		t.Errorf("fix did not survive the round trip: %+v", got[0].Fix)
	}
}

// A client connecting between ticks must see the fleet immediately rather than
// an empty map until the next broadcast.
func TestHubReplaysLastSnapshotOnConnect(t *testing.T) {
	h := NewHub()
	h.Broadcast(testStates("VH-TAV", StatusStale)) // before anyone connects

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	_, data := readEvent(t, bufio.NewScanner(resp.Body))
	var got []State
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Rego != "VH-TAV" || got[0].Status != StatusStale {
		t.Errorf("replayed snapshot = %+v", got)
	}
}

// A client that has not collected its previous frame must receive the newest
// state, not a queued stale one. These are full snapshots, so the latest wholly
// supersedes anything before it.
func TestHubDropsStaleFramesForSlowClients(t *testing.T) {
	h := NewHub()
	ch, _ := h.subscribe()
	defer h.unsubscribe(ch)

	for i := range 50 {
		status := StatusLive
		if i == 49 {
			status = StatusNoContact // the state we must end up seeing
		}
		h.Broadcast(testStates("VH-YSO", status))
	}

	if got := len(ch); got != 1 {
		t.Fatalf("queued %d frames, want exactly 1", got)
	}
	var got []State
	if err := json.Unmarshal(<-ch, &got); err != nil {
		t.Fatal(err)
	}
	if got[0].Status != StatusNoContact {
		t.Errorf("client received a stale frame: status = %s", got[0].Status)
	}
}

// A wedged client must not stall the broadcast goroutine and through it every
// other viewer.
func TestBroadcastNeverBlocks(t *testing.T) {
	h := NewHub()
	for range 10 {
		ch, _ := h.subscribe() // subscribers that never read
		defer h.unsubscribe(ch)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			h.Broadcast(testStates("VH-YSO", StatusLive))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast blocked on clients that never read")
	}
}

func TestHubTracksClientCount(t *testing.T) {
	h := NewHub()
	if got := h.Clients(); got != 0 {
		t.Errorf("new hub has %d clients", got)
	}
	a, _ := h.subscribe()
	b, _ := h.subscribe()
	if got := h.Clients(); got != 2 {
		t.Errorf("after 2 subscribes: %d", got)
	}
	h.unsubscribe(a)
	if got := h.Clients(); got != 1 {
		t.Errorf("after 1 unsubscribe: %d", got)
	}
	h.unsubscribe(b)
	if got := h.Clients(); got != 0 {
		t.Errorf("after both unsubscribed: %d", got)
	}
	// Unsubscribing twice must be harmless -- ServeHTTP defers it on paths that
	// may already have returned.
	h.unsubscribe(a)
	if got := h.Clients(); got != 0 {
		t.Errorf("double unsubscribe changed count to %d", got)
	}
}

// Broadcasting with nobody connected is the overnight case, and must be a no-op
// rather than an error or a leak.
func TestBroadcastWithNoClients(t *testing.T) {
	h := NewHub()
	for range 100 {
		h.Broadcast(testStates("VH-YSO", StatusNoContact))
	}
	if h.last == nil {
		t.Error("last snapshot not retained for the next client to connect")
	}
}

// The handler must return when the client disconnects, or every closed tab
// leaks a goroutine and a hub registration.
func TestHandlerReleasesClientOnDisconnect(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	waitForClients(t, h, 1)
	resp.Body.Close()
	waitForClients(t, h, 0)
}

func waitForClients(t *testing.T, h *Hub, want int) {
	t.Helper()
	for range 200 {
		if h.Clients() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client count settled at %d, want %d", h.Clients(), want)
}

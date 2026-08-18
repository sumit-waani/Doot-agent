package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/events"
)

const (
	// keepaliveInterval stops idle-connection reaping by mobile carrier proxies
	// and Fly's edge.
	keepaliveInterval = 15 * time.Second

	// replayLimit bounds a single reconnect's catch-up.
	replayLimit = 2000
)

// handleSSE streams live updates.
//
// The stream is resumable: every event id is the events table primary key, so a
// client that reconnects with Last-Event-ID gets exactly what it missed. This is
// the normal case on a phone, not an edge case — locking the screen kills the
// connection.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request, _ auth.User) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Without this, proxies buffer the stream and the UI appears frozen.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()

	// Subscribe before replaying, so an event published during replay is queued
	// rather than lost in the gap between the two.
	live, unsubscribe := s.events.Broker().Subscribe()
	defer unsubscribe()

	lastID := lastEventID(r)

	// Tell the client to reload if the gap it asks for has been pruned;
	// replaying an incomplete range would silently drop events.
	if lastID > 0 {
		canReplay, err := s.events.CanReplay(ctx, lastID)
		if err != nil {
			slog.Error("could not check replay window", "err", err)
		} else if !canReplay {
			writeEvent(w, flusher, events.Event{
				ID:      0,
				Type:    events.TypeReload,
				Payload: `{"reason":"event log pruned past requested id"}`,
			})
			return
		}
	}

	replayed, err := s.events.Since(ctx, lastID, replayLimit)
	if err != nil {
		slog.Error("could not replay events", "err", err)
	}
	for _, ev := range replayed {
		writeEvent(w, flusher, ev)
		lastID = ev.ID
	}

	// Retry hint for the browser's automatic reconnect.
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, open := <-live:
			if !open {
				return
			}
			// Skip anything already delivered during replay.
			if ev.ID <= lastID {
				continue
			}
			writeEvent(w, flusher, ev)
			lastID = ev.ID

		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeEvent emits one SSE frame.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, ev events.Event) {
	if ev.ID > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.ID)
	}
	fmt.Fprintf(w, "event: %s\n", ev.Type)

	// A payload containing newlines must be split across data: lines, or
	// everything after the first newline is silently dropped.
	for _, line := range strings.Split(ev.Payload, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")

	flusher.Flush()
}

// lastEventID reads the resume point, preferring the standard header and
// falling back to a query parameter for clients that cannot set headers.
func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

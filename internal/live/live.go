// Package live turns PostgreSQL NOTIFY messages into Server-Sent Events.
// The database triggers call pg_notify('relief_events', ...) on every change,
// so the dashboard updates even when a change comes from psql or pgAdmin.
package live

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Channel is the NOTIFY channel the triggers publish on.
const Channel = "relief_events"

// Hub fans notifications out to connected browsers.
type Hub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
	log     *slog.Logger
}

func NewHub(log *slog.Logger) *Hub { return &Hub{clients: map[chan string]struct{}{}, log: log} }

// Listen holds a dedicated connection that LISTENs and broadcasts until ctx ends.
// pq.Listener reconnects automatically after network failures.
func (h *Hub) Listen(ctx context.Context, dsn string) error {
	l := pq.NewListener(dsn, time.Second, time.Minute, func(ev pq.ListenerEventType, err error) {
		switch ev {
		case pq.ListenerEventDisconnected:
			h.log.Warn("listener disconnected", "err", err)
		case pq.ListenerEventReconnected:
			h.log.Info("listener reconnected")
			h.Broadcast(`{"table":"_reconnected"}`) // clients should refetch everything
		}
	})
	if err := l.Listen(Channel); err != nil {
		return fmt.Errorf("listen %s: %w", Channel, err)
	}
	go func() {
		defer l.Close()
		ping := time.NewTicker(90 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-l.Notify:
				if n != nil {
					h.Broadcast(n.Extra)
				}
			case <-ping.C:
				_ = l.Ping()
			}
		}
	}()
	return nil
}

// Broadcast sends a payload to every client without blocking on slow ones.
func (h *Hub) Broadcast(payload string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- payload:
		default:
		}
	}
}

// ServeHTTP streams events to one browser.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan string, 128)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case p := <-c:
			fmt.Fprintf(w, "data: %s\n\n", p)
			fl.Flush()
		}
	}
}

package main

import (
	"fmt"
	"net/http"
	"sync"
)

// hub pushes state to browsers over Server-Sent Events. SSE rather than a
// websocket is a decision, not a shortcut: the dashboard is strictly one-way,
// EventSource reconnects natively (with the retry interval the server names),
// it is plain HTTP through every proxy an ops network has, and it costs zero
// dependencies — a websocket buys back none of that for this shape of traffic.
//
// A slow browser is cut, never buffered without bound: each client has a small
// channel, and a client that cannot drain it is dropped and reconnects fresh.
// The same shed-not-block rule the venue applies to its subscribers applies to
// the dashboard's own.
type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{clients: map[chan []byte]struct{}{}}
}

const clientBuffer = 8

func (h *hub) broadcast(payload []byte) {
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- payload:
		default:
			delete(h.clients, ch)
			close(ch)
		}
	}
	h.mu.Unlock()
}

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, clientBuffer)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// ServeHTTP is the /events endpoint: one SSE stream per browser tab.
func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()

	ch := h.subscribe()
	defer h.unsubscribe(ch)
	for {
		select {
		case payload, open := <-ch:
			if !open {
				return // shed: the browser reconnects and starts fresh
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

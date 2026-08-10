package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Hub fans captured calls out to the live view.
//
// It applies the same rule as the recorder that feeds it: a slow consumer costs
// itself events, never the pipeline. A browser tab that stops reading must not
// be able to stall persistence, so its buffer fills and its events are dropped.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// clientBuffer is generous enough to absorb a burst while a tab repaints, and
// small enough that a tab left behind gives up quickly instead of hoarding.
const clientBuffer = 256

func NewHub() *Hub {
	return &Hub{clients: map[chan []byte]struct{}{}}
}

// Publish encodes one call and hands it to every current subscriber.
//
// The summary comes from Summary rather than being written out by hand here.
// The hand-written copy this replaced had already fallen behind by four fields,
// and a live view that reports less than a reload does is the failure mode the
// live view exists to avoid.
func (h *Hub) Publish(c *store.Call) {
	payload, err := json.Marshal(toSummary(c.Summary()))
	if err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		select {
		case client <- payload:
		default:
		}
	}
}

func (h *Hub) subscribe() chan []byte {
	client := make(chan []byte, clientBuffer)
	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()
	return client
}

func (h *Hub) unsubscribe(client chan []byte) {
	h.mu.Lock()
	delete(h.clients, client)
	h.mu.Unlock()
}

// stream is the live feed. Server-sent events rather than WebSockets: the
// traffic is one-directional, and SSE reconnects on its own.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported here")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell the browser not to reconnect faster than this if the stream drops.
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	client := s.hub.subscribe()
	defer s.hub.unsubscribe(client)

	// A heartbeat keeps intermediaries and the browser from treating an idle
	// connection as dead. An idle stack is the normal state of this tool.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-client:
			if _, err := fmt.Fprintf(w, "event: call\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

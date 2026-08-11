// Package toy holds the three things every service in the test bed needs: a
// JSON reply, a switch that makes it misbehave on purpose, and a way to pass
// the caller's request id on to the next hop.
//
// It is deliberately thin. The test bed exists so that a reader can look at a
// capture in Sonda and know, without opening anything, what the service was
// doing — which stops being true the moment these services get interesting.
package toy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// JSON writes v as the whole response.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "error", err)
	}
}

// Fail writes an error body under a status. The shape is the same everywhere so
// that a diff between a working call and a broken one is about the failure and
// not about two services disagreeing on how to word one.
func Fail(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]any{"error": msg, "status": status})
}

// Flags are the failure modes a reader turns on and off.
//
// They are switches rather than probabilities on purpose, for the same reason
// Sonda's own fault injection counts calls instead of rolling dice: an exercise
// that says "now break it and look again" has to give the same answer twice.
// Failures that are selected by the request itself — a restricted SKU, a
// customer that does not exist — need no switch at all and are always in
// flight, which is what puts healthy and broken traffic side by side.
type Flags struct {
	mu sync.RWMutex
	v  map[string]bool
}

// NewFlags declares the switches a service has. Naming them up front means
// GET /_control answers with the full set, so a reader can see what is
// available without reading the source.
func NewFlags(names ...string) *Flags {
	f := &Flags{v: make(map[string]bool, len(names))}
	for _, n := range names {
		f.v[n] = false
	}
	return f
}

// On reports whether a switch is thrown.
func (f *Flags) On(name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.v[name]
}

// Handle registers GET and POST /_control on the mux.
//
// This endpoint is meant to be called on the service's own published port,
// straight past Sonda: throwing a switch is stage management, and a capture of
// it in the middle of the field is noise in the exercise it was setting up.
func (f *Flags) Handle(mux *http.ServeMux) {
	mux.HandleFunc("GET /_control", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.RLock()
		defer f.mu.RUnlock()
		JSON(w, http.StatusOK, f.v)
	})

	mux.HandleFunc("POST /_control", func(w http.ResponseWriter, r *http.Request) {
		var want map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&want); err != nil {
			Fail(w, http.StatusBadRequest, "body must be a JSON object of switch names to booleans")
			return
		}
		f.mu.Lock()
		for k, v := range want {
			if _, known := f.v[k]; !known {
				f.mu.Unlock()
				Fail(w, http.StatusBadRequest, "no such switch: "+k)
				return
			}
			f.v[k] = v
		}
		out := make(map[string]bool, len(f.v))
		for k, v := range f.v {
			out[k] = v
		}
		f.mu.Unlock()
		slog.Info("control", "flags", out)
		JSON(w, http.StatusOK, out)
	})
}

// TraceHeaders are the ones Sonda reads to group calls into a single request
// with certainty. Listed here because a gateway that forwards them is what
// makes the difference between a tree Sonda knows and a tree it inferred.
var TraceHeaders = []string{"Traceparent", "X-Request-Id", "X-Correlation-Id"}

// Propagate copies the caller's request id onto an outgoing call.
//
// It only ever forwards an id that arrived; it never invents one. That is what
// a real gateway does, and it is also what lets the test bed show both kinds of
// tree: with an id Sonda groups by the id and says the grouping is certain,
// without one it groups by containment and says it guessed.
func Propagate(from, to http.Header) {
	for _, h := range TraceHeaders {
		if v := from.Get(h); v != "" {
			to.Set(h, v)
		}
	}
}

// Listen starts a server and blocks. Every service in the test bed is a
// foreground process in its own container, so there is nothing to shut down
// gracefully that the container runtime does not already handle.
func Listen(addr string, mux *http.ServeMux) {
	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server stopped", "error", err)
	}
}

// Package stub lets Sonda answer for a service instead of forwarding to it.
//
// The bytes of every response it ever gave are already stored, so handing them
// back is nearly free — and it changes what the tool is for. With one service
// stubbed you can work on the front while its backend is down; with all of
// them stubbed a test runs without twenty-one processes; and a bug that only
// happens with production data can be reproduced from a capture, on a laptop.
//
// The danger is precise and worth stating: a recorded answer that passes for a
// live one. Everything here exists to make that impossible to do by accident —
// stubbing is off unless it was turned on in this run of Sonda, it is never
// remembered across a restart, every stubbed answer carries a header saying so,
// and the capture it produces is linked back to the recording it came from.
package stub

import (
	"context"
	"sort"
	"sync"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Registry remembers which services are answering from capture.
//
// Deliberately in memory, and deliberately not written to the database. A stub
// that survives a restart is one nobody remembers turning on, and "why is
// ms-rates returning last Tuesday's prices" is a bad afternoon.
type Registry struct {
	db *store.Store

	mu sync.RWMutex
	on map[string]bool
}

func New(db *store.Store) *Registry {
	return &Registry{db: db, on: map[string]bool{}}
}

func (r *Registry) Set(target string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.on[target] = true
		return
	}
	delete(r.on, target)
}

// On reports whether this service should be answered from capture. A nil
// registry answers no, so a proxy built without one never stubs.
func (r *Registry) On(target string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.on[target]
}

// Active lists what is being stubbed, sorted, so the interface and the agent
// both get a stable answer.
func (r *Registry) Active() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.on))
	for target := range r.on {
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

// Clear turns everything back to live traffic. The one button that undoes a
// state you may have forgotten you were in.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = map[string]bool{}
}

// Match finds the recorded answer to give, or nil when there is none. Nil is
// not an error: it means this request was never made while Sonda was watching,
// and inventing something would be worse than saying so.
func (r *Registry) Match(ctx context.Context, target, method, path string, body []byte) (*store.Call, error) {
	if r == nil {
		return nil, nil
	}
	return r.db.MatchForStub(ctx, target, method, path, body)
}

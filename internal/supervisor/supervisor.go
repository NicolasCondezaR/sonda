// Package supervisor owns the listeners.
//
// Switching projects means closing one set of ports and opening another while
// the process keeps running, so listeners cannot be started once at boot and
// forgotten. Everything here exists to make that safe: a port that fails to
// open must not take the others with it, a port that closes must actually be
// free afterwards, and the reported state must be what is really listening
// rather than what was asked for.
package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Desired is one listener the supervisor should be running.
type Desired struct {
	// Key identifies the listener across reconciles. Two entries with the same
	// key are the same listener, so a rename does not restart a port and a port
	// change does.
	Key     string
	Listen  string
	Handler http.Handler
}

// Status is what is actually happening on a port, which is not always what was
// asked for — a port already taken by something else is the common case.
type Status struct {
	Key     string `json:"key"`
	Listen  string `json:"listen"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

type listener struct {
	desired Desired
	server  *http.Server
	err     error
}

type Supervisor struct {
	mu      sync.Mutex
	running map[string]*listener
}

func New() *Supervisor {
	return &Supervisor{running: map[string]*listener{}}
}

// Apply reconciles what is running against what is wanted.
//
// It never returns an error for a port that could not be opened: one service
// with a busy port must not stop the other fourteen from being observed. The
// failure is reported per listener instead, so the interface can say which one
// and why.
func (s *Supervisor) Apply(desired []Desired) []Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	wanted := make(map[string]Desired, len(desired))
	for _, d := range desired {
		wanted[d.Key] = d
	}

	// Stop what is gone or has moved. A changed address is a stop and a start,
	// not an update: a listening socket cannot be rebound.
	for key, l := range s.running {
		want, keep := wanted[key]
		if keep && want.Listen == l.desired.Listen && l.err == nil {
			l.desired = want // handler may have been rebuilt
			continue
		}
		s.stop(key, l)
	}

	for key, want := range wanted {
		if _, already := s.running[key]; already {
			continue
		}
		s.start(key, want)
	}

	return s.statusLocked()
}

// StopAll closes every listener. Used when a project is deactivated and on
// shutdown.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, l := range s.running {
		s.stop(key, l)
	}
}

func (s *Supervisor) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Supervisor) statusLocked() []Status {
	out := make([]Status, 0, len(s.running))
	for key, l := range s.running {
		st := Status{Key: key, Listen: l.desired.Listen, Running: l.err == nil}
		if l.err != nil {
			st.Error = l.err.Error()
		}
		out = append(out, st)
	}
	return out
}

func (s *Supervisor) start(key string, d Desired) {
	// net.Listen rather than ListenAndServe: binding has to fail here, where
	// the error can be attached to this listener, instead of on a goroutine
	// nobody is reading.
	ln, err := net.Listen("tcp", d.Listen)
	if err != nil {
		s.running[key] = &listener{desired: d, err: err}
		slog.Warn("could not open port", "service", key, "listen", d.Listen, "error", err)
		return
	}

	server := &http.Server{
		Handler: d.Handler,
		// No read or write timeout: a debugging proxy must not be the thing
		// that kills a slow call the developer is trying to observe.
		ReadHeaderTimeout: 30 * time.Second,
	}
	entry := &listener{desired: d, server: server}
	s.running[key] = entry

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.mu.Lock()
			if current, ok := s.running[key]; ok && current == entry {
				current.err = err
			}
			s.mu.Unlock()
			slog.Warn("listener stopped", "service", key, "error", err)
		}
	}()

	slog.Info("listening", "service", key, "listen", d.Listen)
}

// stop must leave the port genuinely free: the next Apply may rebind it
// immediately, and a Shutdown that returns before the socket is released turns
// a project switch into an "address already in use".
func (s *Supervisor) stop(key string, l *listener) {
	delete(s.running, key)
	if l.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.server.Shutdown(ctx); err != nil {
		// A connection that will not drain — a live gRPC stream, say — must not
		// hold the port hostage while the developer waits to switch projects.
		_ = l.server.Close()
	}
	slog.Info("closed", "service", key, "listen", l.desired.Listen)
}

// Probe reports whether an address can be bound right now. The interface uses
// it to tell a user that a port is taken before they save, rather than after.
func Probe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s is not available: %w", addr, err)
	}
	return ln.Close()
}

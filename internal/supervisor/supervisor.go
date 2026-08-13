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
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
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

	// Serve takes each accepted connection instead of an HTTP server, for a
	// target that never speaks HTTP at all — Postgres and AMQP are framed from
	// their first byte, so there is no request for a handler to be given.
	//
	// It is a field on the same struct rather than a second kind of listener
	// because everything that makes this package worth having — a port that
	// fails to open must not take the others with it, a port that closes must
	// really be free, the reported state must be what is running — is identical
	// either way. Only the four lines between Accept and the goroutine differ.
	Serve func(net.Conn)

	// TLS makes the listener terminate encryption before the handler or raw
	// Serve function sees anything. It is not a third kind of listener: either
	// listener wears a tls.Config and receives the decrypted connection.
	//
	// It does change what sameKind means, though. The config is baked into the
	// socket at tls.NewListener time and there is no way to put it on or take it
	// off a listener that is already accepting, so a service switched between
	// http and https has to be restarted rather than swapped — otherwise the
	// port would keep answering in the clear while every interface reported it
	// as encrypted, which is the tool lying about itself.
	TLS *tls.Config
}

// Status is what is actually happening on a port, which is not always what was
// asked for — a port already taken by something else is the common case.
type Status struct {
	Key     string `json:"key"`
	Listen  string `json:"listen"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`

	// Connections counts every TCP connection this port has accepted since it
	// opened, whether or not any of them ever became a capture.
	//
	// It is the only evidence Sonda has that a client found the port at all. A
	// client speaking TLS to a plaintext listener, or a protocol this port does
	// not speak, connects and is never recorded — and without this counter that
	// reads exactly like a client that was never pointed here, which is a
	// different problem with a different fix. Counting at accept is what makes
	// the two separable; nothing further up the stack sees the connection that
	// failed before a request existed.
	Connections int64 `json:"connections"`
}

type listener struct {
	desired Desired
	server  *http.Server

	// raw is set instead of server for a Serve listener, and closing it is what
	// frees the port.
	raw net.Listener
	err error

	// handler and serve are what the running listener actually dereferences,
	// one load per request or per connection. An http.Server cannot have its
	// Handler replaced once it is serving, so without this indirection editing
	// a service's upstream would leave the old target wired to the port while
	// every interface reported the new one — the tool lying about itself, which
	// is the one thing this package exists to prevent.
	handler atomic.Pointer[http.Handler]
	serve   atomic.Pointer[func(net.Conn)]

	// accepted survives a swap and dies with a restart, which is the honest
	// reading: it counts what this socket has seen, and a socket that was
	// rebound is a new one.
	accepted atomic.Int64
}

// ServeHTTP is the stable handler the http.Server holds.
func (l *listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Take the handler and let go: a swap must never wait for a request to
	// finish, nor a request for a swap.
	h := l.handler.Load()
	if h == nil || *h == nil {
		http.Error(w, "no handler", http.StatusServiceUnavailable)
		return
	}
	(*h).ServeHTTP(w, r)
}

// serveConn is the stable function the accept loop holds. The count is taken
// here rather than inside the target's own handler because a raw target may
// hang up on the first byte, and a connection that arrived is a connection that
// arrived.
func (l *listener) serveConn(c net.Conn) {
	l.accepted.Add(1)
	(*l.serve.Load())(c)
}

// swap points a running listener at a rebuilt target.
func (l *listener) swap(d Desired) {
	if d.Serve != nil {
		l.serve.Store(&d.Serve)
		return
	}
	l.handler.Store(&d.Handler)
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
		if keep && want.Listen == l.desired.Listen && l.err == nil && sameKind(want, l.desired) {
			l.desired = want
			// The target is rebuilt on every reconcile, so keeping the port
			// open only stays honest if the new one is put to work.
			l.swap(want)
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

// sameKind reports whether two Desired want the same kind of listener. A
// service switched between HTTP and raw keeps its key and may keep its port,
// but an http.Server cannot start speaking framed bytes: that one is a restart,
// not a swap. Turning TLS on or off is the same case for the same reason — the
// handshake belongs to the socket, not to the handler.
func sameKind(a, b Desired) bool {
	return (a.Serve != nil) == (b.Serve != nil) && (a.TLS != nil) == (b.TLS != nil)
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
		st := Status{
			Key: key, Listen: l.desired.Listen, Running: l.err == nil,
			Connections: l.accepted.Load(),
		}
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

	if d.TLS != nil {
		// Wrapped after the bind, so a port already taken still fails above with
		// an error attached to this listener rather than at handshake time on a
		// goroutine nobody is reading. Raw protocols such as AMQP use the same
		// listener wrapper; their Serve function receives the decrypted stream.
		ln = tls.NewListener(ln, d.TLS)
	}

	if d.Serve != nil {
		entry := &listener{desired: d, raw: ln}
		entry.swap(d)
		s.running[key] = entry
		go accept(ln, entry.serveConn)
		slog.Info("listening", "service", key, "listen", d.Listen)
		return
	}

	entry := &listener{desired: d}
	entry.swap(d)
	server := &http.Server{
		Handler: entry,
		// No read or write timeout: a debugging proxy must not be the thing
		// that kills a slow call the developer is trying to observe.
		ReadHeaderTimeout: 30 * time.Second,

		// StateNew fires once per accepted connection, before the TLS handshake
		// and before the first request line is parsed. That is deliberately
		// earlier than the handler: the connections worth counting for a
		// diagnosis are precisely the ones that never reach a handler.
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				entry.accepted.Add(1)
			}
		},
	}
	entry.server = server
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
// accept hands every connection to the target's own handler. It ends when the
// listener is closed, which is the only way out and is not an error.
func accept(ln net.Listener, serve func(net.Conn)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serve(conn)
	}
}

func (s *Supervisor) stop(key string, l *listener) {
	delete(s.running, key)

	if l.raw != nil {
		// Closing the socket frees the port immediately, which is all this has
		// to guarantee. Sessions already accepted are left to finish: killing a
		// developer's live psql session because they switched project is worse
		// than recording it under the project it started in.
		_ = l.raw.Close()
		slog.Info("closed", "service", key, "listen", l.desired.Listen)
		return
	}
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

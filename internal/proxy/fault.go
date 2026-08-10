package proxy

import (
	"net/http"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/fault"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/trace"
)

// Faults is the part of the fault registry a proxy needs. Nil never injects
// anything, which is what every proxy does until told otherwise.
type Faults interface {
	Next(target string) fault.Action
}

// FaultHeader marks an answer Sonda broke on purpose. Same role as the stub
// header: a client that cannot tell an injected failure from a real one would
// chase the wrong bug, and this is the one place the distinction reaches code
// that is not looking at Sonda's interface.
const FaultHeader = "X-Sonda-Fault"

// injectFault applies whatever rule is in force before the call is forwarded.
// It reports whether it answered the request itself.
//
// A delay is applied and then the call carries on, because a slow service is
// still a service answering — that is the case a timeout is meant to catch. A
// status or a cut ends the call here: the service is never reached.
func (p *Proxy) injectFault(w http.ResponseWriter, r *http.Request, started time.Time) bool {
	if p.faults == nil {
		return false
	}
	action := p.faults.Next(p.target.Name)
	if !action.Injects() {
		return false
	}

	if action.Delay > 0 {
		select {
		case <-time.After(action.Delay):
		case <-r.Context().Done():
			// The client gave up while being held. Recording it is the point:
			// this is the timeout the rule was written to provoke.
			p.recordFault(r, started, 0, action, "the client gave up during the injected delay")
			return true
		}
	}

	switch {
	case action.Cut:
		// No answer at all — the failure a well-written client handles
		// differently from a 500, and the one almost nobody tests.
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				conn.Close()
			}
		}
		p.recordFault(r, started, 0, action, action.Reason())
		return true

	case action.Status > 0:
		w.Header().Set(FaultHeader, action.Reason())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(action.Status)
		_, _ = w.Write([]byte(action.Reason() + "\n"))
		p.recordFault(r, started, action.Status, action, action.Reason())
		return true
	}

	// Only a delay: the service still gets the call.
	return false
}

// recordFault stores an injected failure as an injected failure. A fault the
// field shows as the service's own would send someone hunting a bug that does
// not exist.
func (p *Proxy) recordFault(r *http.Request, started time.Time, status int, action fault.Action, reason string) {
	call := &store.Call{
		Target:     p.target.Name,
		Protocol:   config.ProtocolHTTP,
		Method:     r.Method,
		Path:       r.URL.RequestURI(),
		Status:     status,
		ClientAddr: r.RemoteAddr,
		StartedAt:  started,
		Duration:   time.Since(started),
		Error:      reason,
		TraceID:    trace.ID(r.Header),
		Request:    store.Message{Headers: r.Header.Clone()},
		Injected:   true,
	}
	// Not forwarded: the service was never reached, so there is no upstream
	// connection to report as verified or otherwise.
	p.markTLS(call, r, false)
	if isGRPCRequest(r.Header) {
		call.Protocol = config.ProtocolGRPC
	}
	p.recorder.Record(call)
}

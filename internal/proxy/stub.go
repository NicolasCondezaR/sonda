package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Stubs is the part of the stub registry a proxy needs. Nil means this proxy
// always forwards, which is what every proxy does until someone says otherwise.
type Stubs interface {
	On(target string) bool
	Match(ctx context.Context, target, method, path string, body []byte) (*store.Call, error)
}

// StubHeader goes on every answer that came from a recording rather than from
// the service. A client that cannot tell the two apart is the failure mode this
// whole feature has to avoid, and a header is the one place the distinction
// reaches code that is not looking at Sonda's interface.
const StubHeader = "X-Sonda-Stub"

// serveFromCapture answers without touching the upstream. It reports whether it
// handled the request; false means carry on and forward.
func (p *Proxy) serveFromCapture(w http.ResponseWriter, r *http.Request, started time.Time) bool {
	if p.stubs == nil || !p.stubs.On(p.target.Name) {
		return false
	}

	// The body has to be read to match on it, and it still has to be recorded,
	// so it is read once here and handed to both.
	body, err := io.ReadAll(io.LimitReader(r.Body, p.maxBody))
	if err != nil {
		body = nil
	}
	r.Body.Close()

	recorded, err := p.stubs.Match(r.Context(), p.target.Name, r.Method, r.URL.RequestURI(), body)
	if err != nil || recorded == nil {
		p.explainNoRecording(w, r, body, started, err)
		return true
	}

	writeRecorded(w, recorded)
	p.recordStub(r, body, recorded, recorded.Status, started)
	return true
}

// writeRecorded plays a stored response back onto the wire.
func writeRecorded(w http.ResponseWriter, c *store.Call) {
	for name, values := range c.Response.Headers {
		// Content-Length is recomputed by the server from what is actually
		// written; copying the recorded one would contradict the body whenever
		// the capture was truncated.
		if strings.EqualFold(name, "Content-Length") {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set(StubHeader, strconv.FormatInt(c.ID, 10))

	// gRPC carries the real outcome in trailers, after the body. Without them
	// a client sees a well-formed message and then waits for a status that
	// never comes. TrailerPrefix is how they are sent when the values are only
	// known as the response is written.
	for name, values := range c.ResponseTrailers {
		for _, v := range values {
			w.Header().Add(http.TrailerPrefix+name, v)
		}
	}

	w.WriteHeader(c.Status)
	_, _ = w.Write(c.Response.Body)
}

// explainNoRecording answers when stubbing is on and there is nothing to say.
//
// Making something up would defeat the point, and a silent empty 200 would be
// read as the service answering. 501 with a sentence is the honest reply.
func (p *Proxy) explainNoRecording(w http.ResponseWriter, r *http.Request, body []byte, started time.Time, cause error) {
	message := fmt.Sprintf(
		"Sonda is answering for %q from recordings, and it has no recording of %s %s. "+
			"Make the call once with stubbing off, or turn it off for this service.",
		p.target.Name, r.Method, r.URL.RequestURI())
	if cause != nil {
		message = "Sonda could not look for a recording: " + cause.Error()
	}

	w.Header().Set(StubHeader, "none")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = io.WriteString(w, message+"\n")

	p.recordStub(r, body, nil, http.StatusNotImplemented, started)
}

// recordStub stores the exchange like any other, linked to the recording it
// came from. A stub that leaves no trace would make the field lie about what
// the system did.
func (p *Proxy) recordStub(r *http.Request, body []byte, from *store.Call, status int, started time.Time) {
	call := &store.Call{
		Target:     p.target.Name,
		Protocol:   config.ProtocolHTTP,
		Method:     r.Method,
		Path:       r.URL.RequestURI(),
		Status:     status,
		ClientAddr: r.RemoteAddr,
		StartedAt:  started,
		Duration:   time.Since(started),
		Request: store.Message{
			Headers: r.Header.Clone(),
			Body:    body,
			Size:    int64(len(body)),
		},
	}
	if isGRPCRequest(r.Header) {
		call.Protocol = config.ProtocolGRPC
	}

	if from == nil {
		call.Error = "no recording to answer with"
	} else {
		id := from.ID
		call.StubOf = &id
		call.Response = store.Message{
			Headers: from.Response.Headers,
			Body:    from.Response.Body,
			Size:    int64(len(from.Response.Body)),
		}
		call.ResponseTrailers = from.ResponseTrailers
		call.GRPCStatus, call.GRPCMessage = from.GRPCStatus, from.GRPCMessage
	}
	p.recorder.Record(call)
}

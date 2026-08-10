package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/trace"
)

// A WebSocket cannot go through the reverse proxy the rest of the traffic uses.
// Once the handshake succeeds the connection stops being requests and responses
// and becomes two streams of frames, and Go's ReverseProxy relays those by
// copying bytes it never shows anyone. Sonda has to hold both directions itself
// to see them at all.
//
// What is stored is the raw frame stream per direction, exactly as it crossed,
// and the frames are read back out when someone looks — the same arrangement
// gRPC streaming already uses, for the same reason: decoding on the way in
// would discard whatever the parser did not understand.

// isWebSocketUpgrade reports whether this request is asking to become a socket.
//
// Connection is a comma-separated list and its value is case-insensitive, which
// is exactly the sort of thing a strict equality check gets wrong against a
// real client.
func isWebSocketUpgrade(h http.Header) bool {
	if !strings.EqualFold(strings.TrimSpace(h.Get("Upgrade")), "websocket") {
		return false
	}
	for _, token := range strings.Split(h.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// tap copies one direction and keeps the head of what went past.
//
// Bounded on purpose: a socket can stay open for hours, and a debugger that
// grows without limit while observing is a worse problem than a partial record.
// The full byte count is kept, so the capture says how much it did not keep.
type tap struct {
	limit int64

	// blank rewrites the credential-bearing parts of the stream before anything
	// is kept, and is nil for every protocol that has none. It runs here rather
	// than at display time because the capture is a plaintext file an agent can
	// read: what is never written cannot leak.
	//
	// It is a state machine over the whole stream, so it must see every chunk
	// even after the cap is reached, or it loses track of where it is.
	blank func([]byte) []byte

	mu    sync.Mutex
	head  []byte
	total int64
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Counted before anything else, and reported whatever happens below: the
	// count is of what crossed the wire, and a tap that reported a short write
	// would make io.MultiWriter abort the relay it is observing.
	n := len(p)
	t.total += int64(n)

	if t.blank != nil {
		p = t.blank(p)
	}
	if room := t.limit - int64(len(t.head)); room > 0 {
		if int64(len(p)) > room {
			p = p[:room]
		}
		t.head = append(t.head, p...)
	}
	return n, nil
}

func (t *tap) result() (head []byte, total int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.head, t.total
}

// serveWebSocket handles the upgrade end to end. It reports whether it took the
// request; false means this was not a socket and the ordinary path applies.
func (p *Proxy) serveWebSocket(w http.ResponseWriter, r *http.Request, started time.Time) bool {
	if !isWebSocketUpgrade(r.Header) {
		return false
	}

	call := &store.Call{
		Target:     p.target.Name,
		Protocol:   config.ProtocolWebSocket,
		Method:     r.Method,
		Path:       r.URL.RequestURI(),
		ClientAddr: r.RemoteAddr,
		StartedAt:  started,
		TraceID:    trace.ID(r.Header),
		Request:    store.Message{Headers: r.Header.Clone()},
	}
	p.markTLS(call, r, true)

	// A socket hijacks the connection, so it never touches the reverse proxy's
	// transport and has to make the same TLS decision itself. Without this an
	// https:// upstream would be dialled in the clear and answer with a TLS
	// alert the client would report as a broken handshake.
	upstream, err := p.dialUpstream()
	if err != nil {
		p.failSocket(w, call, started, fmt.Errorf("could not reach %s: %w", p.target.Upstream, err))
		return true
	}
	defer upstream.Close()

	// Send the handshake on as it arrived. The URL is rewritten to the upstream
	// and nothing else is touched: the key, the subprotocols and the extensions
	// are what the two ends are negotiating, and changing any of them would
	// make the recorded conversation a different one.
	outbound := r.Clone(r.Context())
	outbound.URL.Scheme, outbound.URL.Host = "http", p.target.UpstreamURL().Host
	outbound.Host = p.target.UpstreamURL().Host
	outbound.RequestURI = ""
	if err := outbound.Write(upstream); err != nil {
		p.failSocket(w, call, started, fmt.Errorf("could not send the handshake: %w", err))
		return true
	}

	fromUpstream := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(fromUpstream, outbound)
	if err != nil {
		p.failSocket(w, call, started, fmt.Errorf("no answer to the handshake: %w", err))
		return true
	}
	call.Status = resp.StatusCode
	call.Response.Headers = resp.Header.Clone()

	// A refused upgrade is an ordinary response, and relaying it as one is what
	// lets the client see why it was refused.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, p.maxBody))
		for name, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)

		call.Response.Body, call.Response.Size = body, int64(len(body))
		call.Duration = time.Since(started)
		p.recorder.Record(call)
		return true
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.failSocket(w, call, started, fmt.Errorf("this server cannot take over the connection"))
		return true
	}
	client, fromClient, err := hijacker.Hijack()
	if err != nil {
		p.failSocket(w, call, started, fmt.Errorf("could not take over the connection: %w", err))
		return true
	}
	defer client.Close()

	// The 101 goes back verbatim, so the client negotiates with the upstream and
	// not with Sonda.
	if err := resp.Write(client); err != nil {
		call.Error = err.Error()
		call.Duration = time.Since(started)
		p.recorder.Record(call)
		return true
	}

	sent := &tap{limit: p.maxBody}     // client to upstream
	received := &tap{limit: p.maxBody} // upstream to client

	// Both readers may already hold frame bytes that arrived alongside the
	// handshake. Copying from the raw connections would silently drop them, and
	// the missing bytes would be the first frames of the conversation.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(upstream, sent), fromClient)
		// Closing the write half tells the upstream the client is done, which
		// is what unblocks the other direction and ends the conversation.
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(client, received), fromUpstream)
		closeWrite(client)
	}()
	wg.Wait()

	call.Request.Body, call.Request.Size = sent.result()
	call.Request.Truncated = call.Request.Size > int64(len(call.Request.Body))
	call.Response.Body, call.Response.Size = received.result()
	call.Response.Truncated = call.Response.Size > int64(len(call.Response.Body))
	call.Duration = time.Since(started)
	p.recorder.Record(call)
	return true
}

// failSocket answers a handshake that could not be completed and records why.
// A socket that simply never opens, with nothing written down, is the hardest
// kind of failure to chase.
func (p *Proxy) failSocket(w http.ResponseWriter, call *store.Call, started time.Time, cause error) {
	call.Error = cause.Error()
	call.Status = http.StatusBadGateway
	call.Duration = time.Since(started)
	p.recorder.Record(call)
	http.Error(w, cause.Error(), http.StatusBadGateway)
}

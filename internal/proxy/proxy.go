// Package proxy sits between a local client and one upstream service, forwards
// the exchange untouched, and hands a copy to the recorder.
//
// Forwarding fidelity comes first. Mirador is a debugger: if it alters what the
// upstream receives or what the client sees, every conclusion drawn from it is
// worthless. Capture is a side effect of the copy, never a step in it.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"mirador/internal/config"
	"mirador/internal/store"
)

// Recorder is the sink for captured calls. It is an interface so the proxy can
// be tested without a database.
type Recorder interface {
	Record(*store.Call)
}

type Proxy struct {
	target   config.Target
	maxBody  int64
	recorder Recorder
	reverse  *httputil.ReverseProxy
	handler  http.Handler
}

// exchange collects both halves of one call while it is in flight.
type exchange struct {
	request  *capture
	response *capture
	upstream *http.Response
	err      error
}

type ctxKey struct{}

func New(target config.Target, maxBody int64, recorder Recorder) *Proxy {
	p := &Proxy{target: target, maxBody: maxBody, recorder: recorder}
	upstream := target.UpstreamURL()

	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if ex := exchangeFrom(resp.Request.Context()); ex != nil {
				ex.upstream = resp
				resp.Body = ex.response.wrap(resp.Body)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if ex := exchangeFrom(r.Context()); ex != nil {
				ex.err = err
			}
			// 502 is Mirador's answer, not the upstream's. The recorded call
			// carries the transport error so the distinction survives.
			http.Error(w, explainUpstreamFailure(target, upstream.String(), err), http.StatusBadGateway)
		},
		// FlushInterval is deliberately left at zero so ReverseProxy decides per
		// response. Its own rule already flushes immediately when the response
		// has no known length, which covers every streaming call.
		//
		// Forcing -1 here looks harmless and is not: it schedules an immediate
		// header flush even for a response with no body, and a gRPC
		// trailers-only reply — the shape a server uses to report an error with
		// nothing to send — stops being trailers-only the moment its headers go
		// out early. The client then waits for trailers that never come and
		// reports Internal instead of the real status. It raced the handler
		// returning, so it failed roughly one run in a hundred.
	}

	p.handler = p
	if target.Protocol == config.ProtocolGRPC {
		p.reverse.Transport = h2cTransport()
		// gRPC clients speak HTTP/2 over cleartext with prior knowledge, which
		// net/http does not accept on its own — it only negotiates HTTP/2 over
		// TLS. h2c is what makes the plaintext case work.
		p.handler = h2cHandler(p)
	}
	return p
}

// Handler is what the listener serves: the proxy itself for HTTP targets, and
// an h2c-capable wrapper for gRPC ones.
func (p *Proxy) Handler() http.Handler { return p.handler }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ex := &exchange{
		request:  newCapture(p.maxBody),
		response: newCapture(p.maxBody),
	}

	replayOf := replayedFrom(r.Header)
	requestHeaders := r.Header.Clone()
	r.Body = ex.request.wrap(r.Body)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, ex))

	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	p.reverse.ServeHTTP(recorder, r)

	// ReverseProxy has copied and closed both bodies by now, so the captures,
	// the byte counts and the upstream trailers are all final.
	call := &store.Call{
		Target:     p.target.Name,
		Protocol:   config.ProtocolHTTP,
		Method:     r.Method,
		Path:       r.URL.RequestURI(),
		Status:     recorder.status,
		ClientAddr: r.RemoteAddr,
		StartedAt:  started,
		Duration:   time.Since(started),
		Error:      errorText(ex.err),
		ReplayOf:   replayOf,
		Request: store.Message{
			Headers:   requestHeaders,
			Body:      ex.request.bytes(),
			Size:      ex.request.size(),
			Truncated: ex.request.truncated(),
		},
		Response: store.Message{
			Headers:   recorder.Header().Clone(),
			Body:      ex.response.bytes(),
			Size:      ex.response.size(),
			Truncated: ex.response.truncated(),
		},
	}

	// A gRPC target can still carry ordinary HTTP — health checks, metrics —
	// so the content type decides per call rather than the configuration.
	if isGRPCRequest(requestHeaders) {
		call.Protocol = config.ProtocolGRPC
	}
	if ex.upstream != nil {
		call.ResponseTrailers = ex.upstream.Trailer.Clone()
		if code, message, ok := grpcOutcome(ex.upstream); ok {
			call.Protocol = config.ProtocolGRPC
			call.GRPCStatus = &code
			call.GRPCMessage = message
		}
	}

	p.recorder.Record(call)
}

// Serve runs the proxy until ctx is cancelled.
func (p *Proxy) Serve(ctx context.Context) error {
	server := &http.Server{
		Addr:    p.target.Listen,
		Handler: p.handler,
		// No read or write timeout: a debugging proxy must not be the thing
		// that kills a slow upstream call the developer is trying to observe.
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Warn("proxy shutdown", "target", p.target.Name, "error", err)
		}
	}()

	slog.Info("proxying", "target", p.target.Name, "listen", p.target.Listen, "upstream", p.target.Upstream)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("target %s: %w", p.target.Name, err)
	}
	return nil
}

func exchangeFrom(ctx context.Context) *exchange {
	ex, _ := ctx.Value(ctxKey{}).(*exchange)
	return ex
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// capture keeps the head of a stream while the whole stream flows through
// untouched, and counts every byte that passed.
//
// The lock is not decoration. A request body is read on the transport's own
// write goroutine, not on the handler's, and a server is free to answer before
// it has read the whole body — a 4xx on the headers does exactly that. The
// handler then reads these fields while the transport may still be writing.
// It is a narrow window on a localhost proxy, which is precisely the kind of
// race that survives every manual test and shows up once in production.
type capture struct {
	limit int64

	mu    sync.Mutex
	head  bytes.Buffer
	total int64
}

func newCapture(limit int64) *capture { return &capture{limit: limit} }

func (c *capture) wrap(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return nil
	}
	return &capturingBody{capture: c, source: body}
}

func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.head.Len() == 0 {
		return nil
	}
	// A copy, not the buffer's own storage: the recorder writes asynchronously
	// and the reader may still append.
	out := make([]byte, c.head.Len())
	copy(out, c.head.Bytes())
	return out
}

func (c *capture) size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *capture) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total > int64(c.head.Len())
}

type capturingBody struct {
	capture *capture
	source  io.ReadCloser
}

func (b *capturingBody) Read(p []byte) (int, error) {
	n, err := b.source.Read(p)
	if n > 0 {
		c := b.capture
		c.mu.Lock()
		c.total += int64(n)
		if room := c.limit - int64(c.head.Len()); room > 0 {
			c.head.Write(p[:min(int64(n), room)])
		}
		c.mu.Unlock()
	}
	return n, err
}

func (b *capturingBody) Close() error { return b.source.Close() }

// statusRecorder remembers the status line so the captured call reports what
// the client actually received, including the 502 Mirador writes itself.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	// A 1xx is interim: the real status still follows. ReverseProxy forwards
	// interim responses, so recording the first status seen would report every
	// PowerShell POST as 100 — Invoke-RestMethod sends Expect: 100-continue by
	// default, and the upstream answers it before doing any work.
	if status >= 200 && !r.written {
		r.status = status
		r.written = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(p)
}

// Flush is load-bearing, not boilerplate. Wrapping the ResponseWriter hides the
// underlying Flusher, and without this passthrough ReverseProxy cannot push a
// stream out as it arrives: every server-streaming call gets buffered and
// delivered in one lump at the end, which silently destroys the timing this
// tool exists to show. TestGRPCServerStreamIsNotBuffered fails if it is removed.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

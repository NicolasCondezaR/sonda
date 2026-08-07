package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiCaller is the slice of Sonda's HTTP API the tools need. Keeping it this
// narrow is what lets the same tools run in-process and against a Sonda in
// another process without either knowing which one it is.
type apiCaller interface {
	Call(ctx context.Context, method, path string, body []byte) (int, []byte, error)
}

// Local calls a handler directly, with no socket in between. Used when the MCP
// endpoint is served by the same process that owns the API.
type Local struct{ Handler http.Handler }

func (l Local) Call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://sonda"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{status: http.StatusOK}
	l.Handler.ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes(), nil
}

// recorder is a minimal ResponseWriter. net/http/httptest has one, but it is a
// testing package and this is production code; the three methods needed here
// are shorter than the import is worth defending.
type recorder struct {
	status  int
	body    bytes.Buffer
	headers http.Header
}

func (r *recorder) Header() http.Header {
	if r.headers == nil {
		r.headers = http.Header{}
	}
	return r.headers
}
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *recorder) WriteHeader(status int)      { r.status = status }

// Remote calls a Sonda running somewhere else. This is what `sonda mcp` uses:
// the agent starts it as a child process, and it talks to the Sonda that
// already has the ports open — so every agent sees the same captures.
type Remote struct {
	Base   string
	Client *http.Client
}

func NewRemote(base string) (*Remote, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not an address like http://127.0.0.1:9000", base)
	}
	return &Remote{
		Base: strings.TrimRight(base, "/"),
		// Generous, because wait_for_call is allowed to sit for a while, and
		// its own context is what bounds it.
		Client: &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

func (r *Remote) Call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.Base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		// The most likely cause by far, and the least obvious from a transport
		// error, is that Sonda simply is not running.
		return 0, nil, fmt.Errorf("could not reach Sonda at %s — is it running? (%w)", r.Base, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, payload, err
}

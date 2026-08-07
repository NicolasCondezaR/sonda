package proxy

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sonda/internal/config"
	"sonda/internal/store"
)

type collector struct {
	mu    sync.Mutex
	calls []*store.Call
}

func (c *collector) Record(call *store.Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *collector) only(t *testing.T) *store.Call {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) != 1 {
		t.Fatalf("expected exactly one captured call, got %d", len(c.calls))
	}
	return c.calls[0]
}

func newProxy(t *testing.T, upstream *httptest.Server, maxBody int64, rec Recorder) *httptest.Server {
	t.Helper()
	target := config.Target{
		Name:     "test",
		Listen:   "127.0.0.1:0",
		Upstream: upstream.URL,
		Protocol: config.ProtocolHTTP,
	}
	front := httptest.NewServer(New(target, maxBody, rec))
	t.Cleanup(front.Close)
	return front
}

// The whole tool is worthless if it changes what the upstream receives or what
// the client gets back, so that is the first thing checked.
func TestForwardingIsByteExact(t *testing.T) {
	requestBody := make([]byte, 300*1024)
	if _, err := rand.Read(requestBody); err != nil {
		t.Fatal(err)
	}
	responseBody := make([]byte, 300*1024)
	if _, err := rand.Read(responseBody); err != nil {
		t.Fatal(err)
	}

	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		received = body
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		w.Write(responseBody)
	}))
	defer upstream.Close()

	rec := &collector{}
	// A body cap far below the payload: truncating the capture must not
	// truncate the traffic.
	front := newProxy(t, upstream, 1024, rec)

	resp, err := http.Post(front.URL+"/things?x=1", "application/octet-stream", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(received, requestBody) {
		t.Errorf("upstream received %d bytes, sent %d", len(received), len(requestBody))
	}
	if !bytes.Equal(got, responseBody) {
		t.Errorf("client received %d bytes, upstream sent %d", len(got), len(responseBody))
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("upstream response header did not reach the client")
	}
}

func TestCaptureRecordsTruncatedBodiesAndRealSizes(t *testing.T) {
	responseBody := strings.Repeat("r", 5000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	rec := &collector{}
	front := newProxy(t, upstream, 100, rec)

	requestBody := strings.Repeat("q", 4000)
	resp, err := http.Post(front.URL+"/echo", "text/plain", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	call := rec.only(t)
	if call.Request.Size != int64(len(requestBody)) {
		t.Errorf("request size = %d, want %d", call.Request.Size, len(requestBody))
	}
	if len(call.Request.Body) != 100 {
		t.Errorf("stored request bytes = %d, want 100", len(call.Request.Body))
	}
	if !call.Request.Truncated {
		t.Error("request should be marked truncated")
	}
	if call.Response.Size != int64(len(responseBody)) {
		t.Errorf("response size = %d, want %d", call.Response.Size, len(responseBody))
	}
	if len(call.Response.Body) != 100 {
		t.Errorf("stored response bytes = %d, want 100", len(call.Response.Body))
	}
	if !call.Response.Truncated {
		t.Error("response should be marked truncated")
	}
	if got := string(call.Request.Body); got != requestBody[:100] {
		t.Errorf("stored request head is not the start of the body: %q", got)
	}
}

func TestCaptureRecordsMetadata(t *testing.T) {
	// The sleep is load-bearing. Windows' monotonic clock is coarse enough that
	// a local round trip can measure exactly zero, so asserting a positive
	// duration without it is flaky rather than strict. 30ms clears the 15.6ms
	// worst-case tick.
	const upstreamWork = 30 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(upstreamWork)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	rec := &collector{}
	front := newProxy(t, upstream, 1024, rec)

	req, err := http.NewRequest(http.MethodGet, front.URL+"/orders/42?verbose=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "abc-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	call := rec.only(t)
	if call.Method != http.MethodGet {
		t.Errorf("method = %q", call.Method)
	}
	if call.Path != "/orders/42?verbose=true" {
		t.Errorf("path = %q, want the full request URI including the query", call.Path)
	}
	if call.Status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", call.Status, http.StatusTeapot)
	}
	if call.Target != "test" {
		t.Errorf("target = %q", call.Target)
	}
	if call.Request.Headers.Get("X-Request-Id") != "abc-123" {
		t.Error("request headers were not captured")
	}
	if call.Duration < upstreamWork {
		t.Errorf("duration = %v, want at least the %v the upstream took", call.Duration, upstreamWork)
	}
	if call.Error != "" {
		t.Errorf("unexpected transport error: %q", call.Error)
	}
}

// A dead upstream is one of the states worth debugging, so it has to be
// captured rather than dropped.
func TestUnreachableUpstreamIsCaptured(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	rec := &collector{}
	target := config.Target{Name: "dead", Listen: "127.0.0.1:0", Upstream: deadURL, Protocol: config.ProtocolHTTP}
	front := httptest.NewServer(New(target, 1024, rec))
	defer front.Close()

	resp, err := http.Get(front.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	call := rec.only(t)
	if call.Error == "" {
		t.Error("the transport error should be recorded")
	}
	if call.Status != http.StatusBadGateway {
		t.Errorf("recorded status = %d, want %d", call.Status, http.StatusBadGateway)
	}
}

// PowerShell's Invoke-RestMethod sends Expect: 100-continue on every POST with
// a body, and ReverseProxy forwards the upstream's interim response. Recording
// the first status seen would file every one of those calls as status 100.
func TestInterimResponseIsNotRecordedAsTheStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body is what makes Go's server emit 100 Continue.
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "done")
	}))
	defer upstream.Close()

	rec := &collector{}
	front := newProxy(t, upstream, 1024, rec)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/things", strings.NewReader(`{"sku":"ABC-9"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Expect", "100-continue")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("client saw status %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	call := rec.only(t)
	if call.Status != http.StatusCreated {
		t.Errorf("recorded status = %d, want %d (an interim 1xx is not the answer)", call.Status, http.StatusCreated)
	}
}

// An upstream is free to answer before it has read the request body — a 413 on
// the headers is exactly that — and when it does, the transport is still
// writing the body on its own goroutine while the handler reads the capture.
//
// That window is what the lock inside capture exists for. Without a test that
// reaches it, `go test -race` passes either way and the lock is unverified
// reasoning rather than a fix. Removing the lock makes this test report a data
// race.
func TestCaptureIsSafeWhenTheUpstreamAnswersBeforeReadingTheBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately does not read r.Body.
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer upstream.Close()

	rec := &collector{}
	front := newProxy(t, upstream, 1<<20, rec)

	// Large enough that the write is still in flight when the answer arrives.
	body := bytes.Repeat([]byte("x"), 8<<20)

	for range 5 {
		req, err := http.NewRequest(http.MethodPost, front.URL+"/upload", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// The upstream closing early can surface as a transport error on the
			// client, which is a valid outcome here: the point is the capture,
			// not the status.
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) == 0 {
		t.Fatal("nothing was captured")
	}
	for _, call := range rec.calls {
		if call.Request.Size < 0 {
			t.Errorf("negative request size %d, which means the counter was read mid-write", call.Request.Size)
		}
	}
}

// The recorder is allowed to lose calls; it is never allowed to hold up the
// response. A blocking sink must not stall the proxy.
func TestSlowRecorderDoesNotBlockTheResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	rec := store.NewRecorder(nil, 1) // buffer of one, never drained
	front := newProxy(t, upstream, 1024, rec)

	for i := 0; i < 50; i++ {
		resp, err := http.Get(front.URL + "/ok")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("request %d got %q", i, body)
		}
	}
	if rec.Dropped() == 0 {
		t.Error("expected drops to be counted once the buffer filled up")
	}
}

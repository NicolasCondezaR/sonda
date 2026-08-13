package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/proxy"
	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/tlsca"
)

// liveStack wires a real proxy in front of a real upstream and a real API, so a
// replay travels the same path a client's request does.
type liveStack struct {
	api      http.Handler
	store    *store.Store
	target   config.Target
	received chan *http.Request
	bodies   chan []byte

	// scheme and client are how a caller reaches the proxy's own port, which
	// differs once that port terminates TLS.
	scheme string
	client *http.Client
}

type directRecorder struct {
	mu    sync.Mutex
	store *store.Store
}

func (d *directRecorder) Record(c *store.Call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.store.Insert(context.Background(), c)
}

func (d *directRecorder) Dropped() int64 { return 0 }

func newLiveStack(t *testing.T) *liveStack { return newStack(t, false) }

// newTLSLiveStack is the same stack with the proxy's own port wrapped exactly as
// the supervisor wraps a `tls: true` service, so a replay has to speak https to
// get anywhere.
func newTLSLiveStack(t *testing.T) *liveStack { return newStack(t, true) }

func newStack(t *testing.T, terminateTLS bool) *liveStack {
	t.Helper()

	stack := &liveStack{
		received: make(chan *http.Request, 8),
		bodies:   make(chan []byte, 8),
		scheme:   "http://",
		client:   http.DefaultClient,
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stack.received <- r.Clone(context.Background())
		stack.bodies <- body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"echo":` + strconv.Quote(string(body)) + `}`))
	}))
	t.Cleanup(upstream.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	stack.store = db

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stack.target = config.Target{
		Name:     "api",
		Listen:   listener.Addr().String(),
		Upstream: upstream.URL,
		Protocol: config.ProtocolHTTP,
		TLS:      terminateTLS,
	}

	if terminateTLS {
		ca, err := tlsca.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		listener = tls.NewListener(listener, ca.Config())
		stack.scheme = "https://"
		// The authority is created per test directory and never trusted by this
		// machine, so the test client has nothing to check against either.
		stack.client = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}

	recorder := &directRecorder{store: db}
	server := &http.Server{Handler: proxy.New(stack.target, 1<<20, recorder, nil, nil).Handler()}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	project, err := db.CreateProject(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveService(context.Background(), store.Service{
		ProjectID: project.ID, Name: stack.target.Name,
		Listen: stack.target.Listen, Upstream: stack.target.Upstream,
		Protocol: stack.target.Protocol, TLS: stack.target.TLS,
	}); err != nil {
		t.Fatal(err)
	}
	// Activated so the API can resolve the channel, but the runtime is not
	// reconciled: the proxy above already owns that port.
	if err := db.ActivateProject(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}

	rt := runtime.NewWithoutListeners(db, recorder, 1<<20)
	if err := rt.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	stack.api = New(db, recorder, rt).Handler()
	return stack
}

// call sends one request through the proxy and returns the id it was captured
// under.
func (s *liveStack) call(t *testing.T, method, path, body string) int64 {
	t.Helper()
	req, err := http.NewRequest(method, s.scheme+s.target.Listen+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "original")
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	<-s.received
	<-s.bodies
	return s.waitForCall(t, 0)
}

// waitForCall polls until a capture newer than after exists. The recorder runs
// off the request path, so the row lands a moment after the response.
func (s *liveStack) waitForCall(t *testing.T, after int64) int64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		calls, err := s.store.List(context.Background(), store.Filter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) > 0 && calls[0].ID > after {
			return calls[0].ID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no call captured after id %d", after)
	return 0
}

func (s *liveStack) replay(t *testing.T, id int64, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/calls/"+strconv.FormatInt(id, 10)+"/replay", reader)
	rec := httptest.NewRecorder()
	s.api.ServeHTTP(rec, req)

	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A replay that does not put the same bytes on the wire is not a replay.
func TestReplaySendsTheOriginalRequestByteForByte(t *testing.T) {
	stack := newLiveStack(t)
	original := `{"sku":"ABC-9","cliente":"Comercial Andes"}`
	id := stack.call(t, http.MethodPost, "/v1/orders?dry=1", original)

	code, out := stack.replay(t, id, "")
	if code != http.StatusOK {
		t.Fatalf("replay returned %d: %v", code, out)
	}

	select {
	case req := <-stack.received:
		if req.URL.RequestURI() != "/v1/orders?dry=1" {
			t.Errorf("upstream saw %q", req.URL.RequestURI())
		}
		if req.Method != http.MethodPost {
			t.Errorf("method = %q", req.Method)
		}
		if got := req.Header.Get("X-Request-Id"); got != "original" {
			t.Errorf("original headers were not carried over: X-Request-Id = %q", got)
		}
		// The marker Sonda adds must never reach the upstream.
		if got := req.Header.Get(proxy.ReplayHeader); got != "" {
			t.Errorf("the replay marker leaked to the upstream: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream never received the replay")
	}

	select {
	case body := <-stack.bodies:
		if string(body) != original {
			t.Errorf("upstream received %q, want %q", body, original)
		}
	case <-time.After(time.Second):
		t.Fatal("no body received")
	}
}

// A service that terminates TLS is reached over https or not at all: the
// listener answers a handshake, so a cleartext replay dies in the transport and
// the user is told nothing except that it failed.
func TestReplayReachesAServiceThatTerminatesTLS(t *testing.T) {
	stack := newTLSLiveStack(t)
	original := `{"sku":"ABC-9"}`
	id := stack.call(t, http.MethodPost, "/v1/orders", original)

	code, out := stack.replay(t, id, "")
	if code != http.StatusOK {
		t.Fatalf("replay returned %d: %v", code, out)
	}
	if msg, _ := out["error"].(string); msg != "" {
		t.Fatalf("the replay never reached the listener: %s", msg)
	}
	if status, _ := out["status"].(float64); status != http.StatusOK {
		t.Errorf("the service answered %v, want 200", out["status"])
	}

	select {
	case body := <-stack.bodies:
		if string(body) != original {
			t.Errorf("upstream received %q, want %q", body, original)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream never received the replay")
	}
}

// The replay is captured like any other traffic and linked to its source, which
// is what makes the diff possible at all.
func TestReplayIsCapturedAndLinked(t *testing.T) {
	stack := newLiveStack(t)
	id := stack.call(t, http.MethodPost, "/v1/orders", `{"sku":"ABC-9"}`)

	if code, out := stack.replay(t, id, ""); code != http.StatusOK {
		t.Fatalf("replay returned %d: %v", code, out)
	}
	<-stack.received
	<-stack.bodies

	replayID := stack.waitForCall(t, id)
	replayed, err := stack.store.Get(context.Background(), replayID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ReplayOf == nil {
		t.Fatal("the replay was not linked back to the original")
	}
	if *replayed.ReplayOf != id {
		t.Errorf("linked to %d, want %d", *replayed.ReplayOf, id)
	}
	// The captured headers must match what the upstream got, marker included —
	// meaning absent.
	if got := replayed.Request.Headers.Get(proxy.ReplayHeader); got != "" {
		t.Errorf("the marker was recorded as part of the request: %q", got)
	}
}

// Refusing is the honest answer: only the head of the body was kept, so the
// request that would go out is not the one that was captured.
func TestReplayRefusesATruncatedCapture(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "truncated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := db.Insert(context.Background(), &store.Call{
		Target: "api", Protocol: "http", Method: "POST", Path: "/v1/orders",
		Status: 200, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Request: store.Message{
			Headers: http.Header{}, Body: []byte(`{"sku":"AB`),
			Size: 4096, Truncated: true,
		},
		Response: store.Message{Headers: http.Header{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := New(db, noDrops{}, emptyRuntime(t, db)).Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/calls/"+strconv.FormatInt(id, 10)+"/replay", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !bytes.Contains([]byte(out["error"]), []byte("max_body_bytes")) {
		t.Errorf("the refusal should say how to fix it, got %q", out["error"])
	}
}

func TestReplayToAnUnknownChannelIsRejected(t *testing.T) {
	stack := newLiveStack(t)
	id := stack.call(t, http.MethodGet, "/v1/orders", "")

	code, out := stack.replay(t, id, `{"target":"does-not-exist"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", code, out)
	}
}

func TestReplayOfAMissingCall(t *testing.T) {
	stack := newLiveStack(t)
	if code, _ := stack.replay(t, 999999, ""); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// A session and a socket are whole conversations, not requests. Both clients
// already hide the control, but an agent over MCP goes straight at the endpoint
// — so the refusal has to live where every caller passes through.
func TestReplayRefusesAConversation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "conversations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	handler := New(db, noDrops{}, emptyRuntime(t, db)).Handler()

	for _, protocol := range []string{"postgres", "websocket", "amqp"} {
		id, err := db.Insert(context.Background(), &store.Call{
			Target: "db", Protocol: protocol, Method: "SESSION", Path: "orders",
			ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
			Request:  store.Message{Headers: http.Header{}},
			Response: store.Message{Headers: http.Header{}},
		})
		if err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/calls/"+strconv.FormatInt(id, 10)+"/replay", http.NoBody)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409", protocol, rec.Code)
		}
		var out map[string]string
		json.Unmarshal(rec.Body.Bytes(), &out)
		if !bytes.Contains([]byte(out["error"]), []byte(protocol)) {
			t.Errorf("%s: the refusal should name the protocol, got %q", protocol, out["error"])
		}
	}
}

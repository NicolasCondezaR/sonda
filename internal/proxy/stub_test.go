package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// fakeStubs stands in for the registry: on or off, and one recorded answer.
type fakeStubs struct {
	enabled  bool
	recorded *store.Call
	asked    int
}

func (f *fakeStubs) On(string) bool { return f.enabled }

func (f *fakeStubs) Match(_ context.Context, _, _, _ string, _ []byte) (*store.Call, error) {
	f.asked++
	return f.recorded, nil
}

// captured collects what the proxy recorded, since a stubbed exchange still has
// to leave a trace.
type captured struct {
	mu    sync.Mutex
	calls []*store.Call
}

func (c *captured) Record(call *store.Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *captured) last() *store.Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return nil
	}
	return c.calls[len(c.calls)-1]
}

func recording(id int64, status int, body string) *store.Call {
	return &store.Call{
		ID:     id,
		Status: status,
		Response: store.Message{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    []byte(body),
		},
	}
}

// stubbed builds a proxy whose upstream address points nowhere on purpose: if
// anything forwards, the test fails loudly instead of passing by accident.
func stubbed(t *testing.T, stubs Stubs) (*httptest.Server, *captured) {
	t.Helper()
	rec := &captured{}
	target := config.Target{
		Name: "ms-rates", Listen: "127.0.0.1:0",
		Upstream: "http://127.0.0.1:1", Protocol: config.ProtocolHTTP,
	}
	front := httptest.NewServer(New(target, 1<<20, rec, stubs))
	t.Cleanup(front.Close)
	return front, rec
}

// The point of the whole feature: the service is unreachable and the call still
// gets its answer.
func TestAStubbedServiceAnswersWithItsUpstreamDown(t *testing.T) {
	front, rec := stubbed(t, &fakeStubs{enabled: true, recorded: recording(7, 200, `{"rate":42}`)})

	resp, err := http.Get(front.URL + "/v1/rates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want the recorded 200", resp.StatusCode)
	}
	if string(body) != `{"rate":42}` {
		t.Errorf("body = %q, want the recorded one", body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("the recorded headers were not replayed: Content-Type = %q", got)
	}

	// The one thing that must never be missing. A recorded answer a client
	// cannot distinguish from a live one is the failure this feature has to
	// avoid.
	if got := resp.Header.Get(StubHeader); got != "7" {
		t.Errorf("%s = %q, want the id of the recording", StubHeader, got)
	}

	// And it is still recorded, linked back to what it came from, or the field
	// would show traffic that never happened as if it had.
	call := rec.last()
	if call == nil {
		t.Fatal("the stubbed exchange was not recorded")
	}
	if call.StubOf == nil || *call.StubOf != 7 {
		t.Errorf("the capture is not linked to the recording: %v", call.StubOf)
	}
}

// Inventing an answer would defeat the purpose. Saying so is the honest reply,
// and it has to be an error a client notices.
func TestNoRecordingIsAnHonestRefusal(t *testing.T) {
	front, rec := stubbed(t, &fakeStubs{enabled: true, recorded: nil})

	resp, err := http.Get(front.URL + "/v1/never-called")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
	if resp.Header.Get(StubHeader) != "none" {
		t.Errorf("%s = %q, want none", StubHeader, resp.Header.Get(StubHeader))
	}
	// The message has to say what to do about it, or the reader assumes Sonda
	// is broken rather than empty.
	for _, want := range []string{"ms-rates", "/v1/never-called", "stubbing off"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the refusal does not mention %q: %s", want, body)
		}
	}
	if call := rec.last(); call == nil || call.StubOf != nil {
		t.Error("a refusal should be recorded, and not as if it came from a recording")
	}
}

// Off is the default and has to stay the default: a proxy that stubs when
// nobody asked would be the worst possible bug in this package.
func TestNothingIsStubbedUnlessItWasTurnedOn(t *testing.T) {
	stubs := &fakeStubs{enabled: false, recorded: recording(7, 200, "should not be used")}
	front, _ := stubbed(t, stubs)

	resp, err := http.Get(front.URL + "/v1/rates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// The upstream is unreachable on purpose, so forwarding fails with 502 —
	// which is exactly the proof that it tried to forward.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 from a real forwarding attempt", resp.StatusCode)
	}
	if strings.Contains(string(body), "should not be used") {
		t.Error("a recording was served with stubbing switched off")
	}
	if stubs.asked != 0 {
		t.Error("the registry was consulted for a service that is not stubbed")
	}
	if resp.Header.Get(StubHeader) != "" {
		t.Error("a forwarded response carries the stub header")
	}
}

// A proxy built without a registry — every test above this file, and any
// embedding that never wires one — must behave exactly as it always did.
func TestANilRegistryNeverStubs(t *testing.T) {
	front, _ := stubbed(t, nil)

	resp, err := http.Get(front.URL + "/v1/rates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want the usual 502", resp.StatusCode)
	}
}

// gRPC carries its real outcome in trailers, after the body. A stub that drops
// them gives the client a well-formed message and then a wait for a status that
// never arrives.
func TestGRPCTrailersAreReplayed(t *testing.T) {
	call := recording(9, 200, "\x00\x00\x00\x00\x00")
	call.Response.Headers = http.Header{"Content-Type": []string{"application/grpc"}}
	call.ResponseTrailers = http.Header{"Grpc-Status": []string{"5"}, "Grpc-Message": []string{"not found"}}

	front, _ := stubbed(t, &fakeStubs{enabled: true, recorded: call})

	req, err := http.NewRequest("POST", front.URL+"/demo.v1.Orders/GetOrder", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body) // trailers only exist once the body is drained

	if got := resp.Trailer.Get("Grpc-Status"); got != "5" {
		t.Errorf("grpc-status trailer = %q, want the recorded 5", got)
	}
	if got := resp.Trailer.Get("Grpc-Message"); got != "not found" {
		t.Errorf("grpc-message trailer = %q", got)
	}
}

// A stubbed call still belongs to the request that made it. Found by looking at
// the tree in the browser and noticing the one stubbed service missing from it:
// the capture carried no trace id, so it fell out of the very request it was
// part of — and the service that was answered from a recording became the one
// that looked like it had never been called.
func TestAStubbedCallStaysInItsRequest(t *testing.T) {
	front, rec := stubbed(t, &fakeStubs{enabled: true, recorded: recording(7, 200, "x")})

	req, err := http.NewRequest("GET", front.URL+"/v1/rates", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	call := rec.last()
	if call == nil {
		t.Fatal("nothing was recorded")
	}
	if call.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("TraceID = %q, want the id the request carried", call.TraceID)
	}
}

// The refusal is part of the request too: a request that got no answer is
// exactly the one somebody will be looking for in the tree.
func TestARefusalAlsoStaysInItsRequest(t *testing.T) {
	front, rec := stubbed(t, &fakeStubs{enabled: true, recorded: nil})

	req, err := http.NewRequest("GET", front.URL+"/v1/never", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "req-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if call := rec.last(); call == nil || call.TraceID != "req-99" {
		t.Errorf("TraceID = %v, want req-99", call.TraceID)
	}
}

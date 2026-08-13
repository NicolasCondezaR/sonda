package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// The diagnosis is the answer to "I pointed it at my service and I see
// nothing", so these tests are about what it refuses to claim as much as about
// what it reports. Real listeners and real sockets throughout: a mocked
// supervisor would prove that the report reads a struct, which is not the thing
// that has to be right.

// handedOut remembers every port this process has already given away. The probe
// listener closes before the address is returned, so the OS may hand the same
// port to the next call — and a test that asks for two ports and gets one twice
// fails on something that is not the code under test. See the longer note on
// the copy of this helper in internal/runtime, where CI hit it.
var handedOut sync.Map

func freePort(t *testing.T) string {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		if _, seen := handedOut.LoadOrStore(addr, true); !seen {
			return addr
		}
	}
	t.Fatal("no unused ephemeral port after 50 tries")
	return ""
}

// withServices brings up a real project with real listeners and returns the
// API in front of it.
func withServices(t *testing.T, services ...store.Service) (http.Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	p, err := db.CreateProject(ctx, "watched")
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range services {
		svc.ProjectID = p.ID
		if _, err := db.SaveService(ctx, svc); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ActivateProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	rt := runtime.New(db, noRecorder{}, 1<<20).WithCADir(dir)
	if err := rt.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	return New(db, noDrops{}, rt).Handler(), db
}

// report and serviceReport are decoded from the wire rather than reusing the
// handler's own structs: a test that shares the type under test cannot catch a
// renamed JSON field, and every other surface reads these names.
type report struct {
	Project  string          `json:"project"`
	Verdict  string          `json:"verdict"`
	Summary  string          `json:"summary"`
	Probed   bool            `json:"upstreams_probed"`
	Note     string          `json:"note"`
	Services []serviceReport `json:"services"`
}

type serviceReport struct {
	Service           string   `json:"service"`
	Listening         bool     `json:"listening"`
	Connections       int64    `json:"connections"`
	Captures          int64    `json:"captures"`
	LastCapture       string   `json:"last_capture"`
	Expects           string   `json:"expects"`
	PointAt           string   `json:"point_at"`
	Verdict           string   `json:"verdict"`
	Detail            string   `json:"detail"`
	UpstreamProbed    bool     `json:"upstream_probed"`
	UpstreamReachable bool     `json:"upstream_reachable"`
	UpstreamError     string   `json:"upstream_error"`
	CannotDistinguish []string `json:"cannot_distinguish"`
	WhatToCheck       []string `json:"what_to_check"`
}

func diagnosis(t *testing.T, h http.Handler, method string) report {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/api/diagnose", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s /api/diagnose answered %d: %s", method, rec.Code, rec.Body.String())
	}
	var out report
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the report is not JSON: %v (%s)", err, rec.Body.String())
	}
	return out
}

// eventually covers the gap between a socket being accepted and the accept
// bookkeeping being visible. It is not patience for a slow machine: the count
// is taken on the server's own goroutine, so a test that reads it immediately
// is reading a race, not a bug.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within the deadline", what)
}

// The one reading Sonda can make with certainty and on its own. A port that
// never opened can capture nothing, and reporting that service as healthy —
// or as merely quiet — sends someone to debug their client for an hour.
func TestAListenerThatNeverBoundIsNotReportedAsHealthy(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	h, _ := withServices(t, store.Service{
		Name: "api", Listen: taken.Addr().String(),
		Upstream: "http://127.0.0.1:65001", Protocol: "http",
	})

	got := diagnosis(t, h, http.MethodGet)
	svc := got.Services[0]

	if svc.Listening {
		t.Error("a service whose port is held by something else is reported as listening")
	}
	if svc.Verdict != verdictListenerDown {
		t.Errorf("verdict = %q, want %q — a port that never opened is not a quiet one", svc.Verdict, verdictListenerDown)
	}
	if got.Verdict != verdictListenerDown {
		t.Errorf("the report's overall verdict is %q; a dead listener must not be summarised away", got.Verdict)
	}
	if svc.Detail == "" || !strings.Contains(svc.Detail, "never opened") {
		t.Errorf("the detail does not say the port never opened: %q", svc.Detail)
	}
}

// Sonda's one blind spot, stated rather than papered over. Nothing has
// connected, and the three causes of that are indistinguishable from here.
func TestNothingConnectedNamesTheCausesItCannotSeparate(t *testing.T) {
	h, _ := withServices(t, store.Service{
		Name: "api", Listen: freePort(t),
		Upstream: "http://127.0.0.1:65001", Protocol: "http",
	})

	got := diagnosis(t, h, http.MethodGet)
	svc := got.Services[0]

	if svc.Verdict != verdictNoConnections {
		t.Fatalf("verdict = %q, want %q", svc.Verdict, verdictNoConnections)
	}
	if len(svc.CannotDistinguish) < 3 {
		t.Errorf("only %d causes offered; bypassing, the wrong port and not having run yet all read the same from here",
			len(svc.CannotDistinguish))
	}
	if len(svc.WhatToCheck) == 0 {
		t.Error("no next step: saying what cannot be told apart is only half the answer")
	}
	if !strings.Contains(strings.Join(svc.WhatToCheck, " "), svc.PointAt) {
		t.Error("the steps do not hand over the line that points the caller at Sonda")
	}
	if !strings.Contains(got.Note, "never connected") {
		t.Errorf("the report does not state its own blind spot: %q", got.Note)
	}
}

// The reading a capture count alone cannot produce, and the one that separates
// "my client is bypassing Sonda" from "my client is reaching Sonda and being
// misunderstood". A TLS record arriving at a plaintext listener is exactly that
// case, and it never becomes a capture.
func TestAConnectionThatNeverBecameACallIsSeenAndReported(t *testing.T) {
	reached, ignored := freePort(t), freePort(t)
	h, _ := withServices(t,
		store.Service{Name: "reached", Listen: reached, Upstream: "http://127.0.0.1:65001", Protocol: "http"},
		store.Service{Name: "ignored", Listen: ignored, Upstream: "http://127.0.0.1:65001", Protocol: "http"},
	)

	conn, err := net.DialTimeout("tcp", reached, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The first bytes of a TLS handshake. A plaintext listener answers this with
	// a bad request and no handler ever runs, so nothing is captured — which is
	// the whole reason the connection has to be counted where it is.
	if _, err := conn.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'}); err != nil {
		t.Fatal(err)
	}
	// Nothing is waited for: a real TLS client gets no answer it can read here
	// either, and the connection was counted when it was accepted.
	conn.Close()

	var got report
	eventually(t, "the connection to be counted", func() bool {
		got = diagnosis(t, h, http.MethodGet)
		return got.Services[0].Connections > 0 || got.Services[1].Connections > 0
	})

	for _, svc := range got.Services {
		switch svc.Service {
		case "reached":
			if svc.Captures != 0 {
				t.Fatalf("a failed handshake became %d capture(s)", svc.Captures)
			}
			if svc.Verdict != verdictConnectedNotCaptured {
				t.Errorf("verdict = %q, want %q — something arrived here and Sonda knows it",
					svc.Verdict, verdictConnectedNotCaptured)
			}
			if !strings.Contains(strings.Join(svc.CannotDistinguish, " "), "TLS") {
				t.Error("the TLS mismatch is not among the causes offered for a plaintext listener")
			}
			if svc.Expects == "" {
				t.Error("the report does not say what this listener expects to speak")
			}
		case "ignored":
			if svc.Connections != 0 {
				t.Errorf("a port nobody dialled reports %d connection(s)", svc.Connections)
			}
			if svc.Verdict != verdictNoConnections {
				t.Errorf("verdict = %q, want %q", svc.Verdict, verdictNoConnections)
			}
		}
	}
}

// With captures on the books the proxy is demonstrably working, and an empty
// screen is the filter. Saying anything else would send someone to re-check
// wiring that is already proven.
func TestCapturesMeanTheWiringIsProven(t *testing.T) {
	h, db := withServices(t, store.Service{
		Name: "api", Listen: freePort(t),
		Upstream: "http://127.0.0.1:65001", Protocol: "http",
	})

	if _, err := db.Insert(context.Background(), &store.Call{
		Project: "watched", Target: "api", Protocol: "http", Method: "GET", Path: "/v1/orders",
		Status: 200, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(), Duration: time.Millisecond,
		Request: store.Message{Headers: http.Header{}}, Response: store.Message{Headers: http.Header{}},
	}); err != nil {
		t.Fatal(err)
	}

	got := diagnosis(t, h, http.MethodGet)
	svc := got.Services[0]
	if svc.Verdict != verdictCapturing {
		t.Fatalf("verdict = %q, want %q", svc.Verdict, verdictCapturing)
	}
	if svc.LastCapture == "" {
		t.Error("no time for the last capture: 'nothing since I started the client' and 'nothing for two hours' are different findings")
	}
	if !strings.Contains(strings.Join(svc.WhatToCheck, " "), "filter") {
		t.Error("the steps do not point at the filter, which is what an empty field means once captures exist")
	}
}

// A probe is traffic the user did not send. It happens when asked for and never
// otherwise — not on a refresh, not on a poll, not on a page load.
func TestOnlyAnExplicitProbeTouchesTheUpstream(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	var dialled atomic.Int64
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			dialled.Add(1)
			conn.Close()
		}
	}()

	h, _ := withServices(t, store.Service{
		Name: "api", Listen: freePort(t),
		Upstream: "http://" + upstream.Addr().String(), Protocol: "http",
	})

	quiet := diagnosis(t, h, http.MethodGet)
	if quiet.Probed || quiet.Services[0].UpstreamProbed {
		t.Error("a plain read reports itself as having probed")
	}
	// Give a probe that should not exist every chance to show up.
	time.Sleep(100 * time.Millisecond)
	if n := dialled.Load(); n != 0 {
		t.Fatalf("reading the diagnosis dialled the upstream %d time(s)", n)
	}

	probed := diagnosis(t, h, http.MethodPost)
	if !probed.Probed || !probed.Services[0].UpstreamProbed {
		t.Error("an explicit probe does not report itself as one")
	}
	if !probed.Services[0].UpstreamReachable {
		t.Errorf("a listening upstream was reported unreachable: %q", probed.Services[0].UpstreamError)
	}
	eventually(t, "the probe to reach the upstream", func() bool { return dialled.Load() == 1 })

	// And the probe is not a capture. Sonda's own bookkeeping appearing in the
	// field as a call the user made would be the tool lying about the traffic.
	_, calls := get(t, h, "/api/calls")
	if list, _ := calls["calls"].([]any); len(list) != 0 {
		t.Errorf("the probe left %d call(s) in the capture list", len(list))
	}
}

// An upstream that refuses connections is a finding worth reporting above
// "nothing has connected": there is no point pointing a client at Sonda while
// the service behind it is down.
func TestAProbeReportsAnUpstreamThatIsNotThere(t *testing.T) {
	h, _ := withServices(t, store.Service{
		Name: "api", Listen: freePort(t),
		// Free, so nothing is listening on it by the time the probe runs.
		Upstream: "http://" + freePort(t), Protocol: "http",
	})

	got := diagnosis(t, h, http.MethodPost)
	svc := got.Services[0]
	if svc.UpstreamReachable {
		t.Fatal("a port with nothing on it was reported reachable")
	}
	if svc.Verdict != verdictUpstreamUnreachable {
		t.Errorf("verdict = %q, want %q", svc.Verdict, verdictUpstreamUnreachable)
	}
	if svc.UpstreamError == "" {
		t.Error("the refusal carries no reason")
	}
}

// No project is the loudest cause of an empty screen and the easiest to fix, so
// it is answered before anything else and never as a per-service reading.
func TestNoActiveProjectIsItsOwnAnswer(t *testing.T) {
	h, _ := newServer(t)

	got := diagnosis(t, h, http.MethodGet)
	if got.Verdict != verdictNoProject {
		t.Fatalf("verdict = %q, want %q", got.Verdict, verdictNoProject)
	}
	if len(got.Services) != 0 {
		t.Error("services are reported for a project that is not active")
	}
	if !strings.Contains(got.Summary, "Activate") {
		t.Errorf("the summary does not say what to do: %q", got.Summary)
	}
}

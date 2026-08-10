package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/fault"
)

// broken stands up a proxy in front of a real upstream, with a rule in force.
func broken(t *testing.T, rule fault.Rule) (*httptest.Server, *captured) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "the real answer")
	}))
	t.Cleanup(upstream.Close)

	faults := fault.New()
	if err := faults.Set("ms-rates", rule); err != nil {
		t.Fatal(err)
	}

	rec := &captured{}
	target := config.Target{
		Name: "ms-rates", Listen: "127.0.0.1:0",
		Upstream: upstream.URL, Protocol: config.ProtocolHTTP,
	}
	front := httptest.NewServer(New(target, 1<<20, rec, nil, faults))
	t.Cleanup(front.Close)
	return front, rec
}

// A status rule must not reach the service at all, or it is not a fault, it is
// a slow success with a different number on it.
func TestAStatusRuleNeverReachesTheService(t *testing.T) {
	front, rec := broken(t, fault.Rule{Status: 503})

	resp, err := http.Get(front.URL + "/v1/rates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 503 {
		t.Errorf("status = %d, want the injected 503", resp.StatusCode)
	}
	if strings.Contains(string(body), "the real answer") {
		t.Error("the service was reached anyway")
	}
	// The header is the one place the distinction reaches code that is not
	// looking at Sonda's interface.
	if resp.Header.Get(FaultHeader) == "" {
		t.Errorf("%s is missing, so a client cannot tell this from a real 503", FaultHeader)
	}

	call := rec.last()
	if call == nil {
		t.Fatal("the injected failure was not recorded")
	}
	if !call.Injected {
		t.Error("the capture does not say the failure was injected")
	}
	if !strings.Contains(call.Error, "on purpose") {
		t.Errorf("the capture explains it as %q", call.Error)
	}
}

// Latency is the case a timeout is written to catch, and the service still has
// to answer: a delay that swallows the response is a different fault.
func TestALatencyRuleStillLetsTheServiceAnswer(t *testing.T) {
	front, _ := broken(t, fault.Rule{LatencyMS: 120})

	started := time.Now()
	resp, err := http.Get(front.URL + "/v1/rates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(started)

	if !strings.Contains(string(body), "the real answer") {
		t.Errorf("the service did not answer: %q", body)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("the call took %s, so the delay was not applied", elapsed)
	}
}

// No answer at all is the failure a well-written client handles differently
// from a 500, and the one almost nobody tests.
func TestACutRuleGivesNoAnswer(t *testing.T) {
	front, rec := broken(t, fault.Rule{Cut: true})

	_, err := http.Get(front.URL + "/v1/rates")
	if err == nil {
		t.Error("the client got an answer from a cut connection")
	}

	// Give the recorder a moment: the connection died, the capture did not.
	for i := 0; i < 50 && rec.last() == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	call := rec.last()
	if call == nil {
		t.Fatal("a cut connection left no capture — the hardest failure to chase")
	}
	if !call.Injected {
		t.Error("the capture does not say the cut was deliberate")
	}
}

// The schedule has to hold end to end, not just inside the registry: two calls
// through, the third broken.
func TestOneInThreeHoldsThroughTheProxy(t *testing.T) {
	front, _ := broken(t, fault.Rule{Status: 503, OneIn: 3})

	var got []int
	for i := 0; i < 6; i++ {
		resp, err := http.Get(front.URL + "/v1/rates")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got = append(got, resp.StatusCode)
	}
	want := []int{200, 200, 503, 200, 200, 503}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %d, want %d (whole run: %v)", i+1, got[i], want[i], got)
		}
	}
}

// Nothing is broken unless somebody said so.
func TestAProxyWithNoRulesForwardsNormally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "the real answer")
	}))
	defer upstream.Close()

	rec := &captured{}
	target := config.Target{Name: "ms-rates", Listen: "127.0.0.1:0", Upstream: upstream.URL, Protocol: config.ProtocolHTTP}
	front := httptest.NewServer(New(target, 1<<20, rec, nil, fault.New()))
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/rates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "the real answer") {
		t.Errorf("a proxy with no rules did not forward: %q", body)
	}
	if resp.Header.Get(FaultHeader) != "" {
		t.Error("an untouched response carries the fault header")
	}
}

package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A rule in force has to be readable from the terminal, and from the two places
// a terminal is looked at: the bar, which is what someone reads when the field
// fills with failures, and the channel, which is where this client says
// everything else that is true of a service.
//
// The terminal reads faults and does not set them, which is exactly the level
// it works at for stubbing.
func TestAnArmedFaultIsVisibleInTheTerminal(t *testing.T) {
	m := sampleModel(t, 120, 30)
	m.broken = map[string]string{"orders": "+2000ms, HTTP 503, one call in 3"}
	frame := stripANSI(m.View())

	if !strings.Contains(frame, "1 BROKEN ON PURPOSE") {
		t.Error("the bar does not say a service is being broken on purpose, so the failures on the field read as the service's own")
	}
	if !strings.Contains(frame, markFault+"orders") {
		t.Error("the channel being broken on purpose carries no mark on the rail")
	}
	if strings.Contains(frame, markFault+"echo") {
		t.Error("a channel with no rule in force was marked as broken on purpose")
	}
}

// The rule itself, beside the call being read. Above one_in 1 most calls pass
// through untouched, so a reader shown nothing but the injected ones cannot
// tell the service is armed at all.
func TestTheInspectorNamesTheRuleInForce(t *testing.T) {
	m := sampleModel(t, 120, 30)
	m.broken = map[string]string{"orders": "+2000ms, one call in 3"}
	m.detail = &CallDetail{Call: Call{
		ID: newID(), Target: "orders", Protocol: "grpc", Method: "POST",
		Path: "/demo.v1.Orders/Get", Status: 200,
	}}

	frame := stripANSI(m.View())
	if !strings.Contains(frame, "+2000ms, one call in 3") {
		t.Error("the inspector does not say what Sonda is doing to this service")
	}

	m.broken = nil
	if strings.Contains(stripANSI(m.View()), "ARMED · orders") {
		t.Error("a service with no rule in force is reported as broken on purpose")
	}
}

// Nothing armed reads as nothing armed. A warning that is always on is a
// warning nobody reads.
func TestNoRuleSaysNothing(t *testing.T) {
	if frame := stripANSI(sampleModel(t, 120, 30).View()); strings.Contains(frame, "BROKEN ON PURPOSE") {
		t.Error("the terminal announces a fault nobody armed")
	}
}

// The rules are written by one package and decoded by another: a renamed field
// would leave the terminal drawing nothing while every test above still passed,
// because they all hand the model a map built by hand.
func TestFaultsDecodeFromWhatTheAPIWrites(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Byte for byte the shape internal/api's faultSnapshot writes.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"faults":{"ms-auth":"+2000ms, one call in 3"},"note":"..."}`))
	}))
	defer api.Close()

	broken, err := NewClient(api.URL).Faults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if broken["ms-auth"] != "+2000ms, one call in 3" {
		t.Errorf("faults decoded as %v, want the rule the API wrote", broken)
	}
}

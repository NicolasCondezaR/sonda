package web

import (
	"strings"
	"testing"
)

// Both runtime switches have to be operable from here, not only readable.
//
// Fault injection shipped with a GET and no POST: the interface drew the BROKEN
// badge for a rule it had no way to arm, so the only surfaces that could arm one
// were curl and an agent. That is the same half-a-feature CONTRIBUTING names,
// and this is the cheapest thing that fails when it happens again.
func TestTheInterfaceCanWorkBothRuntimeSwitches(t *testing.T) {
	source, err := assets.ReadFile("static/sonda.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)

	for _, want := range []string{
		`call("POST", "api/stub"`,
		`call("POST", "api/faults"`,
		`fetch("api/stub")`,
		`fetch("api/faults")`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the interface never does %s, so that half of the switch lives on other surfaces only", want)
		}
	}
}

func TestTheInterfaceExposesAMQPConfigurationAndDecodedFrames(t *testing.T) {
	source, err := assets.ReadFile("static/sonda.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)
	for _, want := range []string{
		`["grpc", "http", "postgres", "amqp"]`,
		`call.amqp.sent`,
		`call.amqp.received`,
		`function renderAMQP`,
		`amqp://127.0.0.1:5672`,
		`call.protocol === "amqp"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the web surface does not expose %s", want)
		}
	}
}

// A capability is not finished until it exists on every surface, and the flow
// comparison is the one most likely to be left behind: it is reachable from an
// agent through one MCP call, which makes it easy to believe it shipped.
func TestTheInterfaceCanCompareTwoRuns(t *testing.T) {
	source, err := assets.ReadFile("static/sonda.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)

	for _, want := range []string{
		`fetch("api/flowdiff?a="`,
		// Holding a run is the only way to reach the comparison from here.
		"HOLD RUN",
		// The three ways the answer can mislead have to be on screen, not only
		// in the payload.
		"same_entry",
		"d.certain",
		"d.unmatched > d.matched",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the interface never does %s, so comparing two runs lives on other surfaces only", want)
		}
	}
}

// The trigger is armed from here and nowhere else in the interface: the
// terminal client reads the armed switches without arming them, the same way it
// already treats injected faults. If this control disappears, the only way left
// to arm one is an agent.
func TestTheInterfaceCanArmAndReadTheTrigger(t *testing.T) {
	source, err := assets.ReadFile("static/sonda.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(source)

	for _, want := range []string{
		`call("POST", "api/trigger"`,
		`fetch("api/trigger")`,
		// A firing arrives as its own named event, not as a call.
		`addEventListener("trigger"`,
		// And it must never take the view from someone already holding it.
		"if (state.held) return;",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the interface never does %s, so the trigger is half wired here", want)
		}
	}
}

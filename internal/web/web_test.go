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

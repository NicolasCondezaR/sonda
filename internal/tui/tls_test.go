package tui

import (
	"strings"
	"testing"
)

// The terminal is a client of the same API as the browser, and a reader must
// never have to leave it to find out whether the service that answered was ever
// identified.
func TestTheInspectorStatesWhetherTheUpstreamWasVerified(t *testing.T) {
	cases := []struct {
		name string
		call Call
		want string
	}{
		{"both ends encrypted", Call{TLS: true, UpstreamTLS: true}, "upstream verified"},
		{"only the upstream", Call{UpstreamTLS: true}, "upstream verified"},
		{"only the client", Call{TLS: true}, "upstream in the clear"},
		{"verification skipped", Call{TLS: true, UpstreamTLS: true, UpstreamInsecure: true}, "NOT VERIFIED"},
		// Nothing to say about a plaintext call between plaintext ends, and
		// saying it anyway would be a line on every row that carries no reading.
		{"neither end", Call{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.call.TLSNote()
			if c.want == "" {
				if got != "" {
					t.Errorf("a plaintext call reports %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("got %q, which does not say %q", got, c.want)
			}
		})
	}
}

func TestTheInspectorDrawsTheEncryptionLine(t *testing.T) {
	m := Model{width: 100, detail: &CallDetail{Call: Call{
		Target: "payments", Protocol: "http", Method: "GET", Path: "/v1/charges",
		Status: 200, TLS: true, UpstreamTLS: true, UpstreamInsecure: true,
	}}}
	out := m.renderInspector()
	if !strings.Contains(out, "NOT VERIFIED") {
		t.Errorf("the inspector never says the upstream went unchecked:\n%s", out)
	}
}

// The rail is the only place the terminal lists targets, so "which of these am
// I not checking the certificate of" has to be answerable from it.
func TestTheRailMarksAnUnverifiedTarget(t *testing.T) {
	m := Model{width: 100, targets: []Target{
		{Name: "checked", Protocol: "http"},
		{Name: "unchecked", Protocol: "http", InsecureSkipVerify: true},
	}}
	out := m.renderLanes(40)
	if !strings.Contains(out, "!unchecked") {
		t.Errorf("the rail names the unverified target the same as the rest:\n%s", out)
	}
	if strings.Contains(out, "!checked") {
		t.Errorf("the rail marks a target that is being verified:\n%s", out)
	}
}

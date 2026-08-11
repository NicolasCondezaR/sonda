package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// emptyModel is the state a first run lands in: channels configured, nothing
// captured, and a user asking why the field is blank.
func emptyModel(t *testing.T, diag *Diagnosis) Model {
	t.Helper()
	m := New(context.Background(), NewClient("http://127.0.0.1:9000"), nil)
	m.live = true
	m.targets = []Target{{Name: "api", Protocol: "http"}, {Name: "orders", Protocol: "grpc"}}
	m.diag = diag
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	return updated.(Model)
}

func sampleDiagnosis() *Diagnosis {
	return &Diagnosis{
		Verdict: "no_connections",
		Summary: "Nothing has connected to at least one Sonda port.",
		Note:    "Sonda cannot see a client that never connected to it.",
		Services: []ServiceDiagnosis{
			{
				Service: "api", Listen: "127.0.0.1:8080", Upstream: "http://127.0.0.1:3000",
				Expects: "plaintext HTTP", PointAt: "API_URL=127.0.0.1:8080",
				Connections: 0, Captures: 0, Verdict: "no_connections",
				Detail:            "Nothing has connected to this port since it opened.",
				CannotDistinguish: []string{"the caller is still pointed at the service itself"},
				WhatToCheck:       []string{"Point the caller at Sonda: API_URL=127.0.0.1:8080"},
			},
			{
				Service: "orders", Listen: "127.0.0.1:8081", Upstream: "http://127.0.0.1:3001",
				Expects: "gRPC over cleartext HTTP/2 (h2c)", PointAt: "ORDERS_GRPC_URL=127.0.0.1:8081",
				Connections: 4, Captures: 0, Verdict: "connected_not_captured",
				Detail:      "4 connection(s) reached this port and none of them became a call.",
				WhatToCheck: []string{"This listener answers gRPC over cleartext HTTP/2 (h2c)."},
			},
		},
	}
}

// An empty field with "select a call" under it is an instruction the reader
// cannot follow. Where there is nothing to select, the inspector says what
// Sonda knows about each channel instead.
func TestTheEmptyInspectorExplainsItself(t *testing.T) {
	frame := stripANSI(emptyModel(t, sampleDiagnosis()).View())

	if strings.Contains(frame, "Select a call") {
		t.Error("the terminal still asks for a selection that cannot be made")
	}
	for _, want := range []string{
		"NOTHING CAPTURED",
		"Nothing has connected to at least one Sonda port.",
		"api", "orders",
		"0 conn", "4 conn",
		"NO CONNECTIONS", "CONNECTED NOT CAPTURED",
		"never connected", // the blind spot, stated
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the empty field does not carry %q:\n%s", want, frame)
		}
	}
}

// The rail cursor already means "this one", so the diagnosis expands whichever
// channel it is on rather than inventing a second kind of selection.
func TestTheCursorChoosesWhichChannelIsExplained(t *testing.T) {
	m := emptyModel(t, sampleDiagnosis())

	first := stripANSI(m.View())
	if !strings.Contains(first, "Point the caller at Sonda") {
		t.Errorf("the first channel's steps are missing:\n%s", first)
	}

	moved, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	second := stripANSI(moved.(Model).View())
	if !strings.Contains(second, "This listener answers gRPC") {
		t.Errorf("moving the cursor did not move the explanation:\n%s", second)
	}
	if strings.Contains(second, "Point the caller at Sonda") {
		t.Error("both channels are expanded at once, which does not fit the inspector")
	}
}

// Dialling somebody's services is not something a keystroke does from the
// middle of an ordinary session.
func TestProbingIsOnlyOfferedWhereItIsExplained(t *testing.T) {
	quiet := emptyModel(t, nil)
	after, cmd := quiet.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd != nil {
		t.Error("p dialled the upstreams with no diagnosis on screen")
	}
	if after.(Model).status != "" {
		t.Errorf("p left a status behind with nothing to probe: %q", after.(Model).status)
	}

	shown := emptyModel(t, sampleDiagnosis())
	if !strings.Contains(stripANSI(shown.View()), "p dial every upstream once") {
		t.Error("the diagnosis does not say the probe exists, so nobody will find it")
	}
	_, cmd = shown.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd == nil {
		t.Error("p did nothing while the diagnosis was on screen")
	}
}

// A verdict that is not a failure must not be painted as one: fault is reserved
// for failure, and a red reading sends someone chasing a fault that is not there.
func TestOnlyRealFailuresAreDrawnAsFaults(t *testing.T) {
	// The colour itself, not the rendered string: a test terminal has no colour
	// profile, so every style renders identically and the comparison would pass
	// no matter what this function returned.
	for verdict, want := range map[string]lipgloss.TerminalColor{
		"listener_down":          colFault,
		"upstream_unreachable":   colFault,
		"capturing":              colArmed,
		"no_connections":         colInkDim,
		"connected_not_captured": colInkDim,
	} {
		if got := verdictStyle(verdict).GetForeground(); got != want {
			t.Errorf("%s is drawn %v, want %v", verdict, got, want)
		}
	}
}

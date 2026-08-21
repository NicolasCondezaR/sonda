package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sampleModel builds a frame the way a real session would look: several
// channels, a burst of traffic, and a couple of faults.
func sampleModel(t *testing.T, width, height int) Model {
	t.Helper()
	now := time.Now()

	m := New(context.Background(), NewClient("http://127.0.0.1:9000"), nil)
	m.now = now
	m.live = true
	m.failedOnly = false
	m.targets = []Target{
		{Name: "echo", Protocol: "http"},
		{Name: "orders", Protocol: "grpc"},
		{Name: "admin-api", Protocol: "http"},
	}
	m.stats = Stats{
		Calls: 128,
		ByTarget: []TargetStat{
			{Target: "echo", Calls: 64, Faults: 2},
			{Target: "orders", Calls: 51, Faults: 3},
			{Target: "admin-api", Calls: 13},
		},
	}

	grpcFault := int32(7)
	for i := range 24 {
		age := time.Duration(i*11) * time.Second
		m.calls = append(m.calls, call("echo", at(now, age), i%9 == 0))
		m.calls = append(m.calls, call("orders", at(now, age+3*time.Second), false))
	}
	m.calls = append(m.calls, Call{
		ID: newID(), Target: "orders", Protocol: "grpc", Method: "POST",
		Path: "/demo.v1.Orders/Fail", Status: 200, DurationMS: 0.53,
		GRPCStatus: &grpcFault, GRPCStatusText: "PermissionDenied",
		GRPCMessage: "no tienes acceso a este pedido",
		started:     at(now, 8*time.Second),
	})

	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(Model)
}

// Nothing may spill past the terminal's edge: a wrapped line pushes the whole
// instrument out of alignment, and the field stops lining up with its rail.
func TestFrameFitsTheTerminal(t *testing.T) {
	for _, width := range []int{80, 120, 200} {
		m := sampleModel(t, width, 30)
		for i, line := range strings.Split(m.View(), "\n") {
			if got := len([]rune(stripANSI(line))); got > width {
				t.Errorf("width %d: line %d is %d characters:\n%s", width, i, got, stripANSI(line))
			}
		}
	}
}

func TestFrameCarriesTheInstrument(t *testing.T) {
	frame := stripANSI(sampleModel(t, 120, 30).View())

	for _, want := range []string{
		"S O N D A",     // masthead
		"LIVE",          // acquisition state
		"FAULTS", "ALL", // the filter switch
		"CHANNEL", "CALLS", "FAULT", // rail header
		"NOW",                         // the axis is a ruler
		"echo", "orders", "admin-api", // every channel has a lane
		"128 CAPTURED", // the readout
		"q quit",       // the footer names its keys
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the frame is missing %q", want)
		}
	}
}

// The rail is one row per channel and the lanes must align with it, or every
// channel is reading someone else's traffic.
func TestEveryChannelGetsExactlyOneLane(t *testing.T) {
	m := sampleModel(t, 120, 30)
	lines := strings.Split(stripANSI(m.View()), "\n")

	found := 0
	for _, target := range m.targets {
		for _, line := range lines {
			if strings.Contains(line, target.Name) && strings.Contains(line, ruleV) {
				found++
				break
			}
		}
	}
	if found != len(m.targets) {
		t.Errorf("%d of %d channels have a lane", found, len(m.targets))
	}
}

func TestFaultsAreDrawnWithTheirOwnShape(t *testing.T) {
	frame := stripANSI(sampleModel(t, 120, 30).View())
	if !strings.Contains(frame, markFault) && !strings.Contains(frame, markMixed) {
		t.Error("the sample has faults but none of them is drawn with the fault shape")
	}
	if !strings.Contains(frame, markCall) {
		t.Error("the sample has ordinary calls but none is drawn")
	}
}

// Selecting a call is the point of the terminal client; if the keys do not move
// the cursor, nothing else matters.
func TestArrowKeysSelectAlongAChannel(t *testing.T) {
	m := sampleModel(t, 120, 30)
	m.lane = 1 // orders

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	after := updated.(Model)
	if after.selected == 0 {
		t.Fatal("pressing left with nothing selected should select the newest call")
	}

	call, ok := after.selectedCall()
	if !ok {
		t.Fatal("the selection does not resolve to a call")
	}
	if call.Target != "orders" {
		t.Errorf("selected a call on %q while the cursor is on orders", call.Target)
	}
}

func TestFilterAndWindowKeysLatch(t *testing.T) {
	m := sampleModel(t, 120, 30)

	toggled, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if toggled.(Model).failedOnly == m.failedOnly {
		t.Error("f did not toggle the fault filter")
	}

	cycled, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	if cycled.(Model).windowIdx == m.windowIdx {
		t.Error("w did not cycle the window")
	}

	held, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	if !held.(Model).held {
		t.Error("h did not hold the trace")
	}
}

// Holding the trace is what makes a moving mark selectable. If the clock keeps
// advancing while held, the cursor still drifts.
func TestHoldStopsTheClock(t *testing.T) {
	m := sampleModel(t, 120, 30)
	held, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	frozen := held.(Model)
	before := frozen.now

	advanced, _ := frozen.Update(tickMsg(before.Add(10 * time.Second)))
	if !advanced.(Model).now.Equal(before) {
		t.Error("the clock advanced while the trace was held")
	}
}

// Prints the frame so it can be looked at, not only asserted about.
func TestShowFrame(t *testing.T) {
	if testing.Short() {
		t.Skip("rendering preview")
	}
	t.Log("\n" + sampleModel(t, 120, 30).View())
}

// The terminal reads the armed switches without arming them, the same restraint
// it already shows with injected faults — and for the same reason: this is the
// window left open while something fires.
func TestTheBarSaysWhenATriggerIsArmedOrHasFired(t *testing.T) {
	m := sampleModel(t, 200, 40)

	m.armed = &TriggerState{Armed: true, Describe: "ms-rates, failed (fires once)"}
	if bar := m.renderBar(); !strings.Contains(bar, "ARMED") || !strings.Contains(bar, "ms-rates") {
		t.Errorf("an armed trigger is invisible in the bar:\n%s", bar)
	}

	fired := &TriggerState{Count: 1}
	fired.Fired = &struct {
		CallID int64  `json:"call_id"`
		Target string `json:"target"`
		Path   string `json:"path"`
	}{CallID: 42, Target: "ms-rates"}
	m.armed = fired
	if bar := m.renderBar(); !strings.Contains(bar, "TRIGGERED") || !strings.Contains(bar, "42") {
		t.Errorf("a trigger that already fired is invisible in the bar:\n%s", bar)
	}
}

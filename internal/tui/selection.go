package tui

import (
	"errors"
	"strings"
	"time"
)

var (
	errNoSelection = errors.New("select a call first — arrow left or right along a channel")
	errNoOriginal  = errors.New("this call is not a replay, so there is nothing to compare it against")
)

// laneCalls are the calls on one channel, oldest first, within the window.
//
// Selection is by call id rather than by column on purpose: the trace advances
// continuously, so a column would point at a different call a second later.
func (m Model) laneCalls(lane int) []Call {
	if lane < 0 || lane >= len(m.targets) {
		return nil
	}
	name := m.targets[lane].Name
	cutoff := m.now.Add(-m.window())

	out := make([]Call, 0, 32)
	for _, call := range m.calls {
		if call.Target == name && !call.Started().Before(cutoff) {
			out = append(out, call)
		}
	}
	return out
}

func (m Model) selectedIndex(calls []Call) int {
	for i, call := range calls {
		if call.ID == m.selected {
			return i
		}
	}
	return -1
}

func (m Model) selectedCall() (Call, bool) {
	if m.selected == 0 {
		return Call{}, false
	}
	for _, call := range m.calls {
		if call.ID == m.selected {
			return call, true
		}
	}
	return Call{}, false
}

// selectedColumn is where the cursor sits on a lane, or -1 when the selection
// is not on this lane or has scrolled out of the window.
func (m Model) selectedColumn(lane, width int) int {
	if m.selected == 0 || lane != m.lane {
		return -1
	}
	call, ok := m.selectedCall()
	if !ok || call.Target != m.targets[lane].Name {
		return -1
	}
	col, ok := Bucket(call.Started(), m.now, m.window(), width)
	if !ok {
		return -1
	}
	return col
}

func matchesPath(path, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(path), strings.ToLower(needle))
}

// windowCalls keeps the slice from growing without bound over a long session.
// Anything older than the widest sweep can never be drawn again.
func windowCalls(calls []Call, now time.Time) []Call {
	cutoff := now.Add(-windows[len(windows)-1].Span)
	out := calls[:0]
	for _, call := range calls {
		if !call.Started().Before(cutoff) {
			out = append(out, call)
		}
	}
	return out
}

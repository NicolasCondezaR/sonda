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

// callByID finds a call still inside the widest window, which is the only place
// a cursor can be pointing.
func (m Model) callByID(id int64) (Call, bool) {
	if id == 0 {
		return Call{}, false
	}
	for _, call := range m.calls {
		if call.ID == id {
			return call, true
		}
	}
	return Call{}, false
}

// cursorColumns is where A and B fall on a lane of this width, or -1 for a
// cursor that is not placed or whose call has left the window. Unlike the
// selection these are not per-lane: time is the axis every channel shares, so a
// cursor crosses all of them.
func (m Model) cursorColumns(width int) (int, int) {
	column := func(id int64) int {
		call, ok := m.callByID(id)
		if !ok {
			return -1
		}
		col, ok := Bucket(call.Started(), m.now, m.window(), width)
		if !ok {
			return -1
		}
		return col
	}
	return column(m.cursorA), column(m.cursorB)
}

// placeCursor pins a cursor to the selected call, or lifts it when it is already
// there: one key both places and clears, the way a latched control works.
func (m *Model) placeCursor(which byte) error {
	if m.selected == 0 {
		return errNoSelection
	}
	target := &m.cursorA
	if which == 'b' {
		target = &m.cursorB
	}
	if *target == m.selected {
		*target = 0
		return nil
	}
	*target = m.selected
	return nil
}

// caliperReading is the span between the cursors, or what is missing before
// there can be one. Empty when neither is placed: an instrument with no cursors
// down has nothing to report, and a permanent dash trains the eye to skip the
// spot where the number appears.
func (m Model) caliperReading() string {
	a, okA := m.callByID(m.cursorA)
	b, okB := m.callByID(m.cursorB)

	switch {
	case !okA && !okB:
		return ""
	case okA != okB:
		placed, missing := "A", "b"
		if okB {
			placed, missing = "B", "a"
		}
		return placed + " SET · " + missing + " ON ANOTHER CALL"
	}

	// The arrow carries which cursor is earlier, so the reading never shows a
	// negative span. Start to start: each call's own duration is already on
	// screen as the width of its mark.
	span := b.Started().Sub(a.Started())
	order := "A→B"
	if span < 0 {
		order, span = "B→A", -span
	}
	return order + " " + humanMillis(span)
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

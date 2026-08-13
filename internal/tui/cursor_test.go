package tui

import (
	"context"
	"strings"
	"testing"
	"time"
)

// cursorModel is two calls on one channel, 342ms apart, which is the shape of
// the question the cursors exist for: this went out, and how long after it did
// the next one.
func cursorModel(t *testing.T) (Model, int64, int64) {
	t.Helper()
	now := time.Now()

	m := New(context.Background(), NewClient("http://127.0.0.1:9000"), nil)
	m.now = now
	m.failedOnly = false
	m.width, m.height = 160, 40
	m.ready = true
	m.targets = []Target{{Name: "gateway", Protocol: "http"}}

	first := call("gateway", at(now, 10*time.Second), false)
	second := call("gateway", at(now, 10*time.Second-342*time.Millisecond), false)
	m.calls = []Call{first, second}
	return m, first.ID, second.ID
}

// Nothing placed is nothing to report. A bar that always carries a dash trains
// the eye to skip the spot where the number appears.
func TestNoCursorPlacedReportsNothing(t *testing.T) {
	m, _, _ := cursorModel(t)
	if got := m.caliperReading(); got != "" {
		t.Errorf("reading with no cursors = %q, want empty", got)
	}
}

// One cursor down is not a measurement, and saying so names the next key rather
// than showing a number that is not there yet.
func TestOneCursorNamesWhatIsMissing(t *testing.T) {
	m, first, _ := cursorModel(t)
	m.selected = first
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}

	got := m.caliperReading()
	if !strings.HasPrefix(got, "A SET") {
		t.Errorf("reading = %q, want it to start with A SET", got)
	}
	if !strings.Contains(got, "b") {
		t.Errorf("reading = %q, does not name the key that completes the measurement", got)
	}
}

// The reading itself: start to start, and the same formatting the web client
// prints for the same capture.
func TestBothCursorsReportTheSpanBetweenTheCalls(t *testing.T) {
	m, first, second := cursorModel(t)

	m.selected = first
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}
	m.selected = second
	if err := m.placeCursor('b'); err != nil {
		t.Fatal(err)
	}

	if got, want := m.caliperReading(), "A→B 342ms"; got != want {
		t.Errorf("reading = %q, want %q", got, want)
	}
}

// Placed in the other order the span is the same and the arrow turns around, so
// the reading never has to show a negative number.
func TestTheArrowCarriesWhichCursorIsEarlier(t *testing.T) {
	m, first, second := cursorModel(t)

	m.selected = second // the later call gets A
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}
	m.selected = first
	if err := m.placeCursor('b'); err != nil {
		t.Fatal(err)
	}

	if got, want := m.caliperReading(), "B→A 342ms"; got != want {
		t.Errorf("reading = %q, want %q", got, want)
	}
}

// The same key places and lifts. Without that there is no way to clear a cursor
// at all, and a cursor you cannot lift is a mark stuck on the field.
func TestTheSameKeyLiftsTheCursorItPlaced(t *testing.T) {
	m, first, _ := cursorModel(t)
	m.selected = first

	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}
	if m.cursorA != first {
		t.Fatalf("cursor A = %d, want %d", m.cursorA, first)
	}
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}
	if m.cursorA != 0 {
		t.Errorf("cursor A = %d after pressing a again, want it lifted", m.cursorA)
	}
}

// With nothing selected there is nothing to place a cursor on, and the refusal
// names what to do instead of failing silently.
func TestPlacingACursorWithNoSelectionSaysWhatToDo(t *testing.T) {
	m, _, _ := cursorModel(t)
	err := m.placeCursor('a')
	if err == nil {
		t.Fatal("placing a cursor with nothing selected was accepted")
	}
	if !strings.Contains(err.Error(), "select a call") {
		t.Errorf("refusal = %q, does not say what to do", err)
	}
}

// A cursor pinned to a call that has left the window has nothing to point at.
// Reporting a span against it would claim a measurement to something the field
// is no longer holding.
func TestACursorOutsideTheWindowStopsReporting(t *testing.T) {
	m, first, second := cursorModel(t)
	m.selected, _ = first, 0
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}
	m.selected = second
	if err := m.placeCursor('b'); err != nil {
		t.Fatal(err)
	}
	if m.caliperReading() == "" {
		t.Fatal("the reading is empty before anything was dropped")
	}

	// The call carrying cursor A is gone, the way windowCalls drops it after a
	// long session.
	m.calls = m.calls[1:]
	got := m.caliperReading()
	if strings.Contains(got, "→") {
		t.Errorf("reading = %q, still reports a span against a call that is gone", got)
	}
	if !strings.HasPrefix(got, "B SET") {
		t.Errorf("reading = %q, want the surviving cursor reported on its own", got)
	}
}

// A cursor crosses every channel, because time is the one axis they share. A
// per-lane cursor would answer a different question than the one being asked.
func TestCursorColumnsAreNotTiedToTheSelectedLane(t *testing.T) {
	m, first, _ := cursorModel(t)
	m.selected = first
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}

	a, b := m.cursorColumns(100)
	if a < 0 || a >= 100 {
		t.Errorf("cursor A column = %d, want it inside a 100-wide field", a)
	}
	if b != -1 {
		t.Errorf("cursor B column = %d, want -1 while it is not placed", b)
	}

	// Moving to a lane the cursor's call does not belong to changes nothing: the
	// column is a time, not a position on one channel.
	m.targets = append(m.targets, Target{Name: "ms-auth", Protocol: "grpc"})
	m.lane = 1
	if again, _ := m.cursorColumns(100); again != a {
		t.Errorf("cursor A moved to column %d when the selected lane changed, was %d", again, a)
	}
}

// The reading is a measurement the user asked for a second ago, so it outlives
// the latched switches when the bar runs out of room — their position is also
// legible from the field they are filtering, and the number is not written
// anywhere else.
//
// The assertion is that ordering, not merely that the reading appears: it has to
// still be there at a width where the switches have already gone. Appending it
// after them instead reads identically on a wide terminal and fails here.
func TestTheCursorReadingOutlivesTheSwitches(t *testing.T) {
	m, first, second := cursorModel(t)
	m.selected = first
	if err := m.placeCursor('a'); err != nil {
		t.Fatal(err)
	}
	m.selected = second
	if err := m.placeCursor('b'); err != nil {
		t.Fatal(err)
	}

	m.width = 60
	bar := stripANSI(m.renderBar())

	if strings.Contains(bar, "30M") || strings.Contains(bar, "FAULTS") {
		t.Fatalf("60 columns was not narrow enough to shed the switches, so this proves nothing: %q", bar)
	}
	if !strings.Contains(bar, "342ms") {
		t.Errorf("the switches were shed but so was the reading: %q", bar)
	}
}

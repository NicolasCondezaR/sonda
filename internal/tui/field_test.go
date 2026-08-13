package tui

import (
	"strings"
	"testing"
	"time"
)

func at(now time.Time, ago time.Duration) time.Time { return now.Add(-ago) }

func TestBucketPlacesNowAtTheRightEdge(t *testing.T) {
	now := time.Now()
	const width = 100
	window := 5 * time.Minute

	cases := []struct {
		name string
		ago  time.Duration
		want int
	}{
		{"just happened", 0, width - 1},
		{"a whole window ago", window, 0},
		{"halfway", window / 2, width/2 - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			col, ok := Bucket(at(now, tc.ago), now, window, width)
			if !ok {
				t.Fatal("expected the call to land inside the window")
			}
			if col != tc.want {
				t.Errorf("column = %d, want %d", col, tc.want)
			}
		})
	}
}

// Anything outside the sweep is not drawn, and that includes the future:
// clock skew between a captured timestamp and this process is not something to
// hide by clamping it into the newest column.
func TestBucketRejectsWhatIsOutsideTheWindow(t *testing.T) {
	now := time.Now()
	window := time.Minute

	if _, ok := Bucket(at(now, 2*time.Minute), now, window, 80); ok {
		t.Error("a call older than the window should not be placed")
	}
	if _, ok := Bucket(now.Add(time.Second), now, window, 80); ok {
		t.Error("a call in the future should not be placed")
	}
	if _, ok := Bucket(now, now, window, 0); ok {
		t.Error("a zero-width lane can hold nothing")
	}
}

// nextID hands out ids the way the store does: from one upwards. Zero is the
// "nothing selected" sentinel, so a sample call must never land on it.
//
// Every sample call in this package takes its id from here, and none writes one
// by hand. Two did, both `ID: 500`, and the counter climbs across a whole
// package run: once it crossed 500 a generated call collided with them, two
// calls on different channels shared an id, and selectedCall() returned
// whichever came first in the slice. It surfaced as
// TestArrowKeysSelectAlongAChannel selecting a call on the wrong channel — a
// failure that pointed at the selection code, which was correct, and that
// appeared only because an unrelated test was added ahead of it. Uniqueness is
// the helper's promise to make, not something each call site can be trusted to
// avoid breaking.
var nextID int64

func newID() int64 {
	nextID++
	return nextID
}

func call(target string, started time.Time, fault bool) Call {
	c := Call{ID: newID(), Target: target, Status: 200, started: started}
	if fault {
		c.Status = 503
	}
	return c
}

func TestLaneCellsCountsSeveralCallsInOneCell(t *testing.T) {
	now := time.Now()
	window := time.Minute
	const width = 60

	// Three calls close enough together to share a column, one of them failing.
	calls := []Call{
		call("api", at(now, 30*time.Second), false),
		call("api", at(now, 30*time.Second), false),
		call("api", at(now, 30*time.Second), true),
	}

	cells := LaneCells(calls, now, window, width)
	total, faults := 0, 0
	for _, c := range cells {
		total += c.Calls
		faults += c.Faults
	}
	if total != 3 || faults != 1 {
		t.Fatalf("counted %d calls and %d faults, want 3 and 1", total, faults)
	}

	// A burst must not read as a single ordinary call.
	for _, c := range cells {
		if c.Calls == 3 && Mark(c) != markMixed {
			t.Errorf("a cell holding a fault among successes rendered as %q", Mark(c))
		}
	}
}

// Shape carries the outcome before colour does, which is what keeps the field
// readable to someone who cannot tell the two colours apart.
func TestMarkDistinguishesByShape(t *testing.T) {
	cases := []struct {
		name string
		cell Cell
		want string
	}{
		{"empty", Cell{}, markEmpty},
		{"one call", Cell{Calls: 1}, markCall},
		{"one fault", Cell{Calls: 1, Faults: 1}, markFault},
		{"a fault among successes", Cell{Calls: 4, Faults: 1}, markMixed},
	}
	for _, tc := range cases {
		if got := Mark(tc.cell); got != tc.want {
			t.Errorf("%s: mark = %q, want %q", tc.name, got, tc.want)
		}
	}

	// The four glyphs must actually differ, or the shape rule is decorative.
	seen := map[string]bool{}
	for _, g := range []string{markEmpty, markCall, markFault, markMixed} {
		if seen[g] {
			t.Errorf("glyph %q is used for more than one meaning", g)
		}
		seen[g] = true
	}
}

func TestDivisionColumnsStayInsideTheLane(t *testing.T) {
	for _, width := range []int{1, 7, 40, 200} {
		for _, count := range []int{4, 5, 6} {
			for _, col := range divisionColumns(width, count) {
				if col < 0 || col >= width {
					t.Errorf("width %d, %d divisions: column %d is outside the lane", width, count, col)
				}
			}
		}
	}
	if cols := divisionColumns(0, 4); len(cols) != 0 {
		t.Errorf("a zero-width lane has no divisions, got %v", cols)
	}
}

// The axis is a ruler: every tick must be a round figure, or the grid stops
// being a measurement grid.
func TestAxisLabelsAreRoundFigures(t *testing.T) {
	for _, w := range windows {
		labels := axisLabels(120, w.Span, w.Divisions, noCursors)
		plain := stripANSI(labels)
		if !strings.Contains(plain, "NOW") {
			t.Errorf("window %s: the axis has no NOW", w.Label)
		}
		for _, field := range strings.Fields(plain) {
			if field == "NOW" {
				continue
			}
			if strings.Contains(field, ".") {
				t.Errorf("window %s: tick %q is not a round figure", w.Label, field)
			}
		}
	}
}

func TestAxisLabelsFitTheWidth(t *testing.T) {
	for _, width := range []int{20, 60, 200} {
		plain := stripANSI(axisLabels(width, 5*time.Minute, 5, noCursors))
		if len([]rune(plain)) != width {
			t.Errorf("width %d: the axis is %d characters wide", width, len([]rune(plain)))
		}
	}
}

func TestRenderLaneIsOneCellPerColumn(t *testing.T) {
	cells := LaneCells(nil, time.Now(), time.Minute, 40)
	plain := stripANSI(renderLane(cells, channelColor(0), divisionColumns(40, 4), -1, noCursors))
	if len([]rune(plain)) != 40 {
		t.Errorf("a 40-column lane rendered %d characters", len([]rune(plain)))
	}
}

// stripANSI removes styling so a test can measure the text a terminal shows.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// A recording is not a measurement, and the field has to say so with a shape —
// the same rule the web client follows with a hatch, and the same reason: an
// answer that never reached its service must not look like one that did.
func TestAStubbedCellIsADifferentShape(t *testing.T) {
	cases := []struct {
		name string
		cell Cell
		want string
	}{
		{"a plain call", Cell{Calls: 1}, markCall},
		{"answered from a recording", Cell{Calls: 1, Stubs: 1}, markStub},
		{"all of them from recordings", Cell{Calls: 3, Stubs: 3}, markStub},
		// A failure outranks a recording: the failure is why the tool is open,
		// and a cell can only carry one shape.
		{"a stubbed failure", Cell{Calls: 1, Stubs: 1, Faults: 1}, markFault},
		// Mixed live and recorded is not "recorded": saying so would claim more
		// than the cell knows.
		{"some recorded, some real", Cell{Calls: 3, Stubs: 1}, markCall},
		{"nothing", Cell{}, markEmpty},
	}
	for _, c := range cases {
		if got := Mark(c.cell); got != c.want {
			t.Errorf("%s: mark = %q, want %q", c.name, got, c.want)
		}
	}
}

// noCursors is the field with neither cursor placed, which is how it renders
// until someone asks for a measurement.
var noCursors = [2]int{-1, -1}

// A cursor is drawn over the trace, not instead of it. One terminal cell holds
// one glyph, so a cursor that replaced the mark would hide a call in the exact
// column being measured — and hiding a call is the one thing this field cannot
// do.
func TestACursorDoesNotHideTheCallItIsMeasuring(t *testing.T) {
	cells := make([]Cell, 12)
	cells[4] = Cell{Calls: 1}
	cells[9] = Cell{Calls: 1, Faults: 1}

	plain := stripANSI(renderLane(cells, channelColor(0), nil, -1, [2]int{4, 9}))
	runes := []rune(plain)

	if got := string(runes[4]); got != markCall {
		t.Errorf("cell 4 under cursor A = %q, want the call mark %q", got, markCall)
	}
	if got := string(runes[9]); got != markFault {
		t.Errorf("cell 9 under cursor B = %q, want the fault mark %q", got, markFault)
	}
}

// On an empty cell the cursor is the hairline itself, so the column is visibly
// crossed even where no call landed on it.
func TestACursorOnAnEmptyCellDrawsTheHairline(t *testing.T) {
	plain := stripANSI(renderLane(make([]Cell, 8), channelColor(0), nil, -1, [2]int{3, -1}))
	runes := []rune(plain)

	if got := string(runes[3]); got != gridV {
		t.Errorf("empty cell under a cursor = %q, want %q", got, gridV)
	}
	if got := string(runes[2]); got != markEmpty {
		t.Errorf("cell 2 has no cursor on it and reads %q, want %q", got, markEmpty)
	}
}

// The letters engrave on the ruler, and the row has to stay exactly as wide as
// the field: the rail and every lane are aligned to it, and one extra cell walks
// every channel off its own traffic.
func TestTheCursorLettersLandOnTheAxisWithoutWideningIt(t *testing.T) {
	const width = 60
	plain := stripANSI(axisLabels(width, 5*time.Minute, 5, [2]int{10, 40}))

	if got := len([]rune(plain)); got != width {
		t.Fatalf("axis is %d cells wide, want %d", got, width)
	}
	if got := string([]rune(plain)[10]); got != "A" {
		t.Errorf("column 10 of the ruler = %q, want A", got)
	}
	if got := string([]rune(plain)[40]); got != "B" {
		t.Errorf("column 40 of the ruler = %q, want B", got)
	}
}

// Both clients read the same captures, so they have to print the same number.
// The cases are the web client's duration() step for step.
func TestHumanMillisMatchesTheWebClientsFormatting(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{342 * time.Millisecond, "342ms"},
		{1201 * time.Millisecond, "1.20s"},
		{time.Second, "1.00s"},
		{999 * time.Millisecond, "999ms"},
		{500 * time.Microsecond, "0.50ms"},
		{0, "0.00ms"},
	} {
		if got := humanMillis(c.in); got != c.want {
			t.Errorf("humanMillis(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

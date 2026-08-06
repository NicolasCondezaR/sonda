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
var nextID int64

func call(target string, started time.Time, fault bool) Call {
	nextID++
	c := Call{ID: nextID, Target: target, Status: 200, started: started}
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
		labels := axisLabels(120, w.Span, w.Divisions)
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
		plain := stripANSI(axisLabels(width, 5*time.Minute, 5))
		if len([]rune(plain)) != width {
			t.Errorf("width %d: the axis is %d characters wide", width, len([]rune(plain)))
		}
	}
}

func TestRenderLaneIsOneCellPerColumn(t *testing.T) {
	cells := LaneCells(nil, time.Now(), time.Minute, 40)
	plain := stripANSI(renderLane(cells, channelColor(0), divisionColumns(40, 4), -1))
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

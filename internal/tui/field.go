package tui

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Cell is one column of one lane: a bucket of time holding whatever landed in
// it. Several calls can share a cell, which is why the counts matter — a burst
// must not render as a single call.
type Cell struct {
	Calls  int
	Faults int
}

// Bucket assigns a call to a column. The right edge is now and time runs
// leftwards, so a call from exactly one window ago lands in column zero.
//
// Returns false when the call falls outside the window, including the future:
// clock skew between a captured timestamp and this process is not something to
// paper over by clamping it into the last column.
func Bucket(started, now time.Time, window time.Duration, width int) (int, bool) {
	if width <= 0 || window <= 0 {
		return 0, false
	}
	age := now.Sub(started)
	if age < 0 || age > window {
		return 0, false
	}
	// Newest at the right: column = width-1 for age zero.
	col := width - 1 - int(float64(age)/float64(window)*float64(width))
	if col < 0 {
		col = 0
	}
	if col >= width {
		col = width - 1
	}
	return col, true
}

// LaneCells distributes one channel's calls across the lane.
func LaneCells(calls []Call, now time.Time, window time.Duration, width int) []Cell {
	cells := make([]Cell, max(width, 0))
	for _, call := range calls {
		col, ok := Bucket(call.Started(), now, window, width)
		if !ok {
			continue
		}
		cells[col].Calls++
		if call.Fault() {
			cells[col].Faults++
		}
	}
	return cells
}

// Mark is the glyph for a cell. Shape carries the outcome before colour does: a
// fault fills the cell, an ordinary call fills half of it, and an empty cell is
// the grid showing through. That ordering is what keeps the field readable to
// someone who cannot tell the two colours apart.
func Mark(c Cell) string {
	switch {
	case c.Faults > 0 && c.Calls > c.Faults:
		return markMixed
	case c.Faults > 0:
		return markFault
	case c.Calls > 0:
		return markCall
	default:
		return markEmpty
	}
}

// renderLane draws one channel's row, with the time grid showing through the
// empty cells so the divisions always line up with the labelled axis.
func renderLane(cells []Cell, colour lipgloss.Color, divisions []int, selected int) string {
	call := lipgloss.NewStyle().Foreground(colour)
	var b strings.Builder

	for i, cell := range cells {
		glyph := Mark(cell)
		switch {
		case i == selected:
			b.WriteString(styleSelected.Render(glyph))
		case cell.Faults > 0:
			b.WriteString(styleFault.Render(glyph))
		case cell.Calls > 0:
			b.WriteString(call.Render(glyph))
		case contains(divisions, i):
			b.WriteString(styleGrid.Render(gridV))
		default:
			b.WriteString(styleGrid.Render(markEmpty))
		}
	}
	return b.String()
}

// divisionColumns are the fixed time divisions. They stay put while events move
// under them, which is what makes the grid a measurement grid rather than
// texture.
func divisionColumns(width, count int) []int {
	if count <= 0 || width <= 0 {
		return nil
	}
	cols := make([]int, 0, count)
	for i := 1; i < count; i++ {
		cols = append(cols, i*width/count)
	}
	return cols
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// axisLabels writes the time ruler. Values land on round figures because the
// division count is chosen per window, the same way the web client does it.
func axisLabels(width int, window time.Duration, count int) string {
	if width <= 0 {
		return ""
	}
	row := []rune(strings.Repeat(" ", width))
	place := func(col int, text string) {
		for i, r := range text {
			if col+i >= 0 && col+i < width {
				row[col+i] = r
			}
		}
	}

	step := window / time.Duration(count)
	for i := range count {
		place(i*width/count, "-"+shortSpan(window-time.Duration(i)*step))
	}
	place(width-3, "NOW")
	return styleFaint.Render(string(row))
}

func shortSpan(d time.Duration) string {
	seconds := int(d.Round(time.Second).Seconds())
	if seconds >= 60 && seconds%60 == 0 {
		return strconv.Itoa(seconds/60) + "M"
	}
	return strconv.Itoa(seconds) + "S"
}

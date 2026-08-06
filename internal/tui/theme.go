// Package tui is the terminal client. It reads the same HTTP API the web
// interface reads — that seam is why a second client costs a package rather
// than a rewrite.
//
// The visual system is DESIGN.md, translated rather than reinvented. Most of it
// lands directly: monospace is free here, hairlines become box-drawing
// characters, and the probe-colour channel identity carries over unchanged.
// Two things need a different expression. There are no type sizes, so the four
// roles become weight and dimming. And a lane is one row tall, so a fault
// cannot be a taller bar — it becomes a full block where an ordinary call is a
// half one, which is still a difference in shape and still survives a terminal
// with no colour.
package tui

import "github.com/charmbracelet/lipgloss"

// The instrument palette, straight from DESIGN.md.
const (
	colCase     = lipgloss.Color("#14171c")
	colField    = lipgloss.Color("#0f1216")
	colRaised   = lipgloss.Color("#1b1f26")
	colRule     = lipgloss.Color("#3a4048")
	colGrid     = lipgloss.Color("#242a32")
	colInk      = lipgloss.Color("#e6e9ee")
	colInkDim   = lipgloss.Color("#9aa3ae")
	colInkFaint = lipgloss.Color("#78828d")
	colFault    = lipgloss.Color("#ff5257")
	colArmed    = lipgloss.Color("#5fa858")
)

// channelColors is the logic-probe colour code in its standard order, minus
// black. Identical to the web client's list and assigned the same way — by
// configuration order — so a channel is the same colour in both.
var channelColors = []lipgloss.Color{
	lipgloss.Color("#8a5a3c"), // brown
	lipgloss.Color("#c8443c"), // red
	lipgloss.Color("#d97b2b"), // orange
	lipgloss.Color("#d8b843"), // yellow
	lipgloss.Color("#5fa858"), // green
	lipgloss.Color("#4a86c8"), // blue
	lipgloss.Color("#8b6bc4"), // violet
	lipgloss.Color("#8a8f98"), // grey
	lipgloss.Color("#d6dae0"), // white
}

func channelColor(index int) lipgloss.Color {
	return channelColors[index%len(channelColors)]
}

// The marks. A call is a half block and a fault is a full one: colour says
// which, but the shape says it too, which is the rule the web client follows
// with a taller bar.
const (
	markEmpty = "·"
	markCall  = "▄"
	markFault = "█"
	markMixed = "▆" // several calls in one cell, at least one of them a fault
)

var (
	styleLabel = lipgloss.NewStyle().Foreground(colInkFaint).Bold(true)
	styleInk   = lipgloss.NewStyle().Foreground(colInk)
	styleDim   = lipgloss.NewStyle().Foreground(colInkDim)
	styleFaint = lipgloss.NewStyle().Foreground(colInkFaint)
	styleFault = lipgloss.NewStyle().Foreground(colFault)
	styleArmed = lipgloss.NewStyle().Foreground(colArmed)
	styleGrid  = lipgloss.NewStyle().Foreground(colGrid)
	styleRule  = lipgloss.NewStyle().Foreground(colRule)

	styleMasthead = lipgloss.NewStyle().Foreground(colInk).Bold(true)
	styleField    = lipgloss.NewStyle().Background(colField)

	// A latched control shows its position by ink and a raised ground, never by
	// a filled accent — the same rule the web client's switches follow.
	styleKey        = lipgloss.NewStyle().Foreground(colInkFaint).Padding(0, 1)
	styleKeyEngaged = lipgloss.NewStyle().Foreground(colInk).Background(colRaised).Bold(true).Padding(0, 1)

	styleSelected = lipgloss.NewStyle().Foreground(colInk).Background(colRaised)
	styleHeader   = lipgloss.NewStyle().Foreground(colInkFaint).Bold(true)
)

// Hairlines. Rules are one character, always, as they are one pixel in the web
// client.
const (
	ruleH     = "─"
	ruleV     = "│"
	gridV     = "│"
	cornerTL  = "┌"
	cornerTR  = "┐"
	cornerBL  = "└"
	cornerBR  = "┘"
	teeDown   = "┬"
	teeUp     = "┴"
	teeRight  = "├"
	teeLeft   = "┤"
	crossRule = "┼"
)

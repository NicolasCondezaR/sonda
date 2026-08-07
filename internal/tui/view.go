package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	// The rail is a fixed grid. Deriving both the header and the rows from the
	// same widths is not tidiness: a row one character wider than its header
	// shifts every lane off the channel it belongs to, which is the one thing
	// this layout cannot get wrong.
	colCursor = 1 // ▸
	colSwatch = 2 // ■ and the space after it
	colCalls  = 6
	colFaults = 6
	colName   = railWidth - colCursor - colSwatch - colCalls - colFaults

	railWidth      = 26
	minFieldWidth  = 24
	inspectorLines = 14
)

func (m Model) View() string {
	if !m.ready {
		return styleFaint.Render("starting…")
	}
	if m.err != nil && len(m.targets) == 0 {
		return m.renderUnreachable()
	}

	fieldWidth := max(m.width-railWidth-1, minFieldWidth)

	var b strings.Builder
	b.WriteString(m.renderBar())
	b.WriteByte('\n')
	b.WriteString(m.renderAxis(fieldWidth))
	b.WriteByte('\n')
	b.WriteString(m.renderLanes(fieldWidth))
	b.WriteString(m.renderSeparator(fieldWidth))
	b.WriteByte('\n')
	b.WriteString(m.renderInspector())
	b.WriteByte('\n')
	b.WriteString(m.renderFooter())
	return b.String()
}

func (m Model) renderUnreachable() string {
	return lipgloss.JoinVertical(lipgloss.Left,
		styleMasthead.Render("SONDA"),
		"",
		styleFault.Render("Cannot reach the API."),
		styleFaint.Render(m.err.Error()),
		"",
		styleFaint.Render("Is sonda running? Point this at it with -api."),
		"",
		styleFaint.Render("q  quit"),
	)
}

/* ------------------------------------------------------------------ bar -- */

func (m Model) renderBar() string {
	lamp, state := styleFaint.Render("■"), styleFaint.Render("CONNECTING")
	switch {
	case m.held:
		lamp, state = styleDim.Render("■"), styleDim.Render("TRACE HELD")
	case m.err != nil:
		lamp, state = styleFault.Render("■"), styleFault.Render("RECONNECTING")
	case m.live:
		lamp, state = styleArmed.Render("■"), styleDim.Render("LIVE")
	}

	// Shed from the least load-bearing end when the terminal is narrow. The
	// masthead and the acquisition lamp stay: knowing whether the feed is live
	// outranks knowing which sweep is selected.
	pieces := []string{
		styleMasthead.Render("M I R A D O R"),
		lamp + " " + state,
		m.key("FAULTS", m.failedOnly) + m.key("ALL", !m.failedOnly),
		m.windowKeys(),
	}
	right := m.renderReadout()

	for {
		left := strings.Join(pieces, "  ")
		gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap >= 1 {
			return left + strings.Repeat(" ", gap) + right
		}
		if len(pieces) > 2 {
			pieces = pieces[:len(pieces)-1]
			continue
		}
		if right != "" {
			right = ""
			continue
		}
		return truncate(left, m.width)
	}
}

// key is a latched control: ink and a raised ground show its position, never a
// filled accent.
func (m Model) key(label string, engaged bool) string {
	if engaged {
		return styleKeyEngaged.Render(label)
	}
	return styleKey.Render(label)
}

func (m Model) windowKeys() string {
	var b strings.Builder
	for i, w := range windows {
		b.WriteString(m.key(w.Label, i == m.windowIdx))
	}
	return b.String()
}

func (m Model) renderReadout() string {
	shown, faults := len(m.calls), 0
	for _, call := range m.calls {
		if call.Fault() {
			faults++
		}
	}

	parts := []string{fmt.Sprintf("%d CAPTURED", m.stats.Calls)}
	if m.failedOnly {
		parts = append(parts, fmt.Sprintf("%d FLAGGED", shown))
	} else {
		parts = append(parts, fmt.Sprintf("%d SHOWN", shown))
		if faults > 0 {
			parts = append(parts, fmt.Sprintf("%d FLAGGED", faults))
		}
	}
	if m.stats.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d DROPPED", m.stats.Dropped))
	}

	text := strings.Join(parts, "  ·  ")
	if m.failedOnly && shown > 0 {
		return styleFault.Render(text)
	}
	return styleDim.Render(text)
}

/* ---------------------------------------------------------------- field -- */

func (m Model) renderAxis(fieldWidth int) string {
	head := styleHeader.Render(pad("CHANNEL", colCursor+colSwatch+colName)) +
		styleHeader.Render(pad("CALLS", colCalls)) + styleHeader.Render(pad("FAULT", colFaults))
	return head + styleRule.Render(ruleV) + axisLabels(fieldWidth, m.window(), m.divisions())
}

func (m Model) renderLanes(fieldWidth int) string {
	if len(m.targets) == 0 {
		return styleFaint.Render("  no channels configured") + "\n"
	}

	divisions := divisionColumns(fieldWidth, m.divisions())
	byTarget := map[string]TargetStat{}
	for _, stat := range m.stats.ByTarget {
		byTarget[stat.Target] = stat
	}

	var b strings.Builder
	for i, target := range m.targets {
		colour := channelColor(i)
		cells := LaneCells(m.laneCalls(i), m.now, m.window(), fieldWidth)

		swatch := lipgloss.NewStyle().Foreground(colour).Render("■") + " "

		nameStyle := styleDim
		cursor := " "
		if i == m.lane {
			nameStyle, cursor = styleInk, styleInk.Render("▸")
		}

		stat := byTarget[target.Name]
		faultStyle := styleFaint
		if stat.Faults > 0 {
			faultStyle = styleFault
		}

		b.WriteString(cursor + swatch +
			nameStyle.Render(pad(target.Name, colName)) +
			styleFaint.Render(pad(fmt.Sprint(stat.Calls), colCalls)) +
			faultStyle.Render(pad(fmt.Sprint(stat.Faults), colFaults)) +
			styleRule.Render(ruleV) +
			renderLane(cells, colour, divisions, m.selectedColumn(i, fieldWidth)) + "\n")
	}
	return b.String()
}

func (m Model) renderSeparator(fieldWidth int) string {
	return styleRule.Render(strings.Repeat(ruleH, railWidth) + teeUp + strings.Repeat(ruleH, fieldWidth))
}

/* ------------------------------------------------------------ inspector -- */

func (m Model) renderInspector() string {
	if m.diff != nil {
		return m.renderDiff()
	}
	if m.detail == nil {
		return styleFaint.Render(" Select a call: ↑↓ pick a channel, ←→ step along it, enter to read.")
	}

	d := m.detail
	var lines []string
	lines = append(lines, styleInk.Render(" "+d.Method+" "+truncate(d.Path, m.width-2)))

	// The protocol is only worth a column when it is not the obvious one:
	// "HTTP   HTTP 200" says the same thing twice.
	meta := []string{d.Target}
	if d.Protocol == "grpc" {
		meta = append(meta, "gRPC")
	}
	meta = append(meta, fmt.Sprintf("HTTP %d", d.Status), fmt.Sprintf("%.2fms", d.DurationMS))
	if d.ReplayOf != nil {
		meta = append(meta, fmt.Sprintf("replay of #%d", *d.ReplayOf))
	}
	lines = append(lines, styleFaint.Render(" "+strings.Join(meta, "   ")))

	if d.GRPCStatusText != "" {
		text := fmt.Sprintf(" gRPC %d %s", *d.GRPCStatus, d.GRPCStatusText)
		if d.GRPCMessage != "" {
			text += " — " + d.GRPCMessage
		}
		style := styleDim
		if *d.GRPCStatus != 0 {
			style = styleFault
		}
		lines = append(lines, style.Render(truncate(text, m.width-1)))
	}
	if d.Error != "" {
		lines = append(lines, styleFault.Render(" "+truncate(d.Error, m.width-2)))
	}

	if d.GRPC != nil {
		lines = append(lines, m.renderGRPC(d.GRPC)...)
	} else {
		lines = append(lines, m.renderBody("REQUEST", d.Request)...)
		lines = append(lines, m.renderBody("RESPONSE", d.Response)...)
	}

	return strings.Join(clamp(lines, inspectorLines), "\n")
}

func (m Model) renderGRPC(g *GRPCView) []string {
	source := "no schema"
	if g.Schema.Source != "" {
		source = "schema from " + strings.ReplaceAll(g.Schema.Source, "_", " ")
	}
	lines := []string{styleLabel.Render(" "+g.Service+" / "+g.Method) + "   " + styleFaint.Render(source)}
	if g.Schema.Error != "" {
		lines = append(lines, styleFaint.Render(" "+truncate(g.Schema.Error, m.width-2)))
	}

	for _, side := range []struct {
		label    string
		messages []GRPCMessage
	}{{"REQUEST", g.Request}, {"RESPONSE", g.Response}} {
		lines = append(lines, styleLabel.Render(" "+side.label)+styleFaint.Render(
			fmt.Sprintf("  %d message(s)", len(side.messages))))
		for _, msg := range side.messages {
			if len(msg.JSON) == 0 {
				lines = append(lines, styleFaint.Render("   "+orDefault(msg.Error, "not decoded")))
				continue
			}
			lines = append(lines, indent(compactJSON(msg.JSON), "   ", m.width)...)
		}
	}
	return lines
}

func (m Model) renderBody(label string, msg Message) []string {
	head := styleLabel.Render(" "+label) + styleFaint.Render(fmt.Sprintf("  %d B", msg.Size))
	if msg.Truncated {
		head += styleFaint.Render(fmt.Sprintf(" · stored %d B", msg.Stored))
	}
	lines := []string{head}

	switch {
	case msg.Text != "":
		lines = append(lines, indent(msg.Text, "   ", m.width)...)
	case msg.Base64 != "":
		lines = append(lines, styleFaint.Render("   not valid UTF-8; kept as raw bytes"))
	default:
		lines = append(lines, styleFaint.Render("   no body"))
	}
	return lines
}

/* ----------------------------------------------------------------- diff -- */

func (m Model) renderDiff() string {
	lines := []string{styleLabel.Render(" DIFF") + styleFaint.Render("   a is red, b is green")}

	if len(m.metadataChanges()) == 0 {
		lines = append(lines, styleDim.Render(" Same outcome."))
	} else {
		lines = append(lines, m.renderChanges(m.metadataChanges())...)
	}

	for _, side := range []struct {
		label string
		diff  SideDiff
	}{{"REQUEST", m.diff.Request}, {"RESPONSE", m.diff.Response}} {
		lines = append(lines, styleLabel.Render(" "+side.label))
		switch {
		case !side.diff.Comparable:
			lines = append(lines, styleFaint.Render("   "+side.diff.Reason))
		case side.diff.Identical && len(side.diff.Messages) == 0:
			lines = append(lines, styleDim.Render("   Identical."))
		default:
			lines = append(lines, m.renderChanges(side.diff.Changes)...)
			for _, msg := range side.diff.Messages {
				if msg.Identical {
					lines = append(lines, styleDim.Render(fmt.Sprintf("   #%d identical", msg.Index)))
					continue
				}
				if !msg.Comparable {
					lines = append(lines, styleFaint.Render("   "+msg.Reason))
					continue
				}
				lines = append(lines, m.renderChanges(msg.Changes)...)
			}
		}
	}
	return strings.Join(clamp(lines, inspectorLines), "\n")
}

func (m Model) metadataChanges() []Change { return m.diff.Metadata }

func (m Model) renderChanges(changes []Change) []string {
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		mark, style := "~", styleDim
		switch c.Kind {
		case "added":
			mark, style = "+", styleArmed
		case "removed":
			mark, style = "-", styleFault
		}
		line := "   " + style.Render(mark) + " " + styleInk.Render(c.Path)
		if c.Kind != "added" {
			line += "  " + styleFault.Render(format(c.A))
		}
		if c.Kind != "removed" {
			line += "  " + styleArmed.Render(format(c.B))
		}
		lines = append(lines, truncate(line, m.width+40))
	}
	return lines
}

/* --------------------------------------------------------------- footer -- */

func (m Model) renderFooter() string {
	if m.focus == focusSearch {
		return styleLabel.Render(" FIND ") + m.search.View() +
			styleFaint.Render("   enter apply · esc clear")
	}
	if m.status != "" {
		style := styleDim
		if m.statusErr {
			style = styleFault
		}
		return style.Render(" " + truncate(m.status, m.width-2))
	}

	// Keys read in order of use and are dropped from the far end when the
	// terminal is narrow — but quit is re-appended every time. A legend that
	// silently loses the way out is worse than a short one.
	keys := []string{
		"↑↓ chan", "←→ call", "⏎ read", "r replay", "d diff",
		"f faults", "w window", "h hold", "/ find",
	}
	prefix := " "
	if search := m.search.Value(); search != "" {
		prefix = " " + styleDim.Render("find: "+search) + "  ·  "
	}

	for {
		line := prefix + styleFaint.Render(strings.Join(append(keys, "q quit"), "  ·  "))
		if lipgloss.Width(line) <= m.width || len(keys) == 0 {
			return truncate(line, m.width)
		}
		keys = keys[:len(keys)-1]
	}
}

/* --------------------------------------------------------------- pieces -- */

func pad(s string, width int) string {
	if len(s) >= width {
		return s[:max(width-1, 0)] + " "
	}
	return s + strings.Repeat(" ", width-len(s))
}

func truncate(s string, width int) string {
	if width <= 1 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

func indent(text, prefix string, width int) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		out = append(out, styleDim.Render(truncate(prefix+line, width)))
	}
	return out
}

// clamp keeps the inspector from pushing the footer off the screen. The note
// says how much was left out rather than trailing off silently.
func clamp(lines []string, limit int) []string {
	if len(lines) <= limit {
		return lines
	}
	out := append([]string{}, lines[:limit-1]...)
	return append(out, styleFaint.Render(fmt.Sprintf("   … %d more lines", len(lines)-limit+1)))
}

func compactJSON(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}

func format(v any) string {
	if v == nil {
		return "(absent)"
	}
	if s, ok := v.(string); ok {
		return s
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(encoded)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

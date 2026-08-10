package tui

import (
	"encoding/json"
	"fmt"
	"sort"
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
	if m.drift != nil {
		return m.renderDrift()
	}
	if m.trace != nil {
		return m.renderTrace()
	}
	if m.diff != nil {
		return m.renderDiff()
	}
	if m.detail == nil {
		return styleFaint.Render(" Select a call: ↑↓ pick a channel, ←→ step along it, enter to read.")
	}

	d := m.detail
	var lines []string
	lines = append(lines, styleInk.Render(" "+truncate(d.Label(), m.width-1)))

	// The protocol is only worth a column when it is not the obvious one:
	// "HTTP   HTTP 200" says the same thing twice.
	meta := []string{d.Target}
	if d.Protocol == "grpc" {
		meta = append(meta, "gRPC")
	}
	if d.Protocol == "postgres" {
		// A session has no HTTP status. "HTTP 0" would be a reading of
		// something that was never measured.
		meta = append(meta, "POSTGRES")
	} else {
		meta = append(meta, fmt.Sprintf("HTTP %d", d.Status))
	}
	meta = append(meta, fmt.Sprintf("%.2fms", d.DurationMS))
	if d.ReplayOf != nil {
		meta = append(meta, fmt.Sprintf("replay of #%d", *d.ReplayOf))
	}
	lines = append(lines, styleFaint.Render(" "+strings.Join(meta, "   ")))

	// Stated before the payload, for the same reason the web client states it:
	// everything below is a reading of something recorded earlier, and a reader
	// who scrolls straight to the body would take it for what just happened.
	// Sonda's own interference, said before the payload for the same reason the
	// web client says it: a reader who takes it for the service's failure
	// spends an hour on a bug that is not there.
	if d.Injected {
		lines = append(lines, styleFault.Render(
			" BROKEN ON PURPOSE · "+truncate(orDefault(d.Error, "Sonda injected this failure."), m.width-24)))
	}

	if d.StubOf != nil {
		lines = append(lines, styleInk.Render(
			fmt.Sprintf(" FROM RECORDING · the service was not called. Answered from capture #%d", *d.StubOf)))
	}

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
	// Said in the header and not only in the payload below: this call answered
	// HTTP 200, and a reader who takes the status line at its word closes the
	// inspector believing it worked.
	if d.GraphQLErrors > 0 {
		lines = append(lines, styleFault.Render(truncate(
			fmt.Sprintf(" GRAPHQL · %d error(s) under HTTP %d", d.GraphQLErrors, d.Status), m.width-1)))
	}
	// Same reason again: nothing about a Postgres session's transport says it
	// failed, so the header has to.
	if d.PostgresErrors > 0 {
		lines = append(lines, styleFault.Render(truncate(
			fmt.Sprintf(" POSTGRES · %d server error(s)", d.PostgresErrors), m.width-1)))
	}
	if d.Error != "" {
		lines = append(lines, styleFault.Render(" "+truncate(d.Error, m.width-2)))
	}

	switch {
	case d.GraphQL != nil:
		lines = append(lines, m.renderGraphQL(d.GraphQL)...)
	case d.Socket != nil:
		lines = append(lines, m.renderFrames("SENT", d.Socket.Sent, d.Socket.SentSummary)...)
		lines = append(lines, m.renderFrames("RECEIVED", d.Socket.Received, d.Socket.ReceivedSummary)...)
	case d.Postgres != nil:
		lines = append(lines, m.renderPostgres("SENT", d.Postgres.Sent, d.Postgres.SentIncomplete)...)
		lines = append(lines, m.renderPostgres("RECEIVED", d.Postgres.Received, d.Postgres.ReceivedIncomplete)...)
	case d.Stream != nil:
		lines = append(lines, m.renderEvents(d.Stream)...)
	case d.GRPC != nil:
		lines = append(lines, m.renderGRPC(d.GRPC)...)
	default:
		lines = append(lines, m.renderBody("REQUEST", d.Request)...)
		lines = append(lines, m.renderBody("RESPONSE", d.Response)...)
	}

	return strings.Join(clamp(lines, inspectorLines), "\n")
}

// renderFrames shows one direction of a socket conversation.
func (m Model) renderFrames(title string, frames []FrameView, summary string) []string {
	if summary == "" {
		summary = "no frames"
	}
	lines := []string{"", styleLabel.Render(" "+title) + styleFaint.Render("   "+summary)}
	if len(frames) == 0 {
		return append(lines, styleFaint.Render("  nothing in this direction"))
	}

	for _, f := range frames {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("  %s  %d B", strings.ToUpper(f.Kind), f.Size)))
		switch {
		case f.Kind == "close":
			// Why the socket ended is usually why someone is reading this.
			why := "closed with no code"
			if f.CloseCode != 0 {
				why = fmt.Sprintf("code %d", f.CloseCode)
				if f.CloseReason != "" {
					why += " — " + f.CloseReason
				}
			}
			lines = append(lines, styleDim.Render("  "+truncate(why, m.width-3)))
		case f.Text != "":
			lines = append(lines, indent(f.Text, "  ", m.width-3)...)
		case f.Size > 0:
			lines = append(lines, styleFaint.Render("  not text"))
		}
	}
	return lines
}

// renderPostgres shows one direction of a session as the messages it carried.
//
// A session is mostly DataRows, and printing every one of them would bury the
// statement that produced them. Rows are counted instead, and the messages that
// say something a reader came for — the SQL, its parameters, the command tag,
// the server's error — are printed.
func (m Model) renderPostgres(title string, msgs []PGMessage, incomplete bool) []string {
	lines := []string{"", styleLabel.Render(" "+title) +
		styleFaint.Render(fmt.Sprintf("   %d message(s)", len(msgs)))}
	if len(msgs) == 0 {
		return append(lines, styleFaint.Render("  nothing in this direction"))
	}

	rows := 0
	flushRows := func() {
		if rows > 0 {
			lines = append(lines, styleFaint.Render(fmt.Sprintf("  %d data row(s)", rows)))
			rows = 0
		}
	}

	for _, msg := range msgs {
		if msg.Kind == "data_row" {
			rows++
			continue
		}
		flushRows()

		// The statement and its parameters arrive in different messages —
		// Parse carries the SQL and Bind carries the values — so they are
		// printed independently. Hanging the parameters off the SQL would drop
		// every one of them for the extended protocol, which is what every ORM
		// uses.
		head := styleFaint.Render("  " + strings.ToUpper(msg.Kind))
		printed := false
		if msg.SQL != "" {
			lines = append(lines, head)
			lines = append(lines, indent(msg.SQL, "  ", m.width-3)...)
			printed = true
		}
		if len(msg.Params) > 0 {
			if !printed {
				lines = append(lines, head)
			}
			for i, p := range msg.Params {
				lines = append(lines, styleDim.Render(fmt.Sprintf("   $%d = %s", i+1, pgValue(p))))
			}
			printed = true
		}
		if printed {
			continue
		}

		switch {
		case msg.Kind == "error_response" || msg.Kind == "notice_response":
			head := strings.TrimSpace(orDefault(msg.Severity, strings.ToUpper(msg.Kind)) + " " + msg.Code)
			style := styleFault
			if msg.Kind == "notice_response" {
				style = styleDim
			}
			lines = append(lines, style.Render("  "+truncate(head+" — "+msg.Message, m.width-3)))
			for _, extra := range []string{msg.Detail, msg.Hint} {
				if extra != "" {
					lines = append(lines, styleFaint.Render("   "+truncate(extra, m.width-4)))
				}
			}
		case msg.Kind == "command_complete":
			lines = append(lines, styleInk.Render("  "+truncate(msg.Tag, m.width-3)))
		case msg.Kind == "startup":
			lines = append(lines, styleFaint.Render("  STARTUP   "+truncate(pgParameters(msg.Parameters), m.width-13)))
		case msg.Auth != "":
			// Which mechanism, never the exchange: the bytes that carried it
			// were blanked before anything was stored.
			lines = append(lines, styleFaint.Render("  AUTHENTICATION   "+msg.Auth))
		case msg.Encrypted:
			lines = append(lines, styleFaint.Render("  "+truncate(msg.Note, m.width-3)))
		default:
			lines = append(lines, styleFaint.Render(fmt.Sprintf("  %s  %d B", strings.ToUpper(msg.Kind), msg.Size)))
		}
	}
	flushRows()

	if incomplete {
		lines = append(lines, styleFaint.Render("  bytes remain after the last whole message: cut by the body cap, or still open"))
	}
	return lines
}

// pgValue renders a bind parameter. A NULL and an empty string are different on
// the wire and different in a WHERE clause, so they read differently here.
func pgValue(v PGValue) string {
	switch {
	case v.Null:
		return "NULL"
	case v.Text != "":
		return v.Text
	case v.Size == 0:
		return "''"
	}
	return fmt.Sprintf("%d B, not text", v.Size)
}

// pgParameters puts the connection's declared parameters in a stable order, so
// two sessions to the same database read the same way.
func pgParameters(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, " ")
}

// renderEvents shows a server-sent event stream as the events it carried.
func (m Model) renderEvents(v *StreamView) []string {
	lines := []string{"", styleLabel.Render(" EVENTS") +
		styleFaint.Render(fmt.Sprintf("   %d", len(v.Events)))}

	for _, e := range v.Events {
		name := e.Name
		if name == "" {
			name = "message"
		}
		head := "  " + strings.ToUpper(name)
		if e.ID != "" {
			head += "   id " + e.ID
		}
		lines = append(lines, styleFaint.Render(head))
		if e.Data != "" {
			lines = append(lines, indent(e.Data, "  ", m.width-3)...)
		}
	}
	if v.Incomplete {
		lines = append(lines, styleFaint.Render("  the last event has no terminator: cut by the body cap, or still open"))
	}
	return lines
}

// renderGraphQL shows a GraphQL POST as the operations it actually carried.
//
// The request body is one long string with the document escaped into it, which
// is unreadable and identical on every call to the endpoint. What is worth the
// space is the operation, what it asked for, and what came back wrong.
func (m Model) renderGraphQL(v *GraphQLView) []string {
	head := " GRAPHQL"
	if v.Batch {
		head += fmt.Sprintf("   batch of %d", len(v.Operations))
	}
	lines := []string{"", styleLabel.Render(head)}

	for _, op := range v.Operations {
		lines = append(lines, styleInk.Render("  "+truncate(op.Label, m.width-3)))
		if len(op.Fields) > 0 {
			lines = append(lines, styleFaint.Render(
				"   asks for "+truncate(strings.Join(op.Fields, ", "), m.width-14)))
		}
		if len(op.Variables) > 0 {
			lines = append(lines, indent(compactJSON(op.Variables), "   ", m.width)...)
		}
		for _, e := range op.Errors {
			where := e.Path
			if e.Code != "" {
				where = strings.TrimPrefix(where+" "+e.Code, " ")
			}
			lines = append(lines, styleFault.Render("   "+truncate(e.Message, m.width-4)))
			if where != "" {
				lines = append(lines, styleFaint.Render("   at "+truncate(where, m.width-7)))
			}
		}
	}

	// The gap is reported rather than read as "no errors": a body cut by the
	// cap cannot say whether the call worked.
	if v.Unreadable {
		lines = append(lines, styleFaint.Render(
			"  the response is not JSON, so whether it carried errors is unknown"))
	}
	return lines
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

// renderDrift shows whether this endpoint still answers the shape it used to.
// The report arrives already drawn from the API, so the terminal and the agent
// read the same one.
func (m Model) renderDrift() string {
	d := m.drift
	lines := []string{styleInk.Render(fmt.Sprintf(" CONTRACT   vs capture #%d", d.Baseline))}

	for _, line := range strings.Split(strings.TrimRight(d.Rendered, "\n"), "\n") {
		style := styleDim
		// Losing a field or changing its type is what takes a caller down.
		// Adding one is safe, and colouring both the same would hide which is
		// which.
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "~") {
			style = styleFault
		}
		lines = append(lines, style.Render(" "+truncate(line, m.width-2)))
	}

	verdict := "all additive — nothing here breaks a caller"
	if n := len(d.Breaking); n > 0 {
		verdict = fmt.Sprintf("%d of these would break a caller", n)
	}
	lines = append(lines, "", styleFaint.Render(" "+verdict), styleFaint.Render(" esc to go back"))
	return strings.Join(lines, "\n") + "\n"
}

// renderTrace shows the whole request a call belonged to. The drawing arrives
// already made from the API, so the terminal and the agent read the same one.
func (m Model) renderTrace() string {
	t := m.trace
	head := fmt.Sprintf(" CALL TREE   %d calls", t.Tree.Calls)
	if t.Tree.Failed > 0 {
		head += fmt.Sprintf(" · %d failed", t.Tree.Failed)
	}

	lines := []string{styleInk.Render(head)}
	for _, line := range strings.Split(strings.TrimRight(t.Rendered, "\n"), "\n") {
		style := styleDim
		switch {
		// The same rule as everywhere: the failure is why the tool is open.
		case strings.Contains(line, "✗"):
			style = styleFault
		case strings.HasPrefix(line, "(grouped by timing"):
			style = styleFaint
		}
		lines = append(lines, style.Render(" "+truncate(line, m.width-2)))
	}
	lines = append(lines, "", styleFaint.Render(" esc to go back"))
	return strings.Join(lines, "\n") + "\n"
}

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
		"↑↓ chan", "←→ call", "⏎ read", "t tree", "c contract", "r replay", "d diff",
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

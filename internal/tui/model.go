package tui

import (
	"context"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// Windows are the sweeps, each with the number of divisions that puts every
// axis tick on a round figure.
var windows = []struct {
	Span      time.Duration
	Divisions int
	Label     string
}{
	{time.Minute, 4, "1M"},
	{5 * time.Minute, 5, "5M"},
	{30 * time.Minute, 6, "30M"},
}

const (
	// The trace advances on this beat. Fast enough to read as motion, slow
	// enough that an idle terminal is not burning a core to redraw dots.
	tickInterval = 500 * time.Millisecond

	// A periodic reload catches what the stream could not: retention removing
	// old rows, and payload matches for a search the terminal cannot evaluate
	// on a summary.
	reloadInterval = 5 * time.Second

	pageLimit = 1000
)

type focus int

const (
	focusField focus = iota
	focusSearch
)

type Model struct {
	client *Client
	ctx    context.Context

	width, height int
	ready         bool

	targets []Target
	stats   Stats
	calls   []Call
	detail  *CallDetail
	diff    *Diff
	trace   *Trace
	drift   *Drift

	// diag is held only while nothing has been captured, which is the only time
	// it is on screen. probes keeps the last upstream dial separately, with the
	// clock time it was taken at: the refresh that follows carries no probe, and
	// redrawing an old answer as though it were current would be the instrument
	// lying about a measurement it did not make.
	diag     *Diagnosis
	probes   map[string]string
	probedAt time.Time

	failedOnly bool
	windowIdx  int
	search     textinput.Model
	focus      focus
	held       bool

	// Pick the trace, then move the cursor along it — the way you point at
	// something on an instrument. The cursor holds a call id, not a column,
	// because the trace advances and a column would drift onto its neighbour.
	lane     int
	selected int64

	status    string
	statusErr bool
	live      bool
	err       error

	events <-chan Call
	now    time.Time
}

func New(ctx context.Context, client *Client, events <-chan Call) Model {
	input := textinput.New()
	input.Placeholder = "path, id, payload text"
	input.Prompt = ""
	input.CharLimit = 120

	return Model{
		client: client,
		ctx:    ctx,
		// Faults first: it is why the tool gets opened.
		failedOnly: true,
		windowIdx:  1,
		search:     input,
		events:     events,
		now:        time.Now(),
	}
}

func (m Model) window() time.Duration { return windows[m.windowIdx].Span }
func (m Model) divisions() int        { return windows[m.windowIdx].Divisions }

/* ------------------------------------------------------------- messages -- */

type tickMsg time.Time
type reloadMsg time.Time
type loadedMsg struct {
	calls []Call
	stats Stats
	err   error
}
type targetsMsg struct {
	targets []Target
	err     error
}
type detailMsg struct {
	detail *CallDetail
	err    error
}
type driftMsg struct {
	drift *Drift
	err   error
}

type traceMsg struct {
	trace *Trace
	err   error
}

type diffMsg struct {
	diff *Diff
	err  error
}
type replayMsg struct {
	result *ReplayResult
	err    error
}
type diagnoseMsg struct {
	diag  *Diagnosis
	probe bool
	err   error
}
type streamMsg Call

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadTargets(), m.load(), tick(), reloadEvery(), m.waitForEvent())
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func reloadEvery() tea.Cmd {
	return tea.Tick(reloadInterval, func(t time.Time) tea.Msg { return reloadMsg(t) })
}

func (m Model) waitForEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case call, ok := <-m.events:
			if !ok {
				return nil
			}
			return streamMsg(call)
		case <-m.ctx.Done():
			return nil
		}
	}
}

func (m Model) loadTargets() tea.Cmd {
	return func() tea.Msg {
		targets, err := m.client.Targets(m.ctx)
		return targetsMsg{targets: targets, err: err}
	}
}

func (m Model) load() tea.Cmd {
	failed, search, window := m.failedOnly, m.search.Value(), m.window()
	return func() tea.Msg {
		calls, err := m.client.Calls(m.ctx, failed, search, window, pageLimit)
		if err != nil {
			return loadedMsg{err: err}
		}
		stats, err := m.client.Stats(m.ctx)
		return loadedMsg{calls: calls, stats: stats, err: err}
	}
}

func (m Model) loadDetail(id int64) tea.Cmd {
	return func() tea.Msg {
		detail, err := m.client.Detail(m.ctx, id)
		return detailMsg{detail: detail, err: err}
	}
}

func (m Model) loadDrift(call Call) tea.Cmd {
	return func() tea.Msg {
		d, err := m.client.Drift(m.ctx, call)
		return driftMsg{drift: d, err: err}
	}
}

func (m Model) loadTrace(id int64) tea.Cmd {
	return func() tea.Msg {
		trace, err := m.client.Trace(m.ctx, id)
		return traceMsg{trace: trace, err: err}
	}
}

func (m Model) loadDiff(a, b int64) tea.Cmd {
	return func() tea.Msg {
		diff, err := m.client.Diff(m.ctx, a, b)
		return diffMsg{diff: diff, err: err}
	}
}

// loadDiagnosis asks why nothing is being captured. probe is passed only by the
// key that asks for it, never by the periodic refresh.
func (m Model) loadDiagnosis(probe bool) tea.Cmd {
	return func() tea.Msg {
		d, err := m.client.Diagnose(m.ctx, probe)
		return diagnoseMsg{diag: d, probe: probe, err: err}
	}
}

func (m Model) replay(id int64) tea.Cmd {
	return func() tea.Msg {
		result, err := m.client.Replay(m.ctx, id)
		return replayMsg{result: result, err: err}
	}
}

/* --------------------------------------------------------------- update -- */

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		return m, nil

	case tickMsg:
		if !m.held {
			m.now = time.Time(msg)
		}
		return m, tick()

	case reloadMsg:
		if m.held {
			return m, reloadEvery()
		}
		return m, tea.Batch(m.load(), reloadEvery())

	case streamMsg:
		call := Call(msg)
		if !m.held && m.admits(call) {
			m.calls = append(windowCalls(m.calls, m.now), call)
			m.stats.Calls++
			m.bumpTarget(call)
		}
		return m, m.waitForEvent()

	case targetsMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.targets = msg.targets
		m.err = nil
		return m, nil

	case loadedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.live = false
			return m, nil
		}
		// Oldest first, so appending a streamed call keeps the order.
		sort.Slice(msg.calls, func(i, j int) bool { return msg.calls[i].ID < msg.calls[j].ID })
		m.calls, m.stats, m.err, m.live = msg.calls, msg.stats, nil, true
		if m.stats.Calls > 0 {
			// Traffic is arriving; the question the diagnosis answers is no
			// longer being asked, and a stale one on screen would be worse than
			// none.
			m.diag, m.probes = nil, nil
			return m, nil
		}
		return m, m.loadDiagnosis(false)

	case diagnoseMsg:
		if msg.err != nil {
			// The reconnect lamp already says the API is unreachable, and a
			// second complaint in the footer would push out the status the user
			// is actually acting on.
			return m, nil
		}
		m.diag = msg.diag
		if msg.probe {
			m.probes, m.probedAt = map[string]string{}, time.Now()
			for _, svc := range msg.diag.Services {
				m.probes[svc.Service] = probeLine(svc)
			}
			m.status = ""
		}
		return m, nil

	case detailMsg:
		if msg.err != nil {
			return m.withError(msg.err), nil
		}
		m.detail, m.diff = msg.detail, nil
		return m, nil

	case driftMsg:
		if msg.err != nil {
			// Fewer than two captures, or a response that is not JSON. Neither
			// is a failure; there is simply nothing to compare.
			m.status = "nothing to compare this endpoint against yet"
			return m, nil
		}
		m.drift, m.trace, m.diff, m.status = msg.drift, nil, nil, ""
		return m, nil

	case traceMsg:
		if msg.err != nil {
			m.status = "could not arrange the request: " + msg.err.Error()
			return m, nil
		}
		if msg.trace == nil || msg.trace.Tree.Calls < 2 {
			// One call is not a tree, and saying so beats drawing a stump.
			m.status = "this call was on its own — nothing else belonged to the same request"
			return m, nil
		}
		m.trace, m.diff, m.status = msg.trace, nil, ""
		return m, nil

	case diffMsg:
		if msg.err != nil {
			return m.withError(msg.err), nil
		}
		m.diff = msg.diff
		return m, nil

	case replayMsg:
		if msg.err != nil {
			// The server's refusal explains itself, so it is shown as written.
			return m.withError(msg.err), nil
		}
		m.status = "replayed onto " + msg.result.SentTo
		m.statusErr = false
		return m, m.load()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) withError(err error) Model {
	m.status = err.Error()
	m.statusErr = true
	return m
}

func (m Model) admits(call Call) bool {
	if m.failedOnly && !call.Fault() {
		return false
	}
	// A search matches payload text the terminal never received, so a live
	// event can only be judged on its path. The periodic reload is what makes
	// the rest appear.
	return matchesPath(call.Path, m.search.Value())
}

func (m *Model) bumpTarget(call Call) {
	for i := range m.stats.ByTarget {
		if m.stats.ByTarget[i].Target == call.Target {
			m.stats.ByTarget[i].Calls++
			if call.Fault() {
				m.stats.ByTarget[i].Faults++
			}
			return
		}
	}
	stat := TargetStat{Target: call.Target, Calls: 1}
	if call.Fault() {
		stat.Faults = 1
	}
	m.stats.ByTarget = append(m.stats.ByTarget, stat)
}

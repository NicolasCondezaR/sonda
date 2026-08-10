package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.focus == focusSearch {
		return m.handleSearchKey(msg)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "f":
		m.failedOnly = !m.failedOnly
		m.clearSelection()
		return m, m.load()

	case "w":
		m.windowIdx = (m.windowIdx + 1) % len(windows)
		m.clearSelection()
		return m, m.load()

	case "h":
		// Holding the trace is what makes a moving target selectable, the same
		// reason the web client freezes the field under the pointer.
		m.held = !m.held
		m.status = ""
		return m, nil

	case "/":
		m.focus = focusSearch
		m.search.Focus()
		return m, nil

	case "up", "k":
		if m.lane > 0 {
			m.lane--
			m.detail, m.diff = nil, nil
		}
		return m, nil

	case "down", "j":
		if m.lane < len(m.targets)-1 {
			m.lane++
			m.detail, m.diff = nil, nil
		}
		return m, nil

	case "left", "H":
		return m.step(-1)

	case "right", "L":
		return m.step(1)

	case "home", "g":
		// Oldest call on this lane.
		return m.selectIndex(0)

	case "end", "G":
		return m.selectIndex(-1)

	case "enter":
		if call, ok := m.selectedCall(); ok {
			return m, m.loadDetail(call.ID)
		}
		return m, nil

	case "r":
		// Neither a socket nor a database statement is a request that can be
		// sent again: replaying one opens a new connection rather than
		// repeating the one being read.
		if call, ok := m.selectedCall(); ok {
			switch call.Protocol {
			case "websocket":
				m.status = "a socket cannot be replayed — the handshake would open a new conversation"
				return m, nil
			case "postgres":
				m.status = "a statement cannot be replayed — it belongs to a connection that is gone"
				return m, nil
			}
		}

		call, ok := m.selectedCall()
		if !ok {
			return m.withError(errNoSelection), nil
		}
		m.status = "replaying…"
		m.statusErr = false
		return m, m.replay(call.ID)

	case "d":
		call, ok := m.selectedCall()
		if !ok || call.ReplayOf == nil {
			return m.withError(errNoOriginal), nil
		}
		return m, m.loadDiff(*call.ReplayOf, call.ID)

	case "c":
		call, ok := m.selectedCall()
		if !ok {
			return m, nil
		}
		return m, m.loadDrift(call)

	case "t":
		call, ok := m.selectedCall()
		if !ok {
			return m, nil
		}
		return m, m.loadTrace(call.ID)

	case "esc":
		m.detail, m.diff, m.trace, m.drift, m.status = nil, nil, nil, nil, ""
		return m, nil
	}

	return m, nil
}

func (m Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.focus = focusField
		m.search.Blur()
		m.search.SetValue("")
		m.clearSelection()
		return m, m.load()

	case "enter":
		m.focus = focusField
		m.search.Blur()
		m.clearSelection()
		return m, m.load()
	}

	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	return m, cmd
}

// step moves the cursor along the selected lane, call by call rather than cell
// by cell: an empty cell is not something to point at, and stepping through
// dozens of them to reach the next event is not navigation.
func (m Model) step(direction int) (tea.Model, tea.Cmd) {
	calls := m.laneCalls(m.lane)
	if len(calls) == 0 {
		return m, nil
	}

	current := m.selectedIndex(calls)
	next := current + direction
	switch {
	case current < 0:
		// Nothing selected yet: start at the newest going left, oldest going right.
		if direction < 0 {
			next = len(calls) - 1
		} else {
			next = 0
		}
	case next < 0:
		next = 0
	case next >= len(calls):
		next = len(calls) - 1
	}

	m.selected = calls[next].ID
	m.detail, m.diff = nil, nil
	return m, m.loadDetail(calls[next].ID)
}

func (m Model) selectIndex(index int) (tea.Model, tea.Cmd) {
	calls := m.laneCalls(m.lane)
	if len(calls) == 0 {
		return m, nil
	}
	if index < 0 {
		index = len(calls) - 1
	}
	m.selected = calls[index].ID
	m.detail, m.diff = nil, nil
	return m, m.loadDetail(calls[index].ID)
}

func (m *Model) clearSelection() {
	m.selected = 0
	m.detail, m.diff = nil, nil
}

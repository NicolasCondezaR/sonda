package tui

import (
	"strings"
	"testing"
)

// The terminal has to show a socket and an event stream, not fall through to
// "REQUEST / RESPONSE" with a wall of framing bytes in it. These check the
// rendered text, which is the only thing a terminal actually produces.

func rendered(lines []string) string { return strings.Join(lines, "\n") }

func TestTheTerminalShowsSocketFrames(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderFrames("SENT", []FrameView{
		{Kind: "text", Size: 21, Text: `{"subscribe":"rates"}`},
		{Kind: "ping", Size: 0},
		{Kind: "close", Size: 18, CloseCode: 1011, CloseReason: "upstream is gone"},
	}, "2 text, 1 ping"))

	for _, want := range []string{"SENT", "2 text, 1 ping", "TEXT", "subscribe", "PING", "CLOSE", "1011", "upstream is gone"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, out)
		}
	}
}

// Why the socket ended is usually why someone is reading it, so a close with no
// code has to say that rather than show an empty line.
func TestACloseWithNoCodeSaysSo(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderFrames("RECEIVED", []FrameView{{Kind: "close"}}, "1 close"))
	if !strings.Contains(out, "no code") {
		t.Errorf("a bare close was rendered as nothing:\n%s", out)
	}
}

// A direction with nothing in it is a reading, not a blank.
func TestAnEmptyDirectionSaysNothingCrossed(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderFrames("SENT", nil, ""))
	if !strings.Contains(out, "nothing in this direction") {
		t.Errorf("an empty direction rendered as:\n%s", out)
	}
	if !strings.Contains(out, "no frames") {
		t.Errorf("the summary did not degrade to a reading:\n%s", out)
	}
}

// A binary payload has to be reported as binary rather than silently skipped.
func TestABinaryFrameIsReportedAsNotText(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderFrames("RECEIVED", []FrameView{{Kind: "binary", Size: 4}}, "1 binary"))
	if !strings.Contains(out, "not text") {
		t.Errorf("a binary frame left no trace:\n%s", out)
	}
}

func TestTheTerminalShowsEvents(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderEvents(&StreamView{Events: []EventView{
		{Name: "rate", ID: "7", Data: `{"usd":940}`},
		{Data: "sin nombre"},
	}}))

	for _, want := range []string{"EVENTS", "RATE", "id 7", "usd", "MESSAGE", "sin nombre"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, out)
		}
	}
}

// A stream cut mid-event is the ordinary case, and hiding it would be the gap
// the product forbids.
func TestAnIncompleteStreamSaysSoInTheTerminal(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderEvents(&StreamView{
		Events:     []EventView{{Data: "one"}},
		Incomplete: true,
	}))
	if !strings.Contains(out, "no terminator") {
		t.Errorf("a truncated stream rendered as complete:\n%s", out)
	}
}

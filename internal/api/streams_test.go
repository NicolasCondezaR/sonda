package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

func sse(body string, truncated bool) *store.Call {
	return &store.Call{Response: store.Message{
		Headers:   http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:      []byte(body),
		Truncated: truncated,
	}}
}

func TestAnEventStreamIsSplitIntoItsEvents(t *testing.T) {
	view := buildStreamView(sse("event: rate\ndata: {\"usd\":940}\nid: 7\n\nevent: rate\ndata: {\"usd\":941}\n\n", false))

	if len(view.Events) != 2 {
		t.Fatalf("%d events, want 2", len(view.Events))
	}
	if view.Events[0].Name != "rate" || view.Events[0].ID != "7" {
		t.Errorf("first event = %+v", view.Events[0])
	}
	if view.Events[0].Data != `{"usd":940}` {
		t.Errorf("data = %q", view.Events[0].Data)
	}
	if view.Incomplete {
		t.Error("a stream that ended on a blank line was reported as incomplete")
	}
}

// The format allows several data lines per event and they join with newlines.
// Reporting only the last one would quietly drop most of a multi-line payload.
func TestMultipleDataLinesJoin(t *testing.T) {
	view := buildStreamView(sse("data: line one\ndata: line two\n\n", false))
	if len(view.Events) != 1 {
		t.Fatalf("%d events", len(view.Events))
	}
	if view.Events[0].Data != "line one\nline two" {
		t.Errorf("data = %q", view.Events[0].Data)
	}
}

// A stream cut mid-event is the ordinary case: the body cap reached, or the
// stream still running. The partial chunk is evidence and hiding it would be
// the gap the product forbids.
func TestATruncatedStreamSaysSo(t *testing.T) {
	view := buildStreamView(sse("data: whole\n\ndata: cut off here", false))
	if !view.Incomplete {
		t.Error("a stream cut mid-event was not reported as incomplete")
	}
	if len(view.Events) != 2 {
		t.Fatalf("%d events, want the whole one and the partial one", len(view.Events))
	}
	if view.Events[1].Data != "cut off here" {
		t.Errorf("the partial event was lost: %+v", view.Events[1])
	}
}

// Comment lines are how a server keeps the connection alive, and they carry
// nothing a reader wants.
func TestKeepaliveCommentsAreNotEvents(t *testing.T) {
	view := buildStreamView(sse(": keepalive\n\ndata: real\n\n", false))
	if len(view.Events) != 1 {
		t.Fatalf("%d events, want only the real one: %+v", len(view.Events), view.Events)
	}
	if view.Events[0].Data != "real" {
		t.Errorf("data = %q", view.Events[0].Data)
	}
}

// Servers send CRLF as often as LF, and a parser that only knows one produces
// fields with a stray carriage return glued on.
func TestCRLFIsHandled(t *testing.T) {
	view := buildStreamView(sse("event: tick\r\ndata: 1\r\n\r\n", false))
	if len(view.Events) != 1 {
		t.Fatalf("%d events", len(view.Events))
	}
	if view.Events[0].Name != "tick" || view.Events[0].Data != "1" {
		t.Errorf("event = %+v", view.Events[0])
	}
}

func TestOnlyAnEventStreamGetsTheStreamView(t *testing.T) {
	json := &store.Call{Response: store.Message{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"ok":true}`),
	}}
	if isEventStream(json) {
		t.Error("a JSON response was read as an event stream")
	}
	if !isEventStream(sse("", false)) {
		t.Error("an event stream was not recognised")
	}
	// The header carries a charset in practice, which a strict equality check
	// would miss.
	withCharset := &store.Call{Response: store.Message{
		Headers: http.Header{"Content-Type": []string{"TEXT/EVENT-STREAM"}},
	}}
	if !isEventStream(withCharset) {
		t.Error("an uppercase content type was not recognised")
	}
}

func TestASocketViewReadsBothDirections(t *testing.T) {
	// One masked client frame and one unmasked server frame, built the way the
	// wire really carries them.
	client := []byte{0x81, 0x83, 0x01, 0x02, 0x03, 0x04, 'a' ^ 1, 'b' ^ 2, 'c' ^ 3}
	server := []byte{0x81, 0x02, 'o', 'k'}

	view := buildSocketView(&store.Call{
		Request:  store.Message{Body: client},
		Response: store.Message{Body: server},
	})

	if len(view.Sent) != 1 || view.Sent[0].Text != "abc" {
		t.Errorf("sent = %+v, want the unmasked payload", view.Sent)
	}
	if len(view.Received) != 1 || view.Received[0].Text != "ok" {
		t.Errorf("received = %+v", view.Received)
	}
	if !strings.Contains(view.SentSummary, "1 text") {
		t.Errorf("summary = %q", view.SentSummary)
	}
	if view.SentIncomplete || view.ReceivedIncomplete {
		t.Error("whole frames were reported as incomplete")
	}
}

// A conversation cut by the body cap is normal, and the view has to say which
// direction was cut rather than showing fewer frames without explanation.
func TestASocketCutMidFrameSaysSo(t *testing.T) {
	view := buildSocketView(&store.Call{
		Response: store.Message{Body: []byte{0x81, 0x05, 'h', 'i'}}, // claims 5, has 2
	})
	if !view.ReceivedIncomplete {
		t.Error("a frame cut in half was not reported as incomplete")
	}
	if len(view.Received) != 0 {
		t.Errorf("a half frame was reported as whole: %+v", view.Received)
	}
}

// A binary payload still has to be visible as something.
func TestABinaryFrameGoesOutAsBytes(t *testing.T) {
	view := buildSocketView(&store.Call{
		Response: store.Message{Body: []byte{0x82, 0x02, 0xff, 0xfe}},
	})
	if len(view.Received) != 1 {
		t.Fatalf("%d frames", len(view.Received))
	}
	if view.Received[0].Text != "" {
		t.Errorf("binary was claimed as text: %q", view.Received[0].Text)
	}
	if view.Received[0].Base64 == "" {
		t.Error("a binary frame came back with nothing to show")
	}
}

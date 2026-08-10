package api

import (
	"encoding/base64"
	"strings"

	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/wsframe"
)

// A socket and an event stream are both one exchange carrying many messages, so
// they are read back the same way gRPC streaming is: the raw stream is what was
// stored, and the messages are a view computed here when someone looks.

type socketView struct {
	Sent     []frameView `json:"sent"`
	Received []frameView `json:"received"`

	// Summary is the one line a listing has room for: how the conversation went
	// without any of it.
	SentSummary     string `json:"sent_summary"`
	ReceivedSummary string `json:"received_summary"`

	// Incomplete reports bytes left after the last whole frame — the capture
	// was cut by the body cap, or the socket was still open when it was read.
	SentIncomplete     bool `json:"sent_incomplete"`
	ReceivedIncomplete bool `json:"received_incomplete"`
}

type frameView struct {
	Kind        string `json:"kind"`
	Final       bool   `json:"final"`
	Size        int64  `json:"size"`
	Text        string `json:"text,omitempty"`
	Base64      string `json:"base64,omitempty"`
	CloseCode   int    `json:"close_code,omitempty"`
	CloseReason string `json:"close_reason,omitempty"`
}

func buildSocketView(c *store.Call) *socketView {
	sent, sentRest := wsframe.Deframe(c.Request.Body)
	received, receivedRest := wsframe.Deframe(c.Response.Body)

	return &socketView{
		Sent:               toFrames(sent),
		Received:           toFrames(received),
		SentSummary:        wsframe.Summarise(sent),
		ReceivedSummary:    wsframe.Summarise(received),
		SentIncomplete:     sentRest > 0,
		ReceivedIncomplete: receivedRest > 0,
	}
}

func toFrames(frames []wsframe.Frame) []frameView {
	out := make([]frameView, 0, len(frames))
	for _, f := range frames {
		view := frameView{
			Kind: f.Kind, Final: f.Final, Size: f.Size,
			Text: f.Text, CloseCode: f.CloseCode, CloseReason: f.CloseReason,
		}
		// A payload that is not text still has to be visible as something, so
		// it goes out as bytes rather than as an empty field.
		if f.Text == "" && len(f.Payload) > 0 {
			view.Base64 = base64.StdEncoding.EncodeToString(f.Payload)
		}
		out = append(out, view)
	}
	return out
}

// An event stream is a response whose body never ends, so what was stored is a
// run of events rather than one document.
type streamView struct {
	Events     []eventView `json:"events"`
	Incomplete bool        `json:"incomplete"`
}

type eventView struct {
	// Name, ID and Retry are the fields the format defines; everything else on
	// a line is data.
	Name  string `json:"name,omitempty"`
	ID    string `json:"id,omitempty"`
	Retry string `json:"retry,omitempty"`
	Data  string `json:"data,omitempty"`
}

// isEventStream reports whether a response was server-sent events.
func isEventStream(c *store.Call) bool {
	for _, v := range c.Response.Headers.Values("Content-Type") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "text/event-stream") {
			return true
		}
	}
	return false
}

// buildStreamView splits a stored event stream back into its events.
//
// Events are separated by a blank line, and a stream that was cut mid-event —
// by the body cap or because it was still running — leaves a trailing chunk
// with no terminator. That chunk is reported rather than dropped: a partial
// event is still evidence, and hiding it would be the gap the product forbids.
func buildStreamView(c *store.Call) *streamView {
	body := strings.ReplaceAll(string(c.Response.Body), "\r\n", "\n")
	chunks := strings.Split(body, "\n\n")

	incomplete := c.Response.Truncated
	if len(chunks) > 0 && strings.TrimSpace(chunks[len(chunks)-1]) != "" {
		incomplete = true
	}

	out := &streamView{Events: []eventView{}, Incomplete: incomplete}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var e eventView
		var data []string
		for _, line := range strings.Split(chunk, "\n") {
			// A line starting with a colon is a comment, which is how a server
			// keeps the connection alive. Worth nothing to a reader.
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			field, value, found := strings.Cut(line, ":")
			if !found {
				field, value = line, ""
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				e.Name = value
			case "id":
				e.ID = value
			case "retry":
				e.Retry = value
			case "data":
				data = append(data, value)
			}
		}
		e.Data = strings.Join(data, "\n")

		// A chunk made only of comments carries nothing. Skipping it has to be
		// decided here rather than by looking at the raw text, because ": ping"
		// is not blank and would otherwise arrive as an event with no fields —
		// a keepalive shown as if the server had said something.
		if e.Name == "" && e.ID == "" && e.Retry == "" && e.Data == "" {
			continue
		}
		out.Events = append(out.Events, e)
	}
	return out
}

package api

import (
	"github.com/NicolasCondezaR/sonda/internal/pgwire"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// A Postgres session is one exchange carrying many messages, so it is read back
// the way a socket and a gRPC stream are: the raw stream per direction is what
// was stored, and the messages are a view computed here when someone looks.
//
// pgwire.Message goes out as it is rather than through a translation struct.
// It was designed for this — flat, mostly optional fields, with the payload and
// the raw value bytes excluded from JSON — and a second struct that copied it
// field by field would drop whichever field was added last.

type postgresView struct {
	Sent     []pgwire.Message `json:"sent"`
	Received []pgwire.Message `json:"received"`

	// Incomplete reports bytes left after the last whole message: the capture
	// was cut by the body cap, or the session was still open when it was read.
	SentIncomplete     bool `json:"sent_incomplete"`
	ReceivedIncomplete bool `json:"received_incomplete"`
}

func buildPostgresView(c *store.Call) *postgresView {
	sent, sentRest := pgwire.Deframe(c.Request.Body, true)
	received, receivedRest := pgwire.Deframe(c.Response.Body, false)

	return &postgresView{
		Sent:               sent,
		Received:           received,
		SentIncomplete:     sentRest > 0,
		ReceivedIncomplete: receivedRest > 0,
	}
}

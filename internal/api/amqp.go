package api

import (
	"github.com/NicolasCondezaR/sonda/internal/amqpwire"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// An AMQP capture is a direction-specific protocol unit. Content-bearing
// methods include their content header and body frames, while handshake,
// topology, acknowledgement and failure methods stand alone. The raw bytes are
// stored; this decoded view is computed only when someone reads the capture.
type amqpView struct {
	Sent     []amqpwire.Frame `json:"sent"`
	Received []amqpwire.Frame `json:"received"`

	SentIncomplete     bool `json:"sent_incomplete"`
	ReceivedIncomplete bool `json:"received_incomplete"`
}

func buildAMQPView(c *store.Call) *amqpView {
	sent, sentRest := amqpwire.Deframe(c.Request.Body, true)
	received, receivedRest := amqpwire.Deframe(c.Response.Body, false)
	return &amqpView{
		Sent:               sent,
		Received:           received,
		SentIncomplete:     sentRest > 0,
		ReceivedIncomplete: receivedRest > 0,
	}
}

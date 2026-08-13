package tui

import (
	"strings"
	"testing"
)

func TestTheTerminalShowsAMQPRoutingContentAndBrokerErrors(t *testing.T) {
	m := Model{width: 110}
	out := rendered(m.renderAMQP("SENT", []AMQPFrame{
		{Kind: "basic.publish", Channel: 4, Exchange: "orders", RoutingKey: "order.created"},
		{Kind: "content_header", Channel: 4, BodySize: 16, ContentType: "application/json"},
		{Kind: "content_body", Channel: 4, Text: `{"order_id":417}`},
		{Kind: "channel.close", Channel: 4, ReplyCode: 406, ReplyText: "PRECONDITION_FAILED", Cause: "queue.declare"},
	}, false))

	for _, want := range []string{"BASIC.PUBLISH", "orders → order.created", "application/json", `{"order_id":417}`, "406", "PRECONDITION_FAILED", "queue.declare"} {
		if !strings.Contains(out, want) {
			t.Errorf("the terminal rendering is missing %q:\n%s", want, out)
		}
	}
}

func TestAMQPListingDoesNotInventHTTPStatus(t *testing.T) {
	ok := Call{Protocol: "amqp", Method: "basic.ack", Path: "channel/1"}
	if got := ok.Outcome(); got != "basic.ack" {
		t.Errorf("outcome = %q, want the AMQP method", got)
	}
	ok.Error = "channel.close 406: PRECONDITION_FAILED"
	if got := ok.Outcome(); got != "AMQP ERROR" {
		t.Errorf("outcome = %q", got)
	}
}

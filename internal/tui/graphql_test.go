package tui

import (
	"strings"
	"testing"
)

// The terminal is a client of the same API as the browser, and a capability
// that only the browser can see is half a capability. These check the rendered
// text, which is the only thing a terminal actually produces.

func TestTheTerminalShowsTheOperationAndItsErrors(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderGraphQL(&GraphQLView{
		Errors: 1,
		Operations: []GraphQLOperation{{
			Type: "mutation", Name: "Pay", Label: "mutation Pay",
			Fields:    []string{"pay"},
			Variables: []byte(`{"amount":42}`),
			Errors:    []GraphQLError{{Message: "card declined", Path: "pay", Code: "DECLINED"}},
		}},
	}))

	for _, want := range []string{"GRAPHQL", "mutation Pay", "asks for pay", "amount", "card declined", "DECLINED"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, out)
		}
	}
}

func TestTheTerminalSaysABatchIsABatch(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderGraphQL(&GraphQLView{
		Batch: true,
		Operations: []GraphQLOperation{
			{Type: "query", Label: "query me"},
			{Type: "query", Label: "query orders"},
		},
	}))
	if !strings.Contains(out, "batch of 2") {
		t.Errorf("two operations rendered as one call:\n%s", out)
	}
	if !strings.Contains(out, "query orders") {
		t.Errorf("the second operation of the batch was dropped:\n%s", out)
	}
}

// A response that could not be read is a gap, and reporting it as "no errors"
// would be the guess the product forbids.
func TestAnUnreadableResponseSaysSoInTheTerminal(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderGraphQL(&GraphQLView{
		Operations: []GraphQLOperation{{Type: "query", Label: "query me"}},
		Unreadable: true,
	}))
	if !strings.Contains(out, "unknown") {
		t.Errorf("an unreadable response rendered as a clean call:\n%s", out)
	}
}

// The failure definition, checked against the terminal's own copy of it. A
// GraphQL error arrives under HTTP 200 and nothing else about the call says so.
func TestTheTerminalCountsAGraphQLErrorAsAFault(t *testing.T) {
	failed := Call{Status: 200, GraphQLErrors: 1, GraphQLOp: "mutation Pay"}
	if !failed.Fault() {
		t.Error("a GraphQL error under HTTP 200 was not counted as a fault")
	}
	if failed.Outcome() != "GRAPHQL ERROR" {
		t.Errorf("outcome = %q; \"200\" would be the truth about the transport and a lie about the call", failed.Outcome())
	}

	ok := Call{Status: 200, GraphQLOp: "query me"}
	if ok.Fault() {
		t.Error("a GraphQL call that carried no errors was flagged")
	}
	if ok.Outcome() != "200" {
		t.Errorf("outcome = %q on a call that worked", ok.Outcome())
	}
}

// Every GraphQL call is the same method and path, so without the operation a
// whole lane reads as one call repeated — which is the reason for all of this.
func TestTheOperationNamesTheCall(t *testing.T) {
	c := Call{Method: "POST", Path: "/graphql", GraphQLOp: "query GetOrder"}
	if c.Label() != "POST /graphql · query GetOrder" {
		t.Errorf("label = %q", c.Label())
	}
	plain := Call{Method: "GET", Path: "/v1/orders"}
	if plain.Label() != "GET /v1/orders" {
		t.Errorf("a call that is not GraphQL was relabelled: %q", plain.Label())
	}
}

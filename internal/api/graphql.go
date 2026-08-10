package api

import (
	"encoding/json"

	"github.com/NicolasCondezaR/sonda/internal/graphql"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// A GraphQL call is one POST carrying a document, and it is read back the same
// way a socket or an event stream is: the bytes are what was stored, and the
// operation is a view computed here when someone looks.
//
// The listing already carries the operation and the error count, because a
// listing has no bodies to read. This is the rest of it — the fields asked for,
// the variables sent, and what each error actually said.

type graphqlView struct {
	Batch      bool                 `json:"batch"`
	Operations []graphqlOperationJS `json:"operations"`

	// Errors is the total across the batch, so a reader does not have to add up
	// the operations to learn whether anything went wrong.
	Errors int `json:"errors"`

	// Unreadable says the response was not JSON, so whether it carried errors
	// is unknown rather than false — the usual cause is the body cap.
	Unreadable bool `json:"unreadable,omitempty"`
}

type graphqlOperationJS struct {
	Type   string   `json:"type"`
	Name   string   `json:"name,omitempty"`
	Label  string   `json:"label"`
	Fields []string `json:"fields,omitempty"`

	// Variables go out as JSON, not as a string holding JSON: the client
	// renders them the same way it renders any other decoded payload.
	Variables json.RawMessage `json:"variables,omitempty"`
	Errors    []graphql.Error `json:"errors,omitempty"`
}

func buildGraphQLView(c *store.Call) *graphqlView {
	e := graphql.Decode(c.Method, c.Request.Body, c.Response.Body)
	if e == nil {
		return nil
	}

	out := &graphqlView{
		Batch:      e.Batch,
		Errors:     e.Errors(),
		Unreadable: e.ResponseUnreadable,
		Operations: make([]graphqlOperationJS, 0, len(e.Operations)),
	}
	for _, op := range e.Operations {
		out.Operations = append(out.Operations, graphqlOperationJS{
			Type: op.Type, Name: op.Name, Label: op.Label(),
			Fields: op.Fields, Variables: op.Variables, Errors: op.Errors,
		})
	}
	return out
}

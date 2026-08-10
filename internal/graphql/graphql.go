// Package graphql reads a GraphQL exchange out of the bytes that carried it.
//
// Every operation a GraphQL client makes is the same POST to the same path, so
// in a timeline of calls per service they are indistinguishable and the field
// stops being useful for that service. What tells them apart is inside the
// body, which is why this exists.
//
// It is not a GraphQL parser and must not become one. Sonda needs the operation
// type, the operation name and the top-level fields being asked for — enough to
// label a call. Validating a document, resolving fragments or checking types is
// the server's job, and a real parser here would be a dependency and a second
// implementation of a specification that changes.
package graphql

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Exchange is one captured call read as GraphQL.
type Exchange struct {
	// Batch says the client sent an array of operations in one request, which
	// is a normal client optimisation and looks like a single call from
	// outside. Without this, three operations would be reported as one.
	Batch bool `json:"batch"`

	Operations []Operation `json:"operations"`

	// ResponseUnreadable means the response body was not JSON, so whether it
	// carried errors is unknown rather than false. A capture cut by the body
	// cap lands here, and saying "no errors" about it would be a guess
	// presented as a reading.
	ResponseUnreadable bool `json:"response_unreadable,omitempty"`
}

// Operation is one document in the request, paired with its answer.
type Operation struct {
	// Type is query, mutation or subscription. The shorthand `{ ... }` document
	// has no keyword and is a query.
	Type string `json:"type"`

	// Name is empty for an anonymous operation, which is common enough that
	// Label falls back to the fields instead of showing nothing.
	Name string `json:"name,omitempty"`

	// Fields are the top-level selections: what this operation actually asked
	// for. Nested selections are not walked — the point is a label, not a tree.
	Fields []string `json:"fields,omitempty"`

	// Variables is the raw `variables` object, kept as it arrived rather than
	// reformatted, for the same reason bodies are stored verbatim.
	Variables json.RawMessage `json:"variables,omitempty"`

	// Errors is what the response carried for this operation. A GraphQL server
	// reports failure here under HTTP 200, which is the whole reason this
	// package feeds the fault decision.
	Errors []Error `json:"errors,omitempty"`
}

// Error is one entry of a response's `errors` array.
type Error struct {
	Message string `json:"message"`

	// Path is the response field the error belongs to, joined with dots. A
	// GraphQL error usually names where in the answer it happened, and that is
	// most of what makes it actionable.
	Path string `json:"path,omitempty"`

	// Code is `extensions.code`. Not part of the specification, but every
	// server that carries a machine-readable reason puts it there.
	Code string `json:"code,omitempty"`
}

// Failed reports whether any operation came back with errors.
func (e *Exchange) Failed() bool {
	for _, op := range e.Operations {
		if len(op.Errors) > 0 {
			return true
		}
	}
	return false
}

// Errors is the total across the batch, which is what a listing has room for.
func (e *Exchange) Errors() int {
	n := 0
	for _, op := range e.Operations {
		n += len(op.Errors)
	}
	return n
}

// labelledOps is how many operations of a batch appear in the label before it
// is cut. Three fits a tooltip; the count already says how many there are.
const labelledOps = 3

// Label is the one line that replaces "POST /graphql" in a listing.
func (e *Exchange) Label() string {
	if len(e.Operations) == 0 {
		return ""
	}
	if !e.Batch {
		return e.Operations[0].Label()
	}

	parts := make([]string, 0, labelledOps)
	for _, op := range e.Operations {
		if len(parts) == labelledOps {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, op.Label())
	}
	return "batch of " + strconv.Itoa(len(e.Operations)) + ": " + strings.Join(parts, ", ")
}

// Label names an operation the way a developer would say it out loud. An
// anonymous operation falls back to its fields, because "query" alone is no
// more use than the path it came in on.
func (o Operation) Label() string {
	switch {
	case o.Name != "":
		return o.Type + " " + o.Name
	case len(o.Fields) > 0:
		return o.Type + " " + strings.Join(o.Fields, ",")
	default:
		return o.Type
	}
}

// request is the JSON shape a GraphQL client POSTs.
type request struct {
	Query         *string         `json:"query"`
	OperationName string          `json:"operationName"`
	Variables     json.RawMessage `json:"variables"`
}

type result struct {
	Errors []wireError `json:"errors"`
}

type wireError struct {
	Message    string            `json:"message"`
	Path       []json.RawMessage `json:"path"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// Decode reads a call as a GraphQL exchange, or returns nil when it is not one.
//
// Detection is the request body, never the path: /graphql is a convention, and
// plenty of servers mount the endpoint somewhere else or behind a gateway
// prefix. A POST whose body is JSON carrying a `query` string is the thing
// itself, wherever it was sent.
func Decode(method string, request, response []byte) *Exchange {
	if !strings.EqualFold(method, "POST") {
		return nil
	}
	requests, batch, ok := decodeRequests(request)
	if !ok {
		return nil
	}

	e := &Exchange{Batch: batch, Operations: make([]Operation, 0, len(requests))}
	for _, r := range requests {
		op := Operation{Variables: nonEmptyJSON(r.Variables)}
		op.Type, op.Name, op.Fields = parseDocument(*r.Query, r.OperationName)
		e.Operations = append(e.Operations, op)
	}

	attachResults(e, response)
	return e
}

// decodeRequests reads either one operation or a batched array of them. The
// batch is a normal client optimisation, and reading only the first element
// would silently hide every operation after it.
func decodeRequests(body []byte) (out []request, batch bool, ok bool) {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var many []request
		if json.Unmarshal(body, &many) != nil || len(many) == 0 {
			return nil, false, false
		}
		for _, r := range many {
			// One element without a query means this array is something else
			// that happens to be JSON, not a GraphQL batch.
			if r.Query == nil {
				return nil, false, false
			}
		}
		return many, true, true
	}

	var one request
	if json.Unmarshal(body, &one) != nil || one.Query == nil {
		return nil, false, false
	}
	return []request{one}, false, true
}

// attachResults pairs each answer with the operation that asked for it.
//
// A batched response comes back in request order — the specification requires
// it — so the index is the pairing. When the two lengths disagree the server
// broke that rule or the capture was cut, and the extra results are attached to
// nothing rather than guessed onto the wrong operation.
func attachResults(e *Exchange, body []byte) {
	if len(body) == 0 {
		// No body at all is not an unreadable body: a transport error already
		// says what happened, and flagging it here would double the complaint.
		return
	}

	var results []result
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal(body, &results) != nil {
			e.ResponseUnreadable = true
			return
		}
	} else {
		var one result
		if json.Unmarshal(body, &one) != nil {
			e.ResponseUnreadable = true
			return
		}
		results = []result{one}
	}

	for i := range e.Operations {
		if i >= len(results) {
			return
		}
		for _, err := range results[i].Errors {
			e.Operations[i].Errors = append(e.Operations[i].Errors, Error{
				Message: err.Message,
				Path:    joinPath(err.Path),
				Code:    err.Extensions.Code,
			})
		}
	}
}

// joinPath renders an error path. Its elements are field names or list indexes,
// so they arrive as a mix of strings and numbers.
func joinPath(raw []json.RawMessage) string {
	parts := make([]string, 0, len(raw))
	for _, p := range raw {
		var name string
		if json.Unmarshal(p, &name) == nil {
			parts = append(parts, name)
			continue
		}
		parts = append(parts, string(p))
	}
	return strings.Join(parts, ".")
}

// nonEmptyJSON drops the JSON spellings of "nothing", so a client that always
// sends `variables: null` does not put an empty field on every call.
func nonEmptyJSON(raw json.RawMessage) json.RawMessage {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "{}":
		return nil
	}
	return raw
}

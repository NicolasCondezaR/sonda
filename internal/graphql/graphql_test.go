package graphql

import (
	"strings"
	"testing"
)

func decode(t *testing.T, request, response string) *Exchange {
	t.Helper()
	e := Decode("POST", []byte(request), []byte(response))
	if e == nil {
		t.Fatalf("the request was not read as GraphQL: %s", request)
	}
	return e
}

func TestANamedOperationIsReadOffTheDocument(t *testing.T) {
	e := decode(t, `{"query":"query GetOrder($id: ID!) { order(id: $id) { id total } me { name } }",
		"variables":{"id":"ORD-1"}}`, `{"data":{}}`)

	if len(e.Operations) != 1 || e.Batch {
		t.Fatalf("exchange = %+v", e)
	}
	op := e.Operations[0]
	if op.Type != "query" || op.Name != "GetOrder" {
		t.Errorf("operation = %q %q", op.Type, op.Name)
	}
	if strings.Join(op.Fields, ",") != "order,me" {
		t.Errorf("fields = %v, want the two top-level selections", op.Fields)
	}
	if string(op.Variables) != `{"id":"ORD-1"}` {
		t.Errorf("variables = %s", op.Variables)
	}
	if op.Label() != "query GetOrder" {
		t.Errorf("label = %q", op.Label())
	}
}

// The shorthand document has no keyword at all, and it is a query.
func TestTheShorthandDocumentIsAQuery(t *testing.T) {
	e := decode(t, `{"query":"{ viewer { id } }"}`, `{"data":{}}`)
	op := e.Operations[0]
	if op.Type != "query" || op.Name != "" {
		t.Errorf("operation = %q %q", op.Type, op.Name)
	}
	if strings.Join(op.Fields, ",") != "viewer" {
		t.Errorf("fields = %v", op.Fields)
	}
	// Anonymous is common, and "query" on its own is no more use than the path
	// the call came in on.
	if op.Label() != "query viewer" {
		t.Errorf("label = %q", op.Label())
	}
}

func TestMutationsAndSubscriptionsAreNamedAsWhatTheyAre(t *testing.T) {
	for document, want := range map[string]string{
		`mutation Pay { pay { ok } }`:       "mutation Pay",
		`subscription Tick { tick { at } }`: "subscription Tick",
	} {
		e := decode(t, `{"query":`+quote(document)+`}`, "")
		if got := e.Label(); got != want {
			t.Errorf("%s → %q, want %q", document, got, want)
		}
	}
}

// An alias renames a field in the answer. Reporting the alias would name
// something the server never resolved.
func TestAnAliasReportsTheFieldTheServerSaw(t *testing.T) {
	e := decode(t, `{"query":"{ mine: orders { id } }"}`, "")
	if strings.Join(e.Operations[0].Fields, ",") != "orders" {
		t.Errorf("fields = %v", e.Operations[0].Fields)
	}
}

// Arguments, nested selections, directives and comments all sit between the
// scanner and the field names, and any one of them read as a name puts junk in
// the label.
func TestNoiseBetweenTheFieldsIsSteppedOver(t *testing.T) {
	document := `
		# leading comment with a { brace and a "quote
		query Messy($n: Int = 3, $filter: Filter = {kind: "a}b"}) @cached(ttl: 30) {
			orders(first: $n, where: {status: "OPEN # not a comment"}) @include(if: true) {
				id
				lines { sku }
			}
			... on Root { hidden }
			...SomeFragment
			me { name }
		}`
	e := decode(t, `{"query":`+quote(document)+`}`, "")
	op := e.Operations[0]
	if op.Name != "Messy" {
		t.Errorf("name = %q", op.Name)
	}
	if strings.Join(op.Fields, ",") != "orders,me" {
		t.Errorf("fields = %v, want only the two real top-level selections", op.Fields)
	}
}

// A document can hold several operations and the client says which one it ran.
func TestOperationNamePicksTheOperationThatRan(t *testing.T) {
	document := `query A { a } query B { b }`
	e := decode(t, `{"query":`+quote(document)+`,"operationName":"B"}`, "")
	if e.Operations[0].Name != "B" || strings.Join(e.Operations[0].Fields, ",") != "b" {
		t.Errorf("operation = %+v, want the one the client named", e.Operations[0])
	}
}

func TestABatchIsEveryOperationInIt(t *testing.T) {
	e := decode(t,
		`[{"query":"query A { a }"},{"query":"mutation B { b }"},{"query":"{ c }"},{"query":"{ d }"}]`,
		`[{"data":{}},{"errors":[{"message":"nope"}]},{"data":{}},{"data":{}}]`)

	if !e.Batch || len(e.Operations) != 4 {
		t.Fatalf("exchange = %+v", e)
	}
	// The answers come back in request order, so the error belongs to the
	// second operation and to no other.
	if len(e.Operations[1].Errors) != 1 || e.Operations[1].Errors[0].Message != "nope" {
		t.Errorf("the error did not land on the operation that caused it: %+v", e.Operations)
	}
	if !e.Failed() || e.Errors() != 1 {
		t.Errorf("failed = %v, errors = %d", e.Failed(), e.Errors())
	}
	if got := e.Label(); got != "batch of 4: query A, mutation B, query c, …" {
		t.Errorf("label = %q", got)
	}
}

// The reason this package exists at all: a GraphQL failure arrives under 200.
func TestErrorsAreReadWithTheirPathAndCode(t *testing.T) {
	e := decode(t, `{"query":"{ order { total } }"}`,
		`{"data":{"order":null},"errors":[{"message":"Order not found",
			"path":["order",0,"total"],"extensions":{"code":"NOT_FOUND"}}]}`)

	if !e.Failed() {
		t.Fatal("a response carrying an errors array was not read as failed")
	}
	got := e.Operations[0].Errors[0]
	if got.Message != "Order not found" || got.Path != "order.0.total" || got.Code != "NOT_FOUND" {
		t.Errorf("error = %+v", got)
	}
}

// An empty errors array is not a failure, and treating it as one would flag
// every call from a server that always sends the field.
func TestAnEmptyErrorsArrayIsNotAFailure(t *testing.T) {
	e := decode(t, `{"query":"{ a }"}`, `{"data":{},"errors":[]}`)
	if e.Failed() {
		t.Error("an empty errors array was read as a failure")
	}
}

// A body cut by the cap cannot be read, and saying "no errors" about it would
// be a guess presented as a reading.
func TestAnUnreadableResponseSaysSoRatherThanSayingItPassed(t *testing.T) {
	e := decode(t, `{"query":"{ a }"}`, `{"data":{"a":"half of a `)
	if !e.ResponseUnreadable {
		t.Error("a response that is not JSON was not reported as unreadable")
	}
	if e.Failed() {
		t.Error("an unreadable response was claimed to have failed")
	}

	// No body at all is a different thing: the transport error already says
	// what happened.
	if decode(t, `{"query":"{ a }"}`, "").ResponseUnreadable {
		t.Error("an empty response was reported as unreadable")
	}
}

func TestWhatIsNotGraphQL(t *testing.T) {
	for name, c := range map[string]struct{ method, body string }{
		"a GET":                {"GET", `{"query":"{ a }"}`},
		"plain JSON":           {"POST", `{"name":"nico"}`},
		"a non-string query":   {"POST", `{"query":{"nested":true}}`},
		"not JSON at all":      {"POST", `sku=ABC-9&qty=2`},
		"an empty body":        {"POST", ``},
		"an array of anything": {"POST", `[{"id":1},{"id":2}]`},
		"an empty array":       {"POST", `[]`},
		// One element without a query means the array is something else that
		// happens to be JSON.
		"a half batch": {"POST", `[{"query":"{ a }"},{"id":2}]`},
	} {
		if e := Decode(c.method, []byte(c.body), []byte(`{"errors":[{"message":"x"}]}`)); e != nil {
			t.Errorf("%s was read as GraphQL: %+v", name, e)
		}
	}
}

// The endpoint is a convention, not a marker. A gateway prefix or a service
// that mounts it elsewhere must not make its traffic unreadable.
func TestDetectionDoesNotDependOnThePath(t *testing.T) {
	if Decode("POST", []byte(`{"query":"{ a }"}`), nil) == nil {
		t.Error("a GraphQL body was rejected; nothing here may look at the path")
	}
}

func TestVariablesThatSayNothingAreDropped(t *testing.T) {
	for _, body := range []string{
		`{"query":"{ a }"}`,
		`{"query":"{ a }","variables":null}`,
		`{"query":"{ a }","variables":{}}`,
	} {
		if v := decode(t, body, "").Operations[0].Variables; v != nil {
			t.Errorf("%s kept variables %s", body, v)
		}
	}
}

// quote turns a document into a JSON string literal without pulling the test
// into escaping by hand, which is how a test starts proving the wrong thing.
func quote(s string) string {
	return `"` + strings.NewReplacer(
		`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`,
	).Replace(s) + `"`
}

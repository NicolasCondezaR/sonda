package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// insertGraphQL records a POST the way the proxy would, so the derived columns
// are written by the same path production uses rather than set by the test.
func insertGraphQL(t *testing.T, s *store.Store, request, response string) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), &store.Call{
		Target: "api", Protocol: "http", Method: "POST", Path: "/graphql",
		Status: 200, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Duration: 5 * time.Millisecond,
		Request:  store.Message{Headers: http.Header{}, Body: []byte(request), Size: int64(len(request))},
		Response: store.Message{Headers: http.Header{}, Body: []byte(response), Size: int64(len(response))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTheDetailCarriesTheDecodedOperation(t *testing.T) {
	h, s := newServer(t)
	id := insertGraphQL(t, s,
		`{"query":"query GetOrder($id: ID!) { order(id: $id) { total } }","variables":{"id":"ORD-1"}}`,
		`{"data":{"order":{"total":10}}}`)

	code, detail := get(t, h, "/api/calls/"+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	view, ok := detail["graphql"].(map[string]any)
	if !ok {
		t.Fatalf("the detail carries no graphql view: %+v", detail)
	}
	op := view["operations"].([]any)[0].(map[string]any)
	if op["label"] != "query GetOrder" {
		t.Errorf("label = %v", op["label"])
	}
	if op["fields"].([]any)[0] != "order" {
		t.Errorf("fields = %v", op["fields"])
	}
	if op["variables"].(map[string]any)["id"] != "ORD-1" {
		t.Errorf("variables = %v", op["variables"])
	}

	// The raw body is untouched by the decode: the view is a reading of the
	// record, never a replacement for it.
	if request := detail["request"].(map[string]any); request["text"] == nil {
		t.Error("the stored request body did not come back alongside the view")
	}
}

// The whole reason this exists: a GraphQL failure answers HTTP 200, and a
// listing that trusts the status reports it as a success.
func TestAGraphQLErrorIsAFailureEverywhereItIsDecided(t *testing.T) {
	h, s := newServer(t)
	insertGraphQL(t, s, `{"query":"mutation Pay { pay { ok } }"}`,
		`{"data":null,"errors":[{"message":"card declined","path":["pay"],"extensions":{"code":"DECLINED"}}]}`)

	// The listing, which is what the field and the terminal read.
	code, body := get(t, h, "/api/calls")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	call := body["calls"].([]any)[0].(map[string]any)
	if call["graphql_errors"].(float64) != 1 {
		t.Errorf("the summary reports %v errors, so every client will call this a success", call["graphql_errors"])
	}
	if call["graphql_op"] != "mutation Pay" {
		t.Errorf("graphql_op = %v; without it the whole lane reads as one repeated call", call["graphql_op"])
	}

	// The failed filter and the stats rollup, which are one SQL definition.
	if _, only := get(t, h, "/api/calls?failed=true"); len(only["calls"].([]any)) != 1 {
		t.Error("a GraphQL error did not survive the failed filter")
	}
	_, stats := get(t, h, "/api/stats")
	byTarget := stats["by_target"].([]any)[0].(map[string]any)
	if byTarget["faults"].(float64) != 1 {
		t.Errorf("the channel rail counted %v faults, so the rail and the field disagree", byTarget["faults"])
	}

	// The tree, which decides again on its own.
	id := int64(call["id"].(float64))
	_, tree := get(t, h, "/api/trace?call="+strconv.FormatInt(id, 10))
	if tree["trace"].(map[string]any)["failed"].(float64) != 1 {
		t.Errorf("the tree called a failed GraphQL call healthy: %+v", tree["trace"])
	}
}

// An empty errors array is not a failure, and a server that always sends the
// field would otherwise flag every call it ever answered.
func TestAGraphQLCallThatWorkedIsNotFlagged(t *testing.T) {
	h, s := newServer(t)
	insertGraphQL(t, s, `{"query":"{ me { name } }"}`, `{"data":{"me":{"name":"nico"}},"errors":[]}`)

	_, body := get(t, h, "/api/calls")
	if call := body["calls"].([]any)[0].(map[string]any); call["graphql_errors"] != nil {
		t.Errorf("graphql_errors = %v on a call that succeeded", call["graphql_errors"])
	}
	if _, only := get(t, h, "/api/calls?failed=true"); len(only["calls"].([]any)) != 0 {
		t.Error("a successful GraphQL call was reported as a failure")
	}
}

// A POST that is not GraphQL must not grow a view, or every JSON API in the
// project would render as one.
func TestOnlyAGraphQLBodyGetsTheGraphQLView(t *testing.T) {
	h, s := newServer(t)
	id := insert(t, s, []byte(`{"sku":"ABC-9"}`))

	_, detail := get(t, h, "/api/calls/"+strconv.FormatInt(id, 10))
	if _, present := detail["graphql"]; present {
		t.Errorf("a plain JSON POST was read as GraphQL: %+v", detail["graphql"])
	}
}

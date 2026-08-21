package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// A trace id Sonda wrote onto the request has to say so all the way to the
// tree an agent or the interface actually reads, not only in the database —
// the same failure mode the store's own Summary() comment names for stub_of.
func TestATreeSaysWhichTraceIDsCameFromSonda(t *testing.T) {
	h, s := newServer(t)

	id, err := s.Insert(context.Background(), &store.Call{
		Target: "gateway", Protocol: "http", Method: "GET", Path: "/orders",
		Status: 200, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Duration: 5 * time.Millisecond,
		TraceID:  "sonda-abc123", TraceIDInjected: true,
		Request:  store.Message{Headers: http.Header{}},
		Response: store.Message{Headers: http.Header{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, detail := get(t, h, "/api/calls/"+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail["trace_id_injected"] != true {
		t.Errorf("the call detail did not report the trace id as injected: %v", detail["trace_id_injected"])
	}

	code, tree := get(t, h, "/api/trace?call="+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("trace status = %d: %v", code, tree)
	}
	root := tree["trace"].(map[string]any)["root"].(map[string]any)
	call := root["call"].(map[string]any)
	if call["trace_id_injected"] != true {
		t.Errorf("the tree's own node did not carry the flag: %v", call)
	}
}

// The common case — a real trace id, or none at all — must never be flagged.
func TestATreeDoesNotFlagARealOrAbsentTraceID(t *testing.T) {
	h, s := newServer(t)

	id, err := s.Insert(context.Background(), &store.Call{
		Target: "gateway", Protocol: "http", Method: "GET", Path: "/orders",
		Status: 200, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Duration: 5 * time.Millisecond,
		TraceID:  "client-sent-this",
		Request:  store.Message{Headers: http.Header{}},
		Response: store.Message{Headers: http.Header{}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, detail := get(t, h, "/api/calls/"+strconv.FormatInt(id, 10))
	if detail["trace_id_injected"] != nil {
		t.Errorf("a real client trace id was flagged as injected: %v", detail["trace_id_injected"])
	}
}

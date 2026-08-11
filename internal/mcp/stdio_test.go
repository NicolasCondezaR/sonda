package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// One message too large used to end the process. bufio.Scanner returns
// ErrTooLong for good — every message after it is unreadable — ServeStdio
// returned it and `sonda mcp` exited, so an upload_schemas of about three
// megabytes (four thirds of that once base64-encoded) did not fail: the client
// watched the server die and had no idea why.
func TestAnOversizedMessageIsRefusedAndTheSessionSurvives(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_schemas","arguments":{"content_base64":"` +
		strings.Repeat("A", maxMessage+1024) + `"}}}`
	in := strings.NewReader(huge + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")

	var out bytes.Buffer
	s := New(&routedAPI{}, "test")
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("the server stopped on an oversized message: %v", err)
	}

	var answers []response
	decoder := json.NewDecoder(&out)
	for decoder.More() {
		var r response
		if err := decoder.Decode(&r); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, r)
	}
	if len(answers) != 2 {
		t.Fatalf("%d answers, want the refusal and the message after it", len(answers))
	}

	if answers[0].Error == nil {
		t.Fatalf("the oversized message was not reported at all: %+v", answers[0])
	}
	// It has to say what to do instead, or the agent retries the same upload.
	if !strings.Contains(answers[0].Error.Message, "HTTP") {
		t.Errorf("the refusal does not say how to send something this large: %q", answers[0].Error.Message)
	}
	if answers[1].Error != nil {
		t.Errorf("the message after the oversized one was not answered: %+v", answers[1])
	}
}

// A message that fits still has to be read whole, and 64 KB is nothing for a
// captured body quoted back into a tool result.
func TestALargeButLegalMessageIsStillRead(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_calls","arguments":{"text":"` +
		strings.Repeat("x", 512<<10) + `"}}}`

	var out bytes.Buffer
	s := New(&routedAPI{routes: map[string]string{"GET /api/calls": `{"calls":[]}`}}, "test")
	if err := s.ServeStdio(context.Background(), strings.NewReader(big+"\n"), &out); err != nil {
		t.Fatal(err)
	}

	var answer response
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not one JSON message: %v", err)
	}
	if answer.Error != nil {
		t.Errorf("a message well under the limit was refused: %v", answer.Error.Message)
	}
}

// A last line with no newline is still a message: a client that writes one
// request and closes the pipe must not be ignored.
func TestTheLastLineWithoutANewlineIsAnswered(t *testing.T) {
	var out bytes.Buffer
	s := New(&routedAPI{}, "test")
	if err := s.ServeStdio(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Error("a request that arrived without a trailing newline got no answer")
	}
}

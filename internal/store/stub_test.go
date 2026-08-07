package store

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func stubStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "stub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func record(t *testing.T, s *Store, target, method, path, reqBody, respBody string, stubOf *int64) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), &Call{
		Target: target, Protocol: "http", Method: method, Path: path,
		Status: 200, StartedAt: time.Now().UTC(), Duration: time.Millisecond,
		Request:  Message{Headers: http.Header{}, Body: []byte(reqBody)},
		Response: Message{Headers: http.Header{}, Body: []byte(respBody)},
		StubOf:   stubOf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The ordering is the whole design. Replaying "the answer to GetOrder" when the
// question was GetOrder(ORD-2) hands back somebody else's order, and a test
// that gets the wrong data is worse off than one that got an error.
func TestAnIdenticalRequestBodyWinsOverAMoreRecentOne(t *testing.T) {
	s := stubStore(t)
	wanted := record(t, s, "orders", "POST", "/get", `{"id":"ORD-1"}`, "el uno", nil)
	record(t, s, "orders", "POST", "/get", `{"id":"ORD-2"}`, "el dos", nil) // newer

	got, err := s.MatchForStub(context.Background(), "orders", "POST", "/get", []byte(`{"id":"ORD-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("no recording matched")
	}
	if got.ID != wanted {
		t.Errorf("matched call %d (%q), want the one with the same request body", got.ID, got.Response.Body)
	}
}

// With nothing identical to go on, the most recent call to the same place is
// the best available guess.
func TestWithoutAnExactBodyTheMostRecentWins(t *testing.T) {
	s := stubStore(t)
	record(t, s, "orders", "POST", "/get", `{"id":"ORD-1"}`, "viejo", nil)
	newest := record(t, s, "orders", "POST", "/get", `{"id":"ORD-2"}`, "nuevo", nil)

	got, err := s.MatchForStub(context.Background(), "orders", "POST", "/get", []byte(`{"id":"ORD-9"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != newest {
		t.Errorf("matched %v, want the most recent", got)
	}
}

// Leaving stubbing on would otherwise slowly feed Sonda its own answers, and
// the recording would drift from anything a service ever really said.
func TestAStubbedCaptureIsNeverUsedAsARecording(t *testing.T) {
	s := stubStore(t)
	real := record(t, s, "orders", "POST", "/get", `{"id":"ORD-1"}`, "de verdad", nil)
	record(t, s, "orders", "POST", "/get", `{"id":"ORD-1"}`, "eco de un stub", &real) // newer, and a stub

	got, err := s.MatchForStub(context.Background(), "orders", "POST", "/get", []byte(`{"id":"ORD-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != real {
		t.Errorf("matched %q, want the one that came from the real service", got.Response.Body)
	}
}

// Nothing recorded is not an error; it is the answer, and the caller turns it
// into an honest refusal rather than an invented response.
func TestNothingRecordedIsNotAnError(t *testing.T) {
	s := stubStore(t)
	record(t, s, "orders", "POST", "/get", "", "", nil)

	for _, c := range []struct{ target, method, path string }{
		{"otro-servicio", "POST", "/get"},
		{"orders", "GET", "/get"},
		{"orders", "POST", "/otra-ruta"},
	} {
		got, err := s.MatchForStub(context.Background(), c.target, c.method, c.path, nil)
		if err != nil {
			t.Errorf("%v: %v", c, err)
		}
		if got != nil {
			t.Errorf("%v matched call %d, want nothing", c, got.ID)
		}
	}
}

// The link survives the round trip, which is what lets everything downstream
// tell a recording apart from a fact.
func TestStubOfSurvivesStorage(t *testing.T) {
	s := stubStore(t)
	origin := record(t, s, "orders", "POST", "/get", "", "x", nil)
	id := record(t, s, "orders", "POST", "/get", "", "x", &origin)

	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.StubOf == nil || *got.StubOf != origin {
		t.Errorf("StubOf = %v, want %d", got.StubOf, origin)
	}
	if plain, _ := s.Get(context.Background(), origin); plain.StubOf != nil {
		t.Error("a real capture came back marked as a stub")
	}
}

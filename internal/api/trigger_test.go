package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/trigger"
)

// post is the counterpart of the get helper: the trigger is the first thing in
// this package that is armed over JSON rather than read.
func post(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response for %s is not JSON: %v (%s)", path, err, rec.Body.String())
	}
	return rec.Code, out
}

// armedServer is the API with a trigger wired in, which is how the binary runs
// it. newServer leaves it nil on purpose, so every other test keeps working
// without one.
func armedServer(t *testing.T) (http.Handler, *store.Store, *Server) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/trigger.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	server := New(db, noDrops{}, emptyRuntime(t, db)).WithTrigger(trigger.New())
	return server.Handler(), db, server
}

// crossed pushes one capture through the same hook the recorder calls, which is
// the only path a trigger can fire from.
//
// startedAt is a parameter rather than time.Now(): arming happens in-process,
// one function call before this one, and on a clock coarse enough — this
// machine included — two calls that close together can read back the exact
// same instant. The trigger is right to refuse that as "not after arming"; the
// fix belongs here, in giving the test a real gap, not in loosening what
// "after" means for everyone who arms one for real, where a network round trip
// to the actual upstream always provides one.
func crossed(t *testing.T, s *Server, target string, status int, startedAt time.Time) *store.Call {
	t.Helper()
	call := &store.Call{
		Target: target, Protocol: "http", Method: "GET", Path: "/rates/CL",
		Status: status, ClientAddr: "127.0.0.1:1", StartedAt: startedAt,
		Duration: 5 * time.Millisecond,
		Request:  store.Message{Headers: http.Header{}},
		Response: store.Message{Headers: http.Header{}},
	}
	s.OnStored(call)
	return call
}

func TestArmingAndFiring(t *testing.T) {
	h, _, server := armedServer(t)
	base := time.Now().UTC()

	code, armed := post(t, h, "/api/trigger", `{"service":"ms-rates","failed":true}`)
	if code != http.StatusOK {
		t.Fatalf("arming answered %d: %v", code, armed)
	}
	if armed["armed"] != true {
		t.Fatalf("the trigger did not report itself armed: %v", armed)
	}

	crossed(t, server, "ms-rates", 200, base.Add(time.Second))
	if _, state := get(t, h, "/api/trigger"); state["fired"] != nil {
		t.Fatal("a call that did not fail fired a trigger armed on failures")
	}

	crossed(t, server, "ms-rates", 503, base.Add(2*time.Second))
	_, state := get(t, h, "/api/trigger")
	if state["fired"] == nil {
		t.Fatal("the failure never fired the trigger")
	}
	// Single mode is the default, so the moment stays readable and the trigger
	// stops watching.
	if state["armed"] != false {
		t.Error("the trigger stayed armed after firing in single mode")
	}
}

func TestClearingTheTrigger(t *testing.T) {
	h, _, server := armedServer(t)
	base := time.Now().UTC()
	post(t, h, "/api/trigger", `{"service":"ms-rates"}`)
	crossed(t, server, "ms-rates", 500, base.Add(time.Second))

	code, cleared := post(t, h, "/api/trigger", `{"clear":true}`)
	if code != http.StatusOK {
		t.Fatalf("clearing answered %d", code)
	}
	if cleared["fired"] != nil || cleared["condition"] != nil {
		t.Error("clearing left something behind to read")
	}
}

func TestATriggerThatWouldFireOnAnythingIsRefused(t *testing.T) {
	h, _, _ := armedServer(t)

	if code, _ := post(t, h, "/api/trigger", `{}`); code != http.StatusBadRequest {
		t.Errorf("an empty condition answered %d; it would fire on the next call whatever it is", code)
	}
	if code, _ := post(t, h, "/api/trigger", `{"service":"x","mode":"burst"}`); code != http.StatusBadRequest {
		t.Errorf("an unknown mode answered %d", code)
	}
}

// A Sonda without a trigger wired in has to answer the question rather than
// panic on a nil, the same way the stub and fault endpoints do.
func TestTheTriggerEndpointsSurviveWithoutOne(t *testing.T) {
	h, _ := newServer(t)

	code, state := get(t, h, "/api/trigger")
	if code != http.StatusOK || state["armed"] != false {
		t.Errorf("reading the trigger without one answered %d: %v", code, state)
	}
	if code, _ := post(t, h, "/api/trigger", `{"service":"x"}`); code != http.StatusServiceUnavailable {
		t.Errorf("arming without a trigger answered %d, want a plain refusal", code)
	}
}

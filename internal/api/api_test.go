package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mirador/internal/runtime"
	"mirador/internal/store"
)

type noDrops struct{}

func (noDrops) Dropped() int64 { return 7 }

// emptyRuntime is a runtime with no active project: enough for the endpoints
// that only read captures, and it opens no ports.
func emptyRuntime(t *testing.T, s *store.Store) *runtime.Runtime {
	t.Helper()
	rt := runtime.New(s, noRecorder{}, 1<<20)
	if err := rt.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	return rt
}

type noRecorder struct{}

func (noRecorder) Record(*store.Call) {}

func newServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	return New(s, noDrops{}, emptyRuntime(t, s)).Handler(), s
}

func insert(t *testing.T, s *store.Store, body []byte) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), &store.Call{
		Target: "api", Protocol: "http", Method: "POST", Path: "/v1/orders",
		Status: 201, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Duration: 5 * time.Millisecond,
		Request:  store.Message{Headers: http.Header{"X-Id": []string{"1"}}, Body: body, Size: int64(len(body))},
		Response: store.Message{Headers: http.Header{}, Body: []byte(`{"id":1}`), Size: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response for %s is not JSON: %v (%s)", path, err, rec.Body.String())
	}
	return rec.Code, body
}

// A listing that carries payloads is unusable once there are a few hundred
// calls, so bodies belong to the detail view only.
func TestListOmitsBodiesAndDetailIncludesThem(t *testing.T) {
	h, s := newServer(t)
	insert(t, s, []byte(`{"sku":"ABC-9"}`))

	code, body := get(t, h, "/api/calls")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	calls := body["calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	first := calls[0].(map[string]any)
	if _, present := first["request"]; present {
		t.Error("the listing should not carry request bodies")
	}
	if first["path"] != "/v1/orders" || first["status"].(float64) != 201 {
		t.Errorf("summary is wrong: %+v", first)
	}

	id := int64(first["id"].(float64))
	code, detail := get(t, h, "/api/calls/"+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("detail status = %d", code)
	}
	request := detail["request"].(map[string]any)
	if request["text"] != `{"sku":"ABC-9"}` {
		t.Errorf("request text = %v", request["text"])
	}
}

// A body that is not valid UTF-8 must still come back intact, as base64.
func TestBinaryBodyIsReturnedAsBase64(t *testing.T) {
	h, s := newServer(t)
	id := insert(t, s, []byte{0x08, 0x96, 0x01, 0xff, 0xfe})

	code, detail := get(t, h, "/api/calls/"+strconv.FormatInt(id, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	request := detail["request"].(map[string]any)
	if request["text"] != nil {
		t.Error("binary body should not be returned as text")
	}
	if request["base64"] != "CJYB//4=" {
		t.Errorf("base64 = %v", request["base64"])
	}
}

func TestFiltersAndErrors(t *testing.T) {
	h, s := newServer(t)
	insert(t, s, []byte(`{"sku":"ABC-9"}`))

	if _, body := get(t, h, "/api/calls?q=ABC-9"); len(body["calls"].([]any)) != 1 {
		t.Error("search filter did not reach the store")
	}
	if _, body := get(t, h, "/api/calls?target=nope"); len(body["calls"].([]any)) != 0 {
		t.Error("target filter did not reach the store")
	}
	if code, _ := get(t, h, "/api/calls?status=abc"); code != http.StatusBadRequest {
		t.Errorf("bad status filter returned %d, want 400", code)
	}
	if code, _ := get(t, h, "/api/calls/999999"); code != http.StatusNotFound {
		t.Errorf("missing call returned %d, want 404", code)
	}
	if code, _ := get(t, h, "/api/calls/not-a-number"); code != http.StatusBadRequest {
		t.Errorf("bad id returned %d, want 400", code)
	}
}

// Dropped calls are surfaced so the loss is visible instead of silent.
func TestStatsReportsDrops(t *testing.T) {
	h, s := newServer(t)
	insert(t, s, nil)

	code, body := get(t, h, "/api/stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["calls"].(float64) != 1 {
		t.Errorf("calls = %v", body["calls"])
	}
	if body["dropped"].(float64) != 7 {
		t.Errorf("dropped = %v, want the recorder's counter", body["dropped"])
	}
}

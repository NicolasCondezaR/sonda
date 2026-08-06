package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func (s *liveStack) diff(t *testing.T, a, b int64) (int, diffResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/diff?a="+strconv.FormatInt(a, 10)+"&b="+strconv.FormatInt(b, 10), nil)
	rec := httptest.NewRecorder()
	s.api.ServeHTTP(rec, req)

	var out diffResult
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not a diff: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestDiffFindsTheChangedField(t *testing.T) {
	stack := newLiveStack(t)
	a := stack.call(t, http.MethodPost, "/v1/orders", `{"sku":"ABC-9","qty":3}`)
	b := stack.call(t, http.MethodPost, "/v1/orders", `{"sku":"ABC-9","qty":7}`)

	code, out := stack.diff(t, a, b)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !out.Request.Comparable {
		t.Fatalf("request should be comparable: %s", out.Request.Reason)
	}
	if out.Request.Identical {
		t.Fatal("the requests differ and the diff says they do not")
	}
	if len(out.Request.Changes) != 1 || out.Request.Changes[0].Path != "qty" {
		t.Errorf("changes = %+v, want one on qty", out.Request.Changes)
	}
}

// Two captures of the same request must come back clean, or every replay would
// look like a regression.
func TestDiffOfIdenticalRequestsIsEmpty(t *testing.T) {
	stack := newLiveStack(t)
	body := `{"sku":"ABC-9","cliente":"Comercial Andes"}`
	a := stack.call(t, http.MethodPost, "/v1/orders", body)
	b := stack.call(t, http.MethodPost, "/v1/orders", body)

	_, out := stack.diff(t, a, b)
	if !out.Request.Identical || len(out.Request.Changes) != 0 {
		t.Errorf("expected identical requests, got %+v", out.Request.Changes)
	}
	if !out.Response.Identical {
		t.Errorf("expected identical responses, got %+v", out.Response.Changes)
	}
	if len(out.Metadata) != 0 {
		t.Errorf("expected no metadata differences, got %+v", out.Metadata)
	}
}

func TestDiffReportsAChangedOutcome(t *testing.T) {
	stack := newLiveStack(t)
	a := stack.call(t, http.MethodPost, "/v1/orders", `{"sku":"ABC-9"}`)
	b := stack.call(t, http.MethodGet, "/v1/orders", "")

	_, out := stack.diff(t, a, b)
	var paths []string
	for _, c := range out.Metadata {
		paths = append(paths, c.Path)
	}
	if len(paths) != 1 || paths[0] != "method" {
		t.Errorf("metadata changes = %v, want just the method", paths)
	}
}

// Duration differs on every replay by definition. Listing it would bury the
// changes that mean something.
func TestDiffIgnoresDuration(t *testing.T) {
	stack := newLiveStack(t)
	body := `{"sku":"ABC-9"}`
	a := stack.call(t, http.MethodPost, "/v1/orders", body)
	b := stack.call(t, http.MethodPost, "/v1/orders", body)

	_, out := stack.diff(t, a, b)
	for _, c := range out.Metadata {
		if c.Path == "duration" || c.Path == "duration_ms" {
			t.Errorf("duration should not be diffed: %+v", c)
		}
	}
}

// A body that is not JSON gets an honest answer rather than a structural diff
// that was never possible.
func TestDiffOfNonJSONBodiesSaysSo(t *testing.T) {
	stack := newLiveStack(t)
	a := stack.call(t, http.MethodPost, "/v1/raw", "plain text one")
	b := stack.call(t, http.MethodPost, "/v1/raw", "plain text two")

	_, out := stack.diff(t, a, b)
	if out.Request.Comparable {
		t.Error("plain text is not structurally comparable and should not claim to be")
	}
	if out.Request.Reason == "" {
		t.Error("the diff should say why it could not compare")
	}
	if out.Request.Identical {
		t.Error("the two bodies differ")
	}
}

func TestDiffOfMissingCall(t *testing.T) {
	stack := newLiveStack(t)
	id := stack.call(t, http.MethodGet, "/v1/orders", "")
	if code, _ := stack.diff(t, id, 999999); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestDiffRejectsBadIds(t *testing.T) {
	stack := newLiveStack(t)
	req := httptest.NewRequest(http.MethodGet, "/api/diff?a=abc&b=1", nil)
	rec := httptest.NewRecorder()
	stack.api.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// insertRun writes one whole run of the checkout flow and returns the id of its
// entry point. Everything shares a trace id, so the trees are facts rather than
// guesses and the comparison is testing alignment, not the tree builder.
func insertRun(t *testing.T, s *store.Store, traceID, orderID string, ratesStatus int, billing bool) int64 {
	t.Helper()
	start := time.Now().UTC()

	insert := func(target, method, path string, status int, offset, dur time.Duration, body string) int64 {
		t.Helper()
		id, err := s.Insert(context.Background(), &store.Call{
			Target: target, Protocol: "http", Method: method, Path: path,
			Status: status, ClientAddr: "127.0.0.1:1", TraceID: traceID,
			StartedAt: start.Add(offset), Duration: dur,
			Request:  store.Message{Headers: http.Header{}, Body: []byte(`{}`), Size: 2},
			Response: store.Message{Headers: http.Header{}, Body: []byte(body), Size: int64(len(body))},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// The entry point spans the whole run, so temporal nesting would also find
	// this shape if the trace id were ever dropped.
	root := insert("gateway", "POST", "/orders/"+orderID, 200, 0, 100*time.Millisecond, `{"ok":true}`)
	insert("ms-rates", "GET", "/rates/"+orderID, ratesStatus, 2*time.Millisecond, 20*time.Millisecond, `{"rate":9}`)
	if billing {
		insert("ms-billing", "POST", "/invoices", 200, 40*time.Millisecond, 20*time.Millisecond, `{"invoice":"X"}`)
	}
	return root
}

func TestFlowDiffFindsTheCallThatChanged(t *testing.T) {
	h, s := newServer(t)
	a := insertRun(t, s, "trace-a", "ORD-1", 200, true)
	b := insertRun(t, s, "trace-b", "ORD-2", 500, true)

	code, body := get(t, h, "/api/flowdiff?a="+strconv.FormatInt(a, 10)+"&b="+strconv.FormatInt(b, 10))
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}

	if body["same_entry"] != true {
		t.Error("two runs of the same flow were not recognised as the same entry point")
	}
	// The ids in the paths differ between runs. If normalisation were not doing
	// its job nothing would align and every call would be reported as unmatched.
	if unmatched := body["unmatched"].(float64); unmatched != 0 {
		t.Errorf("unmatched = %v, so the paths did not align at all", unmatched)
	}

	divergence, ok := body["divergence"].([]any)
	if !ok || len(divergence) == 0 {
		t.Fatalf("no divergence reported for a run where a call started failing: %v", body["divergence"])
	}
	if last := divergence[len(divergence)-1].(string); last != "ms-rates http GET /rates/{}" {
		t.Errorf("divergence points at %q, want the call that started returning 500", last)
	}
}

func TestFlowDiffReportsACallThatStoppedHappening(t *testing.T) {
	h, s := newServer(t)
	a := insertRun(t, s, "trace-a", "ORD-1", 200, true)
	b := insertRun(t, s, "trace-b", "ORD-2", 200, false)

	_, body := get(t, h, "/api/flowdiff?a="+strconv.FormatInt(a, 10)+"&b="+strconv.FormatInt(b, 10))

	if body["only_in_a"].(float64) != 1 {
		t.Errorf("only_in_a = %v, want the call that is no longer made", body["only_in_a"])
	}
	if body["divergence"] == nil {
		t.Error("a call that stopped happening did not count as a divergence")
	}
}

func TestFlowDiffComparesPayloadsOnlyWhereItMatters(t *testing.T) {
	h, s := newServer(t)
	a := insertRun(t, s, "trace-a", "ORD-1", 200, true)
	b := insertRun(t, s, "trace-b", "ORD-2", 500, true)
	base := "/api/flowdiff?a=" + strconv.FormatInt(a, 10) + "&b=" + strconv.FormatInt(b, 10)

	// The default reads the divergence and its children, not the whole flow:
	// a wide run would otherwise mean dozens of payload reads nobody asked for.
	_, first := get(t, h, base)
	firstBodies := len(first["bodies"].([]any))

	_, all := get(t, h, base+"&bodies=all")
	allBodies := len(all["bodies"].([]any))

	_, none := get(t, h, base+"&bodies=none")

	if firstBodies == 0 {
		t.Error("the default compared no payloads at all, so the divergence has no evidence")
	}
	if allBodies <= firstBodies {
		t.Errorf("bodies=all compared %d payloads and the default compared %d; the default is not narrowing anything", allBodies, firstBodies)
	}
	if got := len(none["bodies"].([]any)); got != 0 {
		t.Errorf("bodies=none still compared %d payloads", got)
	}
}

func TestFlowDiffRejectsAnUnknownNormalisation(t *testing.T) {
	h, s := newServer(t)
	a := insertRun(t, s, "trace-a", "ORD-1", 200, true)
	b := insertRun(t, s, "trace-b", "ORD-2", 200, true)

	code, _ := get(t, h, "/api/flowdiff?a="+strconv.FormatInt(a, 10)+"&b="+strconv.FormatInt(b, 10)+"&normalize=sort-of")
	if code != http.StatusBadRequest {
		t.Errorf("status = %d for an unknown normalize value; a typo would silently compare with the default", code)
	}
}

func TestFlowDiffSaysWhenASeedDoesNotExist(t *testing.T) {
	h, s := newServer(t)
	a := insertRun(t, s, "trace-a", "ORD-1", 200, true)

	code, _ := get(t, h, "/api/flowdiff?a="+strconv.FormatInt(a, 10)+"&b=99999")
	if code != http.StatusNotFound {
		t.Errorf("status = %d for a call id that does not exist", code)
	}
}

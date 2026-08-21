package store

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleCall(target, method, path string, status int, body []byte) *Call {
	return &Call{
		Target:     target,
		Protocol:   "http",
		Method:     method,
		Path:       path,
		Status:     status,
		ClientAddr: "127.0.0.1:5555",
		StartedAt:  time.Now().UTC(),
		Duration:   12 * time.Millisecond,
		Request: Message{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    body,
			Size:    int64(len(body)),
		},
		Response: Message{
			Headers: http.Header{"X-Trace": []string{"t-1"}},
			Body:    []byte(`{"ok":true}`),
			Size:    11,
		},
	}
}

// The three states of Filter.Failed, named so a test reads as the question it
// is asking rather than as an address-of.
var yes, no = true, false

// The filter has three states and each one has to be a different answer.
// It had two — true and "everything" — so asking for the calls that worked
// handed back the failures alongside them, which is the opposite of the truth
// and reads as confirmation that nothing is wrong.
//
// The GraphQL capture is the one that matters: it is HTTP 200, so a filter that
// only knew the status column would call it a success in both directions and
// pass this test for the wrong reason.
func TestTheFailedFilterHasThreeStates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	clean := sampleCall("gateway", "GET", "/v1/orders", 200, []byte(`{}`))
	server := sampleCall("gateway", "GET", "/v1/pay", 500, []byte(`{}`))
	graphql := sampleCall("gateway", "POST", "/graphql", 200,
		[]byte(`{"query":"mutation Pay { pay { ok } }"}`))
	graphql.Response.Body = []byte(`{"data":null,"errors":[{"message":"card declined"}]}`)

	for _, c := range []*Call{clean, server, graphql} {
		if _, err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if graphql.GraphQLErrors != 1 {
		t.Fatalf("the GraphQL capture was stored with %d errors, so the 200 case is not being exercised", graphql.GraphQLErrors)
	}

	paths := func(f Filter) []string {
		t.Helper()
		got, err := s.List(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got))
		for _, c := range got {
			out = append(out, c.Path)
		}
		sort.Strings(out)
		return out
	}

	for _, tc := range []struct {
		name string
		f    Filter
		want []string
	}{
		{"absent is no filter", Filter{}, []string{"/graphql", "/v1/orders", "/v1/pay"}},
		{"true is only the failures", Filter{Failed: &yes}, []string{"/graphql", "/v1/pay"}},
		{"false is only what worked", Filter{Failed: &no}, []string{"/v1/orders"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := paths(tc.f); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Bytes in, identical bytes out. Replay depends on this and nothing else.
func TestInsertAndGetPreserveBodiesExactly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Deliberately not valid UTF-8: a stored body must survive the round trip
	// even when it cannot be rendered as text.
	body := []byte{0x08, 0x96, 0x01, 0x00, 0xff, 0xfe, 'h', 'i'}
	in := sampleCall("ms-executive", "POST", "/v1/quotes", 201, body)
	in.Request.Truncated = true

	id, err := s.Insert(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Request.Body) != string(body) {
		t.Errorf("request body changed: %v", out.Request.Body)
	}
	if !out.Request.Truncated {
		t.Error("truncated flag was lost")
	}
	if out.Request.Headers.Get("Content-Type") != "application/json" {
		t.Error("request headers were lost")
	}
	if out.Response.Headers.Get("X-Trace") != "t-1" {
		t.Error("response headers were lost")
	}
	if out.Status != 201 || out.Method != "POST" || out.Target != "ms-executive" {
		t.Errorf("metadata changed: %+v", out)
	}
	if out.Duration != 12*time.Millisecond {
		t.Errorf("duration = %v", out.Duration)
	}
}

func TestListFilters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	mustInsert(t, s, sampleCall("api", "GET", "/orders", 200, []byte(`{"page":1}`)))
	mustInsert(t, s, sampleCall("api", "POST", "/orders", 500, []byte(`{"sku":"ABC-9"}`)))
	mustInsert(t, s, sampleCall("billing", "GET", "/invoices", 200, []byte(`{"page":2}`)))

	cases := []struct {
		name string
		f    Filter
		want int
	}{
		{"no filter", Filter{}, 3},
		{"by target", Filter{Target: "api"}, 2},
		{"by method", Filter{Method: "post"}, 1},
		{"by status", Filter{Status: 500}, 1},
		{"by path fragment", Filter{Path: "invoice"}, 1},
		{"by target and status", Filter{Target: "api", Status: 200}, 1},
		{"no match", Filter{Target: "nope"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.List(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Errorf("got %d calls, want %d", len(got), tc.want)
			}
		})
	}
}

func TestListIsNewestFirstAndPaginates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		mustInsert(t, s, sampleCall("api", "GET", "/orders", 200, nil))
	}

	page, err := s.List(ctx, Filter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("got %d calls, want 2", len(page))
	}
	if page[0].ID <= page[1].ID {
		t.Error("expected newest first")
	}

	next, err := s.List(ctx, Filter{Limit: 2, BeforeID: page[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 || next[0].ID >= page[1].ID {
		t.Errorf("cursor did not advance: %+v", next)
	}
}

// Search terms in this tool are paths and JSON fragments, which are full of
// characters FTS5 would otherwise read as operators.
func TestSearchMatchesBodiesAndSurvivesPunctuation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustInsert(t, s, sampleCall("api", "POST", "/v1/orders", 200, []byte(`{"sku":"ABC-9","qty":3}`)))
	mustInsert(t, s, sampleCall("api", "POST", "/v1/orders", 200, []byte(`{"sku":"XYZ-1","qty":7}`)))

	for _, q := range []string{"ABC-9", `"sku":"ABC-9"`, "/v1/orders"} {
		got, err := s.List(ctx, Filter{Search: q})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) == 0 {
			t.Errorf("search %q found nothing", q)
		}
	}

	got, err := s.List(ctx, Filter{Search: "ABC-9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("search should have matched one call, got %d", len(got))
	}
}

// A single badly encoded byte must not make a call unfindable. This is the
// normal case against a Spanish-language service that never got its charset
// right, and it went unnoticed until a real capture was searched.
func TestSearchFindsTextWithBrokenEncoding(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// "Nicolás" as latin-1: valid text, invalid UTF-8.
	latin1 := []byte(`{"cliente":"Nicol` + "\xe1" + `s","sku":"ABC-9"}`)
	mustInsert(t, s, sampleCall("api", "POST", "/v1/orders", 200, latin1))

	for _, q := range []string{"cliente", "ABC-9", "sku"} {
		got, err := s.List(ctx, Filter{Search: q})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(got) != 1 {
			t.Errorf("search %q found %d calls, want 1", q, len(got))
		}
	}

	// The stored bytes stay untouched: only the index is sanitized.
	out, err := s.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Request.Body) != string(latin1) {
		t.Error("the stored body must keep the original bytes")
	}
}

// Binary payloads stay out of the index; they produce noise, not matches.
func TestSearchSkipsBinaryBodies(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	binary := []byte{0x08, 0x96, 0x01, 0x00, 0x1a, 0x07, 'm', 'a', 'r', 'k', 'e', 'r', 0x00}
	mustInsert(t, s, sampleCall("api", "POST", "/grpc", 200, binary))

	got, err := s.List(ctx, Filter{Search: "marker"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Error("a binary body should not be indexed")
	}
	// The call itself is still findable by its metadata.
	if got, err := s.List(ctx, Filter{Search: "/grpc"}); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Error("the call should still be findable by path")
	}
}

func TestPruneByAgeAndCount(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	old := sampleCall("api", "GET", "/old", 200, []byte("old-marker"))
	old.StartedAt = time.Now().Add(-2 * time.Hour)
	mustInsert(t, s, old)
	for i := 0; i < 4; i++ {
		mustInsert(t, s, sampleCall("api", "GET", "/new", 200, nil))
	}

	deleted, err := s.Prune(ctx, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("age prune deleted %d, want 1", deleted)
	}

	// The search index must shrink with the table, or a pruned call keeps
	// showing up in results with no row behind it.
	if got, err := s.List(ctx, Filter{Search: "old-marker"}); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Error("pruned call is still in the search index")
	}

	deleted, err = s.Prune(ctx, time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("count prune deleted %d, want 2", deleted)
	}
	remaining, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Errorf("%d calls remain, want 2", len(remaining))
	}
}

func TestStats(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustInsert(t, s, sampleCall("api", "GET", "/a", 200, nil))
	mustInsert(t, s, sampleCall("api", "GET", "/b", 200, nil))

	st, err := s.Stats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Calls != 2 {
		t.Errorf("calls = %d, want 2", st.Calls)
	}
	if st.Oldest.IsZero() || st.Newest.IsZero() {
		t.Error("expected oldest and newest timestamps")
	}
}

func mustInsert(t *testing.T, s *Store, c *Call) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A GraphQL failure arrives under HTTP 200, so the SQL that decides what counts
// as a fault cannot see it in the status. The operation and the error count are
// read off the bodies on insert, which is what puts them within reach of a
// listing that carries no bodies at all.
func TestAGraphQLErrorIsAFaultInSQL(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	broken := sampleCall("gateway", "POST", "/graphql", 200,
		[]byte(`{"query":"mutation Pay { pay { ok } }"}`))
	broken.Response.Body = []byte(`{"data":null,"errors":[{"message":"card declined"}]}`)
	fine := sampleCall("gateway", "POST", "/graphql", 200, []byte(`{"query":"{ me { name } }"}`))

	for _, c := range []*Call{broken, fine} {
		if _, err := s.Insert(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	if broken.GraphQLOp != "mutation Pay" || broken.GraphQLErrors != 1 {
		t.Errorf("derived %q / %d errors", broken.GraphQLOp, broken.GraphQLErrors)
	}
	// The bytes are the record: the reading taken off them may not touch them.
	if string(broken.Request.Body) != `{"query":"mutation Pay { pay { ok } }"}` {
		t.Errorf("the stored request body was altered: %s", broken.Request.Body)
	}

	failed, err := s.List(ctx, Filter{Failed: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].GraphQLOp != "mutation Pay" {
		t.Fatalf("the failed filter returned %+v, want only the declined payment", failed)
	}

	stats, err := s.Stats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.ByTarget) != 1 || stats.ByTarget[0].Faults != 1 {
		t.Errorf("the rail counted %+v, so the rail and the field disagree", stats.ByTarget)
	}
}

// The comment on Summary() names exactly this failure mode: stub_of once
// reached the listing and never reached the detail, because it was wired into
// only one of the two SELECT statements. TraceIDInjected touches Insert, List,
// Get and Summary — four places to miss it in — so all four are asserted here
// rather than trusting that adding a struct field was enough.
func TestTraceIDInjectedSurvivesEverySurface(t *testing.T) {
	s := newStore(t)
	call := sampleCall("gateway", "GET", "/orders", 200, nil)
	call.TraceID = "sonda-abc123"
	call.TraceIDInjected = true

	id, err := s.Insert(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TraceIDInjected {
		t.Error("Get did not report the trace id as injected")
	}

	listed, err := s.List(context.Background(), Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].TraceIDInjected {
		t.Errorf("List did not carry the flag through to the summary: %+v", listed)
	}

	if summary := got.Summary(); !summary.TraceIDInjected {
		t.Error("Call.Summary() dropped the flag on the way to the listing shape")
	}
}

// A trace id the client actually sent must never be reported as Sonda's own —
// that would tell a reader to distrust instrumentation that was real.
func TestAClientsOwnTraceIDIsNeverReportedAsInjected(t *testing.T) {
	s := newStore(t)
	call := sampleCall("gateway", "GET", "/orders", 200, nil)
	call.TraceID = "client-sent-this"

	id, err := s.Insert(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TraceIDInjected {
		t.Error("a real client trace id was reported as injected")
	}
}

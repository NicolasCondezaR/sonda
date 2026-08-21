package trace

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// at builds a call starting `startMS` after the base and lasting `durMS`,
// which is the only thing the arranging cares about.
func at(id int64, target string, startMS, durMS int) Call {
	return Call{
		ID:       id,
		Target:   target,
		Started:  base.Add(time.Duration(startMS) * time.Millisecond),
		Duration: time.Duration(durMS) * time.Millisecond,
	}
}

func withTrace(c Call, id string) Call { c.TraceID = id; return c }

// find walks a tree looking for a call, so a test can assert on where
// something ended up rather than on the exact slice indices it took to get
// there.
func find(n *Node, id int64) *Node {
	if n.Call.ID == id {
		return n
	}
	for _, child := range n.Children {
		if got := find(child, id); got != nil {
			return got
		}
	}
	return nil
}

func single(t *testing.T, trees []Tree) Tree {
	t.Helper()
	if len(trees) != 1 {
		t.Fatalf("%d trees built, want 1", len(trees))
	}
	return trees[0]
}

// --- reading the id out of the headers ---

func TestTraceIDIsReadFromTheUsualHeaders(t *testing.T) {
	cases := []struct {
		header, value, want string
	}{
		{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"X-Request-Id", "req-abc-123", "req-abc-123"},
		{"x-correlation-id", "corr-9", "corr-9"},
		{"X-B3-TraceId", "80f198ee56343ba8", "80f198ee56343ba8"},
	}
	for _, c := range cases {
		h := http.Header{}
		h.Set(c.header, c.value)
		if got := ID(h); got != c.want {
			t.Errorf("%s: got %q, want %q", c.header, got, c.want)
		}
	}

	if got := ID(http.Header{}); got != "" {
		t.Errorf("headers with nothing to find returned %q", got)
	}
}

// Something malformed is still something trying to trace. Using it opaquely
// groups the calls correctly, which is the whole job.
func TestAMalformedTraceparentIsStillUsable(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "not-really-a-traceparent")
	if got := ID(h); got != "not-really-a-traceparent" {
		t.Errorf("got %q, want the raw value kept", got)
	}
}

// --- arranging by nesting ---

// The shape this exists for: one click that touched several services.
func TestACallThatHappensInsideAnotherIsItsChild(t *testing.T) {
	trees := Build([]Call{
		at(1, "portal", 0, 1000),
		at(2, "ms-auth", 10, 20),
		at(3, "ms-rates", 40, 300),
		at(4, "ms-schedules", 60, 200), // inside ms-rates
	})

	tree := single(t, trees)
	if tree.Root.Call.ID != 1 {
		t.Fatalf("root is call %d, want the outermost", tree.Root.Call.ID)
	}
	if tree.Calls != 4 {
		t.Errorf("tree holds %d calls, want 4", tree.Calls)
	}

	rates := find(tree.Root, 3)
	if rates == nil {
		t.Fatal("ms-rates is not in the tree")
	}
	if len(rates.Children) != 1 || rates.Children[0].Call.ID != 4 {
		t.Errorf("ms-schedules did not hang off ms-rates: %+v", rates.Children)
	}
	// Two levels down, not flattened onto the root.
	if len(tree.Root.Children) != 2 {
		t.Errorf("root has %d children, want auth and rates only", len(tree.Root.Children))
	}
}

// Merely overlapping is not causing. Two calls that run alongside each other
// are siblings, and calling one the parent of the other invents a relationship.
func TestOverlappingCallsAreNotNested(t *testing.T) {
	trees := Build([]Call{
		at(1, "a", 0, 100),
		at(2, "b", 50, 100), // starts inside a, ends after it
	})
	if len(trees) != 2 {
		t.Fatalf("%d trees, want 2 — neither call contains the other", len(trees))
	}
}

// Without a trace id the grouping itself is a guess, and saying so is the
// difference between a useful tree and a misleading one.
func TestATreeBuiltFromTimingSaysItIsAGuess(t *testing.T) {
	tree := single(t, Build([]Call{
		at(1, "portal", 0, 100),
		at(2, "ms-auth", 10, 20),
	}))

	if tree.Certain {
		t.Error("a tree grouped by timing alone claims to be certain")
	}
	child := find(tree.Root, 2)
	if !child.Inferred {
		t.Error("the link to the parent is not marked as inferred")
	}
	if !strings.Contains(Render(tree), "inferred") {
		t.Error("the rendering does not warn that the shape was guessed")
	}
}

// A trace id makes the grouping a fact, and the tree should stop apologising.
func TestATraceIDMakesTheGroupingCertain(t *testing.T) {
	tree := single(t, Build([]Call{
		withTrace(at(1, "portal", 0, 100), "abc"),
		withTrace(at(2, "ms-auth", 10, 20), "abc"),
	}))

	if !tree.Certain {
		t.Error("calls sharing a trace id were not treated as certainly related")
	}
	if tree.TraceID != "abc" {
		t.Errorf("trace id is %q", tree.TraceID)
	}
	if find(tree.Root, 2).Inferred {
		t.Error("a link inside a real trace is marked as guessed")
	}
}

// Two requests with different ids must not be merged, even when they overlap
// in time — which they will, because that is what concurrency looks like.
func TestDifferentTracesStaySeparateEvenWhenTheyOverlap(t *testing.T) {
	trees := Build([]Call{
		withTrace(at(1, "portal", 0, 100), "uno"),
		withTrace(at(2, "ms-auth", 10, 20), "uno"),
		withTrace(at(3, "portal", 5, 100), "dos"),
		withTrace(at(4, "ms-seo", 15, 20), "dos"),
	})

	if len(trees) != 2 {
		t.Fatalf("%d trees, want one per trace id", len(trees))
	}
	for _, tree := range trees {
		if tree.Calls != 2 {
			t.Errorf("trace %s holds %d calls, want 2", tree.TraceID, tree.Calls)
		}
	}
}

// Concurrent unrelated traffic is what makes the guess unreliable, so a branch
// that could have gone either way has to say so.
//
// The two candidates must genuinely overlap without either containing the
// other — two people clicking at once. When one does contain the other they
// are a chain, not a choice, and there is nothing ambiguous about it.
func TestAnAmbiguousParentIsFlagged(t *testing.T) {
	trees := Build([]Call{
		at(1, "portal-a", 0, 1000),  //   0 … 1000
		at(2, "portal-b", 50, 1150), //  50 … 1200, neither contains the other
		at(3, "ms-auth", 100, 10),   // 100 …  110, inside both
	})

	var child *Node
	for _, tree := range trees {
		if got := find(tree.Root, 3); got != nil {
			child = got
			if !strings.Contains(Render(tree), "another call") {
				t.Error("the rendering hides the ambiguity")
			}
		}
	}
	if child == nil {
		t.Fatal("the inner call is in no tree at all")
	}
	if !child.Ambiguous {
		t.Error("two possible parents and the branch is not flagged as ambiguous")
	}
}

// The other half of the same rule: nested candidates are a chain, and flagging
// that as ambiguous would put a warning on almost every real tree.
func TestNestedCandidatesAreNotAmbiguous(t *testing.T) {
	tree := single(t, Build([]Call{
		at(1, "portal", 0, 1000),  // 0 … 1000
		at(2, "gateway", 10, 900), // inside portal
		at(3, "ms-auth", 100, 10), // inside both, but they are a chain
	}))

	child := find(tree.Root, 3)
	if child == nil {
		t.Fatal("the inner call is not in the tree")
	}
	if child.Ambiguous {
		t.Error("a plain chain of nested calls was flagged as ambiguous")
	}
	if child.Call.ID != 3 || find(tree.Root, 2).Children[0].Call.ID != 3 {
		t.Error("ms-auth did not hang off the innermost call that contains it")
	}
}

// The count is what a person reads first: seven calls, one of them broken.
func TestFailuresAreCountedThroughTheWholeTree(t *testing.T) {
	broken := at(3, "ms-executive", 60, 40)
	broken.Failed = true
	broken.Detail = "InvalidArgument"

	tree := single(t, Build([]Call{
		at(1, "portal", 0, 200),
		at(2, "ms-auth", 10, 20),
		broken,
	}))

	if tree.Failed != 1 {
		t.Errorf("counted %d failures, want 1", tree.Failed)
	}
	rendered := Render(tree)
	if !strings.Contains(rendered, "✗") {
		t.Error("the failure is not marked in the rendering")
	}
	if !strings.Contains(rendered, "InvalidArgument") {
		t.Error("the rendering does not say why it failed")
	}
}

func TestRenderDrawsTheNesting(t *testing.T) {
	tree := single(t, Build([]Call{
		at(1, "portal", 0, 1000),
		at(2, "ms-auth", 10, 20),
		at(3, "ms-rates", 40, 300),
		at(4, "ms-schedules", 60, 200),
	}))

	rendered := Render(tree)
	for _, want := range []string{"portal", "ms-auth", "ms-rates", "ms-schedules", "└─", "├─"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, rendered)
		}
	}
	// The deepest call has to be drawn deeper than its parent, or the drawing
	// conveys nothing the flat list did not.
	lines := strings.Split(strings.TrimSpace(rendered), "\n")
	var rates, schedules string
	for _, l := range lines {
		if strings.Contains(l, "ms-rates") {
			rates = l
		}
		if strings.Contains(l, "ms-schedules") {
			schedules = l
		}
	}
	if len(schedules)-len(strings.TrimLeft(schedules, " │├└─")) <= len(rates)-len(strings.TrimLeft(rates, " │├└─")) {
		t.Errorf("ms-schedules is not drawn deeper than ms-rates:\n%s", rendered)
	}
}

func TestNothingToArrangeIsNotACrash(t *testing.T) {
	if trees := Build(nil); len(trees) != 0 {
		t.Errorf("%d trees from no calls", len(trees))
	}
	if trees := Build([]Call{}); len(trees) != 0 {
		t.Errorf("%d trees from an empty slice", len(trees))
	}
}

func TestInjectWritesAnIDOnlyWhenNoneIsPresent(t *testing.T) {
	h := http.Header{}
	id, injected := Inject(h)
	if !injected || id == "" {
		t.Fatal("Inject did not write an id onto empty headers")
	}
	if got := h.Get(InjectedHeader); got != id {
		t.Errorf("header %s = %q, want the returned id %q", InjectedHeader, got, id)
	}
	if !strings.HasPrefix(id, "sonda-") {
		t.Errorf("id %q does not carry the sonda- prefix that marks it as synthetic", id)
	}
}

// A client's own correlation always outranks a guess that it needs one from
// Sonda — however the id is spelled, not only the headers ID already reads.
func TestInjectNeverOverwritesAnExistingID(t *testing.T) {
	for _, header := range []string{"X-Request-Id", "X-Correlation-Id", "Traceparent"} {
		h := http.Header{}
		h.Set(header, "already-here")
		id, injected := Inject(h)
		if injected {
			t.Errorf("%s: Inject reported writing an id that was already present", header)
		}
		if id != "already-here" && header != "Traceparent" {
			t.Errorf("%s: Inject returned %q instead of the existing id", header, id)
		}
	}
}

func TestInjectIsIdempotentAcrossCalls(t *testing.T) {
	h := http.Header{}
	first, _ := Inject(h)
	second, injected := Inject(h)
	if injected {
		t.Fatal("a second Inject on the same headers reported writing a new id")
	}
	if first != second {
		t.Errorf("two calls to Inject on the same headers disagreed: %q then %q", first, second)
	}
}

func TestTwoInjectionsNeverCollide(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, _ := Inject(http.Header{})
		if seen[id] {
			t.Fatalf("Inject produced the same id twice: %q", id)
		}
		seen[id] = true
	}
}

func TestTheTreeSaysWhenATraceIDCameFromSonda(t *testing.T) {
	c := at(1, "gateway", 0, 100)
	c.TraceIDInjected = true
	tree := single(t, Build([]Call{c}))

	if !strings.Contains(Render(tree), "[trace id from Sonda]") {
		t.Errorf("a call whose trace id Sonda wrote is indistinguishable from one the client sent:\n%s", Render(tree))
	}
}

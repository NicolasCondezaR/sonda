package flowdiff

import (
	"strings"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/trace"
)

// node is the shorthand the tests are written in: a call is its target, method
// and path, and the tree is the shape.
func node(target, method, path string, children ...*trace.Node) *trace.Node {
	return &trace.Node{
		Call:     trace.Call{Target: target, Protocol: "http", Method: method, Path: path, Status: 200},
		Children: children,
	}
}

func tree(root *trace.Node) trace.Tree {
	return trace.Tree{Root: root, Certain: true}
}

// run is the flow both sides of most tests start from: a gateway call that
// fans out to two services, one of which calls a third.
func run() *trace.Node {
	return node("gateway", "POST", "/orders",
		node("ms-rates", "GET", "/rates/CL",
			node("ms-schedules", "GET", "/schedules/7"),
		),
		node("ms-billing", "POST", "/invoices"),
	)
}

func TestIdenticalRunsHaveNoDivergence(t *testing.T) {
	got := Compare(tree(run()), tree(run()), Strict)

	if !got.SameEntry {
		t.Fatal("the same entry point was not recognised as the same call")
	}
	if got.Divergence != nil {
		t.Fatalf("identical runs diverged at %v", got.Divergence)
	}
	if got.Matched != 4 || got.OnlyInA != 0 || got.OnlyInB != 0 {
		t.Fatalf("matched=%d only_in_a=%d only_in_b=%d, want 4/0/0", got.Matched, got.OnlyInA, got.OnlyInB)
	}
}

func TestIdsInThePathDoNotStopTwoRunsMatching(t *testing.T) {
	a := node("gateway", "GET", "/orders/ORD-1",
		node("ms-billing", "GET", "/invoices/4821"),
	)
	b := node("gateway", "GET", "/orders/ORD-2",
		node("ms-billing", "GET", "/invoices/9930"),
	)

	got := Compare(tree(a), tree(b), Strict)
	if got.Matched != 2 || got.OnlyInA != 0 || got.OnlyInB != 0 {
		t.Fatalf("matched=%d only_in_a=%d only_in_b=%d, want 2/0/0", got.Matched, got.OnlyInA, got.OnlyInB)
	}

	// And with normalisation off, the very same runs must stop matching —
	// otherwise the knob does nothing and the strict pass proved nothing.
	off := Compare(tree(a), tree(b), Off)
	if off.Matched != 1 || off.OnlyInA != 1 || off.OnlyInB != 1 {
		t.Fatalf("with normalize=off matched=%d only_in_a=%d only_in_b=%d, want 1/1/1",
			off.Matched, off.OnlyInA, off.OnlyInB)
	}
}

func TestADeepStatusChangeIsTheDivergence(t *testing.T) {
	b := run()
	b.Children[0].Children[0].Call.Status = 500
	b.Children[0].Children[0].Call.Failed = true

	got := Compare(tree(run()), tree(b), Strict)

	want := "ms-schedules http GET /schedules/{}"
	if len(got.Divergence) == 0 || got.Divergence[len(got.Divergence)-1] != want {
		t.Fatalf("divergence = %v, want it to end at %q", got.Divergence, want)
	}
	if len(got.Divergence) != 3 {
		t.Fatalf("divergence = %v, want the full path from the root", got.Divergence)
	}
}

func TestACallThatStoppedHappeningIsFound(t *testing.T) {
	b := run()
	b.Children = b.Children[:1] // ms-billing is never called any more

	got := Compare(tree(run()), tree(b), Strict)

	if got.OnlyInA != 1 {
		t.Fatalf("only_in_a = %d, want the dropped call to be counted once", got.OnlyInA)
	}
	last := got.Divergence[len(got.Divergence)-1]
	if !strings.HasPrefix(last, "ms-billing ") {
		t.Fatalf("divergence = %v, want it to point at the dropped call", got.Divergence)
	}
	// The gap belongs to the parent both runs agreed on, so the path is the
	// root and then the missing child — not a path through a node that is not
	// in both runs.
	if len(got.Divergence) != 2 {
		t.Fatalf("divergence = %v, want root then the gap", got.Divergence)
	}
}

func TestANewCallCarriesItsWholeSubtree(t *testing.T) {
	b := run()
	b.Children = append(b.Children, node("ms-trade", "GET", "/quotes",
		node("ms-rates", "GET", "/rates/AR"),
	))

	got := Compare(tree(run()), tree(b), Strict)

	if got.OnlyInB != 2 {
		t.Fatalf("only_in_b = %d, want the new call and the one it caused", got.OnlyInB)
	}
	if got.OnlyInA != 0 {
		t.Fatalf("only_in_a = %d, want nothing missing from the second run", got.OnlyInA)
	}
}

func TestAStubbedBranchIsSaidOutLoud(t *testing.T) {
	b := run()
	b.Children[1].Call.Stubbed = true

	got := Compare(tree(run()), tree(b), Strict)

	var found bool
	for _, c := range got.Root.Children {
		for _, ch := range c.Changes {
			if ch.Field == FieldStubbed {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("a branch answered from a recording in one run only was not reported")
	}
}

func TestGuessedTreesAreNotPresentedAsFacts(t *testing.T) {
	a, b := tree(run()), tree(run())
	b.Certain = false

	if Compare(a, b, Strict).Certain {
		t.Fatal("a comparison against an inferred tree claimed to be certain")
	}
}

func TestComparingTwoDifferentFlowsSaysSo(t *testing.T) {
	b := node("gateway", "GET", "/health")

	if Compare(tree(run()), tree(b), Strict).SameEntry {
		t.Fatal("two unrelated entry points were reported as the same call")
	}
}

func TestAnOperationOutranksThePath(t *testing.T) {
	// Both are POSTs to the same endpoint, which is every GraphQL request ever;
	// only the operation tells them apart.
	a := &trace.Node{Call: trace.Call{Target: "bff", Protocol: "graphql", Method: "POST", Path: "/graphql", Op: "GetOrder"}}
	b := &trace.Node{Call: trace.Call{Target: "bff", Protocol: "graphql", Method: "POST", Path: "/graphql", Op: "CancelOrder"}}

	if signature(a.Call, Strict) == signature(b.Call, Strict) {
		t.Fatal("two different GraphQL operations produced the same signature")
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		path string
		mode Normalize
		want string
	}{
		{"/orders/42", Strict, "/orders/{}"},
		{"/orders/ORD-1", Strict, "/orders/{}"},
		{"/invoices/INV_2026", Strict, "/invoices/{}"},
		{"/u/3f2b1c8a-1111-4222-8333-abcdefabcdef", Strict, "/u/{}"},
		{"/blob/0123456789abcdef01", Strict, "/blob/{}"},
		// A version is a route, not a value, and strict has to leave it alone.
		{"/v2/orders", Strict, "/v2/orders"},
		{"/v2/orders", Loose, "/{}/orders"},
		{"/orders/42", Off, "/orders/42"},
		// gRPC paths carry no values, so nothing should ever fire on one.
		{"/demo.v1.Orders/GetOrder", Strict, "/demo.v1.Orders/GetOrder"},
	}
	for _, c := range cases {
		if got := normalizePath(c.path, c.mode); got != c.want {
			t.Errorf("normalizePath(%q, %s) = %q, want %q", c.path, c.mode, got, c.want)
		}
	}
}

func TestAMissingRunIsNotNoDifference(t *testing.T) {
	got := Compare(tree(run()), trace.Tree{}, Strict)

	if got.OnlyInA != 4 {
		t.Fatalf("only_in_a = %d, want the whole run reported as missing", got.OnlyInA)
	}
	if got.Divergence == nil {
		t.Fatal("comparing against nothing reported no divergence")
	}
}

func TestRenderSaysWhatHappenedWithoutTheJSON(t *testing.T) {
	b := run()
	b.Children[0].Call.Status = 500
	b.Children[0].Call.Failed = true
	b.Children = b.Children[:1]

	out := Render(Compare(tree(run()), tree(b), Strict))

	for _, want := range []string{
		"first divergence:",
		"status: 200 → 500",
		"only in a — this call is no longer made",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering never says %q:\n%s", want, out)
		}
	}
}

func TestRenderSaysWhenThereIsNothingToReport(t *testing.T) {
	out := Render(Compare(tree(run()), tree(run()), Strict))
	if !strings.Contains(out, "no divergence") {
		t.Errorf("two identical runs did not render as identical:\n%s", out)
	}
}

func TestRenderWarnsWhenTheAlignmentIsTheProblem(t *testing.T) {
	// Paths full of ids, compared literally: almost nothing pairs up, and the
	// reader has to be told that is a knob and not a finding.
	a := node("gateway", "GET", "/orders/1", node("ms-rates", "GET", "/rates/1"))
	b := node("gateway", "GET", "/orders/1", node("ms-rates", "GET", "/rates/2"))

	out := Render(Compare(tree(a), tree(b), Off))
	if !strings.Contains(out, "normalize=loose") {
		t.Errorf("a comparison where most calls went unpaired did not point at the knob:\n%s", out)
	}
}

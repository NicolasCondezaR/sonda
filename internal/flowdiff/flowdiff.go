// Package flowdiff compares two runs of the same flow.
//
// Comparing two calls answers "why did this request fail and that one work".
// It does not answer the question people actually arrive with, which is "this
// checkout worked yesterday and today it does not" — because a flow is a tree
// of calls, and the thing that changed is usually several hops down it, or is
// a call that stopped happening at all.
//
// Two runs cannot be aligned by id or by trace id: those are exactly what
// differs between them. They are aligned by what a call *is* — the service, the
// protocol, the method and the shape of the path — and then by position among
// the siblings that share that signature.
//
// The alignment is deliberately the explainable one rather than the optimal
// one. A tree edit distance would match more pairs in odd cases, and nobody
// could be told why two nodes were considered the same call.
package flowdiff

import (
	"regexp"
	"strings"

	"github.com/NicolasCondezaR/sonda/internal/trace"
)

// Normalize says how hard to look at a path segment before deciding it is a
// value rather than a route. Off compares paths literally, which is right when
// the routes carry no ids at all and wrong the moment one does.
type Normalize string

const (
	// Strict wildcards a segment only when it is unambiguously a value: all
	// digits, a UUID, a long hex string, or an identifier with a separator in
	// it such as ORD-1 or INV_2026.
	Strict Normalize = "strict"
	// Loose additionally wildcards any segment containing a digit. It matches
	// more, and it will happily flatten an API version: /v2/orders and
	// /v3/orders become the same call. That is why it is not the default.
	Loose Normalize = "loose"
	// Off compares paths as they were captured.
	Off Normalize = "off"
)

// The fields worth reporting when two aligned calls disagree. Duration is
// deliberately not one of them: it differs on every run, and a divergence list
// that flagged it would flag every node.
const (
	FieldStatus  = "status"
	FieldFailed  = "failed"
	FieldDetail  = "detail"
	FieldStubbed = "stubbed"
)

// Change is one field of an aligned pair that does not agree.
type Change struct {
	Field string `json:"field"`
	A     any    `json:"a,omitempty"`
	B     any    `json:"b,omitempty"`
}

// Pair is one call as seen in both runs. Exactly one of A and B is nil when the
// call happened in only one of them, which is half of what a reader came for:
// a call that stopped being made is invisible to any per-call comparison.
type Pair struct {
	Signature string `json:"signature"`
	// OnlyIn is "a" or "b" when the call is missing from the other run, and
	// empty when both runs have it.
	OnlyIn string `json:"only_in,omitempty"`

	A *trace.Call `json:"a,omitempty"`
	B *trace.Call `json:"b,omitempty"`

	// Changes is empty for a pair that agrees, and is always non-nil so a
	// client can count it without checking for null first.
	Changes []Change `json:"changes"`

	// Inferred says at least one side of this pair was attached to its parent
	// by timing rather than by a trace id, so the pairing inherits that guess.
	Inferred bool `json:"inferred,omitempty"`

	Children []Pair `json:"children,omitempty"`
}

// Result is the comparison of two runs.
type Result struct {
	// Certain is false when either run was grouped by temporal nesting instead
	// of a real trace id. The comparison still runs — it is often all there is
	// — but a difference between two guesses is not the same claim as a
	// difference between two facts, and the caller has to be able to say so.
	Certain bool `json:"certain"`

	// SameEntry is false when the two roots are not even the same call. The
	// comparison below is then almost certainly meaningless, and saying so once
	// at the top beats a reader working it out from a wall of only_in.
	SameEntry bool `json:"same_entry"`

	Root Pair `json:"root"`

	Matched int `json:"matched"`
	OnlyInA int `json:"only_in_a"`
	OnlyInB int `json:"only_in_b"`

	// Divergence is the path of signatures from the root to the first pair, in
	// preorder, that either disagrees or has a child missing from one side.
	// This is the answer to "where did it break"; everything else is evidence.
	Divergence []string `json:"divergence,omitempty"`

	Normalize Normalize `json:"normalize"`
}

// Compare aligns two runs and reports where they parted ways.
func Compare(a, b trace.Tree, mode Normalize) Result {
	out := Result{
		Certain:   a.Certain && b.Certain,
		Normalize: mode,
	}

	if a.Root == nil || b.Root == nil {
		// One side has nothing to compare. Report it as a missing entry rather
		// than as an empty tree, which would read as "no differences".
		out.Root = Pair{Changes: []Change{}}
		switch {
		case a.Root != nil:
			out.Root = leaf(a.Root, "a", mode)
		case b.Root != nil:
			out.Root = leaf(b.Root, "b", mode)
		default:
			return out
		}
		tally(out.Root, &out)
		out.Divergence = firstDivergence(out.Root, nil)
		return out
	}

	out.SameEntry = signature(a.Root.Call, mode) == signature(b.Root.Call, mode)
	out.Root = pair(a.Root, b.Root, mode)
	tally(out.Root, &out)
	out.Divergence = firstDivergence(out.Root, nil)
	return out
}

// pair builds the aligned node for two calls that are the same call, and then
// aligns whatever each of them caused.
func pair(a, b *trace.Node, mode Normalize) Pair {
	p := Pair{
		Signature: signature(a.Call, mode),
		A:         &a.Call,
		B:         &b.Call,
		Changes:   changes(a.Call, b.Call),
		Inferred:  a.Inferred || b.Inferred,
	}
	p.Children = align(a.Children, b.Children, mode)
	return p
}

// align matches siblings by signature and then by position within that
// signature. Order follows the left run, because a reader is usually holding it
// as the one that worked; calls that exist only on the right are appended
// rather than interleaved, since there is no honest place to put them.
//
// ponytail: positional matching inside a signature group. Two runs that made
// the same three calls in a different order pair them up wrongly; a
// content-aware assignment would fix that and cannot be explained in a
// sentence, which is worth more here than the extra pair.
func align(as, bs []*trace.Node, mode Normalize) []Pair {
	if len(as) == 0 && len(bs) == 0 {
		return nil
	}

	left, leftOrder := group(as, mode)
	right, rightOrder := group(bs, mode)

	var out []Pair
	seen := map[string]bool{}
	for _, sig := range leftOrder {
		seen[sig] = true
		la, lb := left[sig], right[sig]
		for i := 0; i < len(la) || i < len(lb); i++ {
			switch {
			case i < len(la) && i < len(lb):
				out = append(out, pair(la[i], lb[i], mode))
			case i < len(la):
				out = append(out, leaf(la[i], "a", mode))
			default:
				out = append(out, leaf(lb[i], "b", mode))
			}
		}
	}

	// Whatever the right run did that the left one never did at all.
	for _, sig := range rightOrder {
		if seen[sig] {
			continue
		}
		for _, n := range right[sig] {
			out = append(out, leaf(n, "b", mode))
		}
	}
	return out
}

func group(nodes []*trace.Node, mode Normalize) (map[string][]*trace.Node, []string) {
	by := map[string][]*trace.Node{}
	var order []string
	for _, n := range nodes {
		sig := signature(n.Call, mode)
		if _, already := by[sig]; !already {
			order = append(order, sig)
		}
		by[sig] = append(by[sig], n)
	}
	return by, order
}

// leaf turns a whole unmatched subtree into pairs on one side, so a call that
// stopped happening takes everything it used to cause with it.
func leaf(n *trace.Node, side string, mode Normalize) Pair {
	p := Pair{
		Signature: signature(n.Call, mode),
		OnlyIn:    side,
		Changes:   []Change{},
		Inferred:  n.Inferred,
	}
	if side == "a" {
		p.A = &n.Call
	} else {
		p.B = &n.Call
	}
	for _, c := range n.Children {
		p.Children = append(p.Children, leaf(c, side, mode))
	}
	return p
}

func changes(a, b trace.Call) []Change {
	out := []Change{}
	if a.Status != b.Status {
		out = append(out, Change{Field: FieldStatus, A: a.Status, B: b.Status})
	}
	if a.Failed != b.Failed {
		out = append(out, Change{Field: FieldFailed, A: a.Failed, B: b.Failed})
	}
	if a.Detail != b.Detail {
		out = append(out, Change{Field: FieldDetail, A: a.Detail, B: b.Detail})
	}
	if a.Stubbed != b.Stubbed {
		// A branch that was stubbed in one run and real in the other explains
		// away every difference below it, so it has to be said out loud.
		out = append(out, Change{Field: FieldStubbed, A: a.Stubbed, B: b.Stubbed})
	}
	return out
}

// firstDivergence walks in preorder and returns the path to the first pair that
// carries a disagreement, is missing from a run, or lost a child.
func firstDivergence(p Pair, path []string) []string {
	here := append(append([]string{}, path...), p.Signature)
	if p.OnlyIn != "" || len(p.Changes) > 0 {
		return here
	}
	// A missing child is a divergence of the parent, not of the child: the
	// reader wants the last call both runs agreed on, and then the gap.
	for _, c := range p.Children {
		if c.OnlyIn != "" {
			return append(here, c.Signature)
		}
	}
	for _, c := range p.Children {
		if found := firstDivergence(c, here); found != nil {
			return found
		}
	}
	return nil
}

func tally(p Pair, out *Result) {
	switch p.OnlyIn {
	case "a":
		out.OnlyInA++
	case "b":
		out.OnlyInB++
	default:
		out.Matched++
	}
	for _, c := range p.Children {
		tally(c, out)
	}
}

// signature is what makes two calls in different runs the same call.
//
// The operation wins where a protocol has one, because there the path
// distinguishes nothing: every GraphQL request in a codebase is a POST to the
// same endpoint, and aligning by path would pair a mutation with a query.
func signature(c trace.Call, mode Normalize) string {
	if c.Op != "" {
		return strings.Join([]string{c.Target, c.Protocol, c.Op}, " ")
	}
	return strings.Join([]string{c.Target, c.Protocol, c.Method, normalizePath(c.Path, mode)}, " ")
}

var (
	uuidLike  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexLike   = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	digitOnly = regexp.MustCompile(`^\d+$`)
	// An identifier carrying a separator: ORD-1, INV_2026, 2026-08-21.
	separated = regexp.MustCompile(`^[A-Za-z0-9]+[-_][-_A-Za-z0-9]*\d[-_A-Za-z0-9]*$`)
	anyDigit  = regexp.MustCompile(`\d`)
)

// normalizePath replaces the segments that carry a value with a placeholder, so
// that /orders/ORD-1 and /orders/ORD-2 are recognised as the same call.
//
// gRPC needs none of this: its path is /package.Service/Method and holds no
// values at all, which is why the strict rules never fire on one.
func normalizePath(path string, mode Normalize) string {
	if mode == Off || path == "" {
		return path
	}
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if s != "" && variable(s, mode) {
			segments[i] = "{}"
		}
	}
	return strings.Join(segments, "/")
}

func variable(segment string, mode Normalize) bool {
	if mode == Loose {
		return anyDigit.MatchString(segment)
	}
	return digitOnly.MatchString(segment) ||
		uuidLike.MatchString(segment) ||
		hexLike.MatchString(segment) ||
		separated.MatchString(segment)
}

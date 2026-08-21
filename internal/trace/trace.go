// Package trace groups captured calls into the requests they belong to.
//
// A flat list answers "what happened". It does not answer the question you
// actually have when fifteen services are talking and one broke: *this* click
// touched seven of them — which one failed, and which one was slow. That needs
// the calls arranged as a tree, and arranging them needs knowing which call
// caused which.
//
// There are two ways to know, and Sonda uses both because neither is enough:
//
//   - A trace id in the headers. Certain, and free to read — but only there if
//     something upstream put it there and every service passed it along.
//   - Temporal nesting: a call that begins and ends inside another one very
//     probably happened because of it. Needs nobody's cooperation, and is a
//     guess. Guesses are labelled as guesses; a tree presented as fact when it
//     was inferred is worse than no tree.
package trace

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Call is the little a tree needs to know about a capture. Deliberately not
// store.Summary: the arranging is pure, and a package that can be tested
// without a database gets tested properly.
type Call struct {
	ID       int64  `json:"id"`
	Target   string `json:"target"`
	Protocol string `json:"protocol,omitempty"`
	Method   string `json:"method,omitempty"`
	Path     string `json:"path,omitempty"`

	// Op is the operation a protocol names for itself when its path does not:
	// the GraphQL operation, since every one of those is a POST to the same
	// endpoint. Empty for the protocols whose path already says which call
	// this is.
	Op       string        `json:"op,omitempty"`
	Status   int           `json:"status"`
	Started  time.Time     `json:"started_at"`
	Duration time.Duration `json:"-"`
	TraceID  string        `json:"trace_id,omitempty"`
	Failed   bool          `json:"failed"`

	// Stubbed says the service was never called: this node is a recording
	// played back. The tree is exactly where someone asks why one branch is
	// suspiciously fast, so it has to answer there and not only in the detail.
	Stubbed bool `json:"stubbed,omitempty"`
	// Detail is the gRPC status, the transport error, whatever explains a
	// failure in one line.
	Detail string `json:"detail,omitempty"`

	// DetailKind says which of those four Detail is. Only one of them is SQL,
	// and nothing downstream can tell that by looking: a Postgres summary and an
	// ordinary error message are both a sentence with punctuation in it. Saying
	// it here is cheaper than guessing later, and guessing later is how MCP's
	// redaction cut `the user's password was rejected` at the apostrophe.
	DetailKind string `json:"detail_kind,omitempty"`

	// DurationMS is what goes over the wire. A time.Duration marshals as a
	// nanosecond integer, which every client then has to know to divide — and
	// the rest of this API already speaks milliseconds.
	DurationMS float64 `json:"duration_ms"`
}

// The values DetailKind takes. They are named rather than spelled out at each
// site because a consumer switches on them — MCP redaction treats
// DetailPostgres as SQL and everything else as prose — and a typo in a bare
// string there fails silently, in the direction of a leak.
const (
	DetailError    = "error"
	DetailGRPC     = "grpc"
	DetailGraphQL  = "graphql"
	DetailPostgres = "postgres"
)

func (c Call) end() time.Time { return c.Started.Add(c.Duration) }

// Node is one call and whatever it caused.
type Node struct {
	Call     Call    `json:"call"`
	Children []*Node `json:"children,omitempty"`

	// Inferred says the link to the parent was guessed from timing rather than
	// read from a trace id.
	Inferred bool `json:"inferred,omitempty"`

	// Ambiguous says more than one call could have been the parent and they
	// were not nested in each other, so this branch may be hanging off the
	// wrong one. Concurrent unrelated traffic is what causes it.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

// Tree is one request and everything it set off.
type Tree struct {
	TraceID string  `json:"trace_id,omitempty"`
	Root    *Node   `json:"root"`
	Calls   int     `json:"calls"`
	Failed  int     `json:"failed"`
	Spans   float64 `json:"spans_ms"`

	// Certain is true when the calls were grouped by a trace id that was really
	// in the headers. False means the whole grouping is a guess.
	Certain bool `json:"certain"`
}

// traceparent is the W3C format: version-traceid-spanid-flags. Only the trace
// id matters here.
var traceparent = regexp.MustCompile(`^[0-9a-f]{2}-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)

// headers worth reading, most standard first. gRPC metadata arrives as HTTP/2
// headers, so the same names cover both protocols.
var traceHeaders = []string{
	"X-Request-Id",
	"X-Correlation-Id",
	"X-Trace-Id",
	"X-B3-Traceid",
	"X-Amzn-Trace-Id",
}

// ID pulls a trace identifier out of request headers, or returns empty when
// there is none to find.
func ID(h http.Header) string {
	if raw := strings.TrimSpace(h.Get("Traceparent")); raw != "" {
		if m := traceparent.FindStringSubmatch(strings.ToLower(raw)); m != nil {
			return m[1]
		}
		// Malformed but present: something is trying to trace. Using it as an
		// opaque string still groups the calls correctly, which is the point.
		return raw
	}
	for _, name := range traceHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// Build arranges calls into trees, newest request first.
//
// Calls carrying the same trace id are one request, for certain. The rest are
// grouped by nesting alone, which is a guess and is marked as one.
func Build(calls []Call) []Tree {
	byTrace := map[string][]Call{}
	var untraced []Call

	for _, c := range calls {
		if c.TraceID != "" {
			byTrace[c.TraceID] = append(byTrace[c.TraceID], c)
			continue
		}
		untraced = append(untraced, c)
	}

	var out []Tree
	for id, group := range byTrace {
		for _, root := range arrange(group, false) {
			out = append(out, summarise(id, root, true))
		}
	}
	// Without a trace id the nesting has to do the grouping as well, so every
	// link in these is a guess.
	for _, root := range arrange(untraced, true) {
		out = append(out, summarise("", root, false))
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Root.Call.Started.After(out[j].Root.Call.Started)
	})
	return out
}

// arrange hangs each call off the innermost call that contains it.
func arrange(calls []Call, inferred bool) []*Node {
	if len(calls) == 0 {
		return nil
	}

	// Earliest first, and where two start together the longer one first, so a
	// container is always seen before what it contains.
	sorted := make([]Call, len(calls))
	copy(sorted, calls)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Started.Equal(sorted[j].Started) {
			return sorted[i].Duration > sorted[j].Duration
		}
		return sorted[i].Started.Before(sorted[j].Started)
	})

	nodes := make([]*Node, len(sorted))
	for i, c := range sorted {
		// Filled once, here, so every consumer reads the same number and none
		// of them has to know that a Duration marshals as nanoseconds.
		c.DurationMS = float64(c.Duration) / float64(time.Millisecond)
		nodes[i] = &Node{Call: c}
	}

	var roots []*Node
	for i, node := range nodes {
		parent, ambiguous := findParent(nodes, i)
		if parent == nil {
			roots = append(roots, node)
			continue
		}
		node.Inferred = inferred
		node.Ambiguous = ambiguous
		parent.Children = append(parent.Children, node)
	}
	return roots
}

// findParent returns the innermost earlier call whose window contains this one,
// and whether the choice was ambiguous.
func findParent(nodes []*Node, i int) (*Node, bool) {
	child := nodes[i].Call

	var candidates []*Node
	for j := 0; j < i; j++ {
		p := nodes[j].Call
		if p.ID == child.ID {
			continue
		}
		// Contained: begins no earlier and ends no later. A call that merely
		// overlaps is not a child — concurrent work looks exactly like that.
		if !p.Started.After(child.Started) && !p.end().Before(child.end()) {
			candidates = append(candidates, nodes[j])
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}

	// The innermost one is the immediate cause; the others are its ancestors.
	best := candidates[len(candidates)-1]
	for _, c := range candidates {
		if c.Call.Started.After(best.Call.Started) {
			best = c
		}
	}

	// Ambiguous when a candidate is neither the chosen parent nor one of its
	// ancestors: two unrelated calls were in flight and this could belong to
	// either.
	for _, c := range candidates {
		if c == best {
			continue
		}
		if !contains(c.Call, best.Call) {
			return best, true
		}
	}
	return best, false
}

func contains(outer, inner Call) bool {
	return !outer.Started.After(inner.Started) && !outer.end().Before(inner.end())
}

func summarise(id string, root *Node, certain bool) Tree {
	t := Tree{TraceID: id, Root: root, Certain: certain}
	var walk func(*Node)
	walk = func(n *Node) {
		t.Calls++
		if n.Call.Failed {
			t.Failed++
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	walk(root)
	t.Spans = float64(root.Call.Duration) / float64(time.Millisecond)
	return t
}

// Render draws a tree the way a person reads it, which is also the shape an
// agent can act on without parsing anything.
func Render(t Tree) string {
	var b strings.Builder
	if !t.Certain {
		b.WriteString("(grouped by timing, not by a trace id — the shape is inferred)\n")
	}
	renderNode(&b, t.Root, "", true, true)
	return b.String()
}

func renderNode(b *strings.Builder, n *Node, prefix string, last, root bool) {
	mark := "✓"
	if n.Call.Failed {
		mark = "✗"
	}

	branch := ""
	if !root {
		branch = "└─ "
		if !last {
			branch = "├─ "
		}
	}

	name := n.Call.Target
	if n.Call.Path != "" {
		name += " " + n.Call.Path
	} else if n.Call.Method != "" {
		name += " " + n.Call.Method
	}

	flags := ""
	if n.Call.Stubbed {
		flags = "  [from recording]"
	}
	if n.Ambiguous {
		flags += "  [could belong to another call]"
	}

	// The padding has to account for the indent, or the millisecond column
	// drifts right with every level and the tree stops being scannable — which
	// was the only reason to draw it instead of listing it.
	label := prefix + branch + name
	if pad := column - runeWidth(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	fmt.Fprintf(b, "%s %7.0fms %s%s\n", label, n.Call.DurationMS, mark, flags)
	if n.Call.Detail != "" {
		fmt.Fprintf(b, "%s%s    %s\n", prefix, indentFor(root, last), n.Call.Detail)
	}

	for i, child := range n.Children {
		renderNode(b, child, prefix+indentFor(root, last), i == len(n.Children)-1, false)
	}
}

// column is where the timings line up: wide enough for a few levels of nesting
// plus a service name and a path.
const column = 56

// runeWidth counts characters rather than bytes. The box-drawing characters are
// three bytes each, and padding by byte length would wreck the alignment it
// exists to keep.
func runeWidth(s string) int { return len([]rune(s)) }

func indentFor(root, last bool) string {
	switch {
	case root:
		return ""
	case last:
		return "   "
	default:
		return "│  "
	}
}

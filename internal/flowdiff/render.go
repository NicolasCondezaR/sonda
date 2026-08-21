package flowdiff

import (
	"fmt"
	"strings"
)

// column is where the verdicts line up. Wider than the trace one, because a
// signature carries the protocol and the method as well as the service.
const column = 62

// Render draws a comparison the way a person reads it, which is also the shape
// an agent can act on without parsing anything.
//
// The terminal client shows this string as it comes rather than drawing its own
// version, for the same reason it does with a trace: a second renderer is a
// second thing to keep in step with the first, for a drawing that is identical
// either way.
func Render(r Result) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%d matched, %d only in a, %d only in b\n", r.Matched, r.OnlyInA, r.OnlyInB)
	if !r.SameEntry {
		b.WriteString("(the two runs do not start from the same call — this comparison is probably meaningless)\n")
	}
	if !r.Certain {
		b.WriteString("(at least one run was grouped by timing, not by a trace id — the shapes are inferred)\n")
	}
	// The count of unpaired calls is the reader's warning that the alignment,
	// not the code under test, is what changed. Said here rather than left to
	// be counted out of the tree below.
	if unmatched := r.OnlyInA + r.OnlyInB; unmatched > r.Matched && r.Matched > 0 {
		fmt.Fprintf(&b, "(more calls went unpaired than paired — try normalize=loose, or normalize=off if these paths carry no ids)\n")
	}
	if len(r.Divergence) > 0 {
		fmt.Fprintf(&b, "first divergence: %s\n", strings.Join(r.Divergence, " → "))
	} else {
		b.WriteString("no divergence: the two runs did the same things with the same outcomes\n")
	}
	b.WriteString("\n")

	renderPair(&b, r.Root, "", true, true)
	return b.String()
}

func renderPair(b *strings.Builder, p Pair, prefix string, last, root bool) {
	branch := ""
	if !root {
		branch = "└─ "
		if !last {
			branch = "├─ "
		}
	}

	label := prefix + branch + p.Signature
	if pad := column - runeWidth(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	fmt.Fprintf(b, "%s %s\n", label, verdict(p))

	indent := prefix + indentFor(root, last)
	for _, c := range p.Changes {
		fmt.Fprintf(b, "%s    %s: %v → %v\n", indent, c.Field, c.A, c.B)
	}
	for i, c := range p.Children {
		renderPair(b, c, indent, i == len(p.Children)-1, false)
	}
}

func verdict(p Pair) string {
	switch {
	case p.OnlyIn == "a":
		return "only in a — this call is no longer made"
	case p.OnlyIn == "b":
		return "only in b — this call is new"
	case len(p.Changes) > 0:
		return "changed"
	case p.Inferred:
		return "same (shape inferred)"
	}
	return "same"
}

// runeWidth counts characters rather than bytes, because the box-drawing
// characters are three bytes each and padding by length would wreck the
// alignment it exists to keep.
func runeWidth(s string) int { return len([]rune(s)) }

func indentFor(root, last bool) string {
	switch {
	case root:
		return ""
	case last:
		return "   "
	}
	return "│  "
}

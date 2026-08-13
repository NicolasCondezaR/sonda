package mcp

import (
	"sort"
	"strings"
)

// The diagnosis is the most expensive answer Sonda gives an agent, and almost
// none of that weight is information.
//
// Measured against a real project of twenty-two services with nothing running:
// 27.5 KB, about 6.900 tokens, of which the per-service entries were 96%. Every
// one of the twenty-two carried the same verdict and the same three-sentence
// explanation, differing only in the address spliced into each sentence. The
// agent paid for twenty-two copies of one paragraph.
//
// This compacts that for MCP only. The API keeps answering in full: the web and
// the terminal render twenty-two rows at no cost to anyone, and a capability
// that reads differently per client is a capability nobody can reason about.
// What is expensive here is specifically the transport into a model's context.
//
// Nothing is dropped. The shared sentences are stated once per verdict with the
// varying values replaced by {listen}, {point_at} and {expects} — each of which
// is a field on the member beside it, so the exact original sentence can be
// reconstructed. That is the difference between compacting and summarising: a
// summary would decide for the reader which services matter.

// fields whose values get lifted out of the prose. Ordered longest-value-first
// at substitution time, because point_at usually contains listen — replacing
// the short one first would leave "DELIA_URL=http://{listen}" and a placeholder
// that no longer matches any field.
var lifted = []string{"point_at", "listen", "expects", "upstream"}

// compactDiagnosis regroups the diagnosis report by verdict.
//
// It is deliberately ignorant of the wording it compacts: it substitutes values
// it can read off the same service and then groups by whatever came out equal.
// A sentence the API rewrites tomorrow compacts just as well, and one that is
// genuinely per-service stays per-service instead of being quietly merged.
func compactDiagnosis(body map[string]any) map[string]any {
	raw, ok := body["services"].([]any)
	if !ok || len(raw) == 0 {
		return body
	}

	type group struct {
		verdict string
		prose   map[string]any
		members []any
		key     string
	}
	var groups []*group
	index := map[string]*group{}

	for _, item := range raw {
		svc, ok := item.(map[string]any)
		if !ok {
			continue
		}
		prose, member := splitDiagnosis(svc)
		key := proseKey(prose)
		g := index[key]
		if g == nil {
			verdict, _ := svc["verdict"].(string)
			g = &group{verdict: verdict, prose: prose, key: key}
			index[key] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, member)
	}

	// Largest group first: with one service broken among twenty that are merely
	// quiet, the quiet twenty are the bigger group and the broken one is the
	// answer. So order by what the report itself calls worth reading — a
	// verdict that names a problem outranks one that names a gap — and only
	// then by size.
	sort.SliceStable(groups, func(i, j int) bool {
		pi, pj := verdictRank(groups[i].verdict), verdictRank(groups[j].verdict)
		if pi != pj {
			return pi < pj
		}
		return len(groups[i].members) > len(groups[j].members)
	})

	out := make([]any, 0, len(groups))
	for _, g := range groups {
		entry := map[string]any{
			"verdict":  g.verdict,
			"services": len(g.members),
		}
		for k, v := range g.prose {
			entry[k] = v
		}
		// The prose was the bulk of it, but not the end: twenty-two members
		// each carrying connections:0, captures:0 and listening:true is the
		// same repetition one level down. A reading shared by every member of
		// the group is a fact about the group.
		if shared := liftShared(g.members); len(shared) > 0 {
			entry["same_for_all"] = shared
		}
		entry["members"] = g.members
		out = append(out, entry)
	}

	compacted := make(map[string]any, len(body)+1)
	for k, v := range body {
		if k == "services" {
			continue
		}
		compacted[k] = v
	}
	compacted["groups"] = out
	compacted["shape"] = "Services that read the same are grouped: the shared reading is stated once, " +
		"and {listen}, {point_at}, {expects} and {upstream} inside it stand for the field of that name on each member below. " +
		"same_for_all holds the fields every member of the group agreed on, so they are not repeated per member. " +
		"Groups are ordered with the ones naming a problem first. The full per-service report is at GET /api/diagnose."
	return compacted
}

// liftShared removes from every member the fields that all of them agree on,
// and returns those fields once. It edits the members in place, which is safe:
// they were built fresh by splitDiagnosis.
//
// A field is only lifted when every member carries it and the values compare
// equal. Comparison is on the JSON scalars the API emits — string, bool and
// float64 — and anything else is left alone rather than guessed at, because
// lifting two values that merely look alike would state as shared something
// that is not.
func liftShared(members []any) map[string]any {
	if len(members) < 2 {
		// With one member there is nothing to share, and hoisting its fields
		// into a group of one would make the answer longer, not shorter.
		return nil
	}
	first, ok := members[0].(map[string]any)
	if !ok {
		return nil
	}

	shared := map[string]any{}
	for key, value := range first {
		if !comparableScalar(value) {
			continue
		}
		// No guard for service and listen: both are unique within a project —
		// Sonda refuses two services on one port and the name is the identity —
		// so they can never compare equal across members and never be lifted. A
		// guard here read as protection and was unreachable; a mutation removing
		// it broke nothing, which is how it was found.
		same := true
		for _, item := range members[1:] {
			m, ok := item.(map[string]any)
			if !ok || m[key] != value {
				same = false
				break
			}
		}
		if same {
			shared[key] = value
		}
	}

	for key := range shared {
		for _, item := range members {
			if m, ok := item.(map[string]any); ok {
				delete(m, key)
			}
		}
	}
	return shared
}

// comparableScalar reports whether a value can be compared with == without
// panicking. Maps and slices are not, and are never lifted.
func comparableScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, nil:
		return true
	default:
		return false
	}
}

// splitDiagnosis separates a service's reading into the prose that may be
// shared and the facts that are its own.
func splitDiagnosis(svc map[string]any) (prose, member map[string]any) {
	prose = map[string]any{}
	member = map[string]any{}

	// Substitute longest value first: see the note on lifted.
	type pair struct{ name, value string }
	var subs []pair
	for _, name := range lifted {
		if v, ok := svc[name].(string); ok && v != "" {
			subs = append(subs, pair{name, v})
		}
	}
	sort.SliceStable(subs, func(i, j int) bool { return len(subs[i].value) > len(subs[j].value) })

	template := func(s string) string {
		for _, p := range subs {
			s = strings.ReplaceAll(s, p.value, "{"+p.name+"}")
		}
		return s
	}

	for k, v := range svc {
		switch k {
		case "detail":
			if s, ok := v.(string); ok {
				prose[k] = template(s)
				continue
			}
			member[k] = v
		case "cannot_distinguish", "what_to_check":
			if list, ok := v.([]any); ok {
				out := make([]any, 0, len(list))
				for _, line := range list {
					if s, ok := line.(string); ok {
						out = append(out, template(s))
						continue
					}
					out = append(out, line)
				}
				prose[k] = out
				continue
			}
			member[k] = v
		case "verdict":
			// Carried by the group, not repeated on every member.
		default:
			member[k] = v
		}
	}
	return prose, member
}

// proseKey is the grouping key: the verdict plus the templated sentences. Two
// services group only when what they say is the same after their own values are
// lifted out, so a reading that genuinely differs is never merged away.
func proseKey(prose map[string]any) string {
	keys := make([]string, 0, len(prose))
	for k := range prose {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		switch t := prose[k].(type) {
		case string:
			b.WriteString(t)
		case []any:
			for _, v := range t {
				if s, ok := v.(string); ok {
					b.WriteString(s)
					b.WriteByte('\x00')
				}
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// verdictRank orders the groups so a reading that names a problem is read before
// one that names an absence. The strings are the API's own verdict values;
// anything unknown sorts between the two, because a verdict this does not
// recognise is more likely to be a new problem than a new kind of quiet.
func verdictRank(verdict string) int {
	switch verdict {
	case "listener_down":
		return 0
	case "connected_not_captured":
		return 1
	case "upstream_unreachable":
		return 2
	case "capturing":
		return 4
	case "no_connections":
		return 5
	default:
		return 3
	}
}

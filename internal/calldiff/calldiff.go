// Package calldiff compares two decoded payloads by structure rather than by
// text.
//
// A textual diff of two JSON bodies reports every reordered key and every
// reindented block as a change, which buries the one field that actually
// differs. Comparing the parsed structures instead means the answer to "why did
// this one fail and that one work" is a short list of paths, not a wall of
// red and green.
package calldiff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type Kind string

const (
	Added   Kind = "added"
	Removed Kind = "removed"
	Changed Kind = "changed"
)

// Change is one difference, addressed by a path like `lines[1].price.currency`.
type Change struct {
	Path string `json:"path"`
	Kind Kind   `json:"kind"`
	A    any    `json:"a,omitempty"`
	B    any    `json:"b,omitempty"`
}

// Structural walks both values together and reports where they diverge.
// Ordering of object keys is irrelevant; ordering of array elements is not,
// because in a protobuf repeated field the position is part of the meaning.
func Structural(a, b any) []Change {
	// Empty rather than nil: "no differences" must serialize as [] so a client
	// can count it without first checking for null.
	changes := []Change{}
	walk("", a, b, &changes)
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// StructuralJSON compares two JSON documents. It reports an error only when a
// side fails to parse, so the caller can fall back to comparing bytes.
func StructuralJSON(a, b []byte) ([]Change, error) {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return nil, fmt.Errorf("left side is not JSON: %w", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return nil, fmt.Errorf("right side is not JSON: %w", err)
	}
	return Structural(av, bv), nil
}

func walk(path string, a, b any, out *[]Change) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, Change{Path: pathOr(path), Kind: Changed, A: a, B: b})
			return
		}
		for _, key := range union(av, bv) {
			aVal, inA := av[key]
			bVal, inB := bv[key]
			child := join(path, key)
			switch {
			case inA && !inB:
				*out = append(*out, Change{Path: child, Kind: Removed, A: aVal})
			case !inA && inB:
				*out = append(*out, Change{Path: child, Kind: Added, B: bVal})
			default:
				walk(child, aVal, bVal, out)
			}
		}

	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, Change{Path: pathOr(path), Kind: Changed, A: a, B: b})
			return
		}
		for i := range max(len(av), len(bv)) {
			child := path + "[" + strconv.Itoa(i) + "]"
			switch {
			case i >= len(bv):
				*out = append(*out, Change{Path: child, Kind: Removed, A: av[i]})
			case i >= len(av):
				*out = append(*out, Change{Path: child, Kind: Added, B: bv[i]})
			default:
				walk(child, av[i], bv[i], out)
			}
		}

	default:
		if !equalScalar(a, b) {
			*out = append(*out, Change{Path: pathOr(path), Kind: Changed, A: a, B: b})
		}
	}
}

// equalScalar compares leaves after normalizing how the value was written.
//
// protojson renders int64 as a string, so the same field can arrive as
// "1290000" from one capture and as 1290000 from another. Reporting that as a
// difference would be a statement about encoding, not about the value, and the
// real change would be buried under it. Normalizing first is the deliberate
// tradeoff: a difference that survives it is a difference in the data.
func equalScalar(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a == b {
		return true
	}
	return scalarText(a) == scalarText(b)
}

func scalarText(v any) string {
	// %v on a float64 switches to exponent notation past six digits, which
	// would make 1290000 and "1290000" look different.
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func union(a, b map[string]any) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	keys := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// pathOr names the document itself when the difference is at the root.
func pathOr(path string) string {
	if strings.TrimSpace(path) == "" {
		return "(root)"
	}
	return path
}

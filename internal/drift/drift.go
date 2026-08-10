// Package drift compares the shape of a response against what it used to be.
//
// In a monorepo where nobody versions a contract, a field that quietly went
// away or changed type breaks the caller days later, far from the change that
// caused it. Sonda already holds every response a service ever gave, so the
// comparison is there for the taking — and it is a comparison of shapes, not of
// values. Two calls returning different prices are not drift. Two calls where
// one returns a price as a number and the other as a string are.
//
// This is the one thing in Sonda that never touches the proxy. It only reads
// what was already stored.
package drift

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Kind says what happened to a field.
const (
	Gone    = "gone"    // it was there and is not any more
	Added   = "added"   // it was not there and now is
	Retyped = "retyped" // same field, different type
)

// Change is one difference between two shapes.
type Change struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Was  string `json:"was,omitempty"`
	Now  string `json:"now,omitempty"`
}

// Line reads the way a person would say it.
func (c Change) Line() string {
	switch c.Kind {
	case Gone:
		return fmt.Sprintf("- %s   (was %s)", c.Path, c.Was)
	case Added:
		return fmt.Sprintf("+ %s   (%s)", c.Path, c.Now)
	default:
		return fmt.Sprintf("~ %s   %s -> %s", c.Path, c.Was, c.Now)
	}
}

// Shape is every field a payload carries, and the type it carried there.
type Shape map[string]string

// Of reads the shape of a JSON document.
//
// Arrays collapse to one entry per field rather than one per element: a list of
// two hundred orders has the shape of one order, and reporting it two hundred
// times would bury the one field that changed.
func Of(payload []byte) (Shape, error) {
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, fmt.Errorf("not JSON, so it has no shape to compare: %w", err)
	}
	shape := Shape{}
	walk("", v, shape)
	return shape, nil
}

func walk(path string, v any, into Shape) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			// A field whose value became {} is worth reporting; a whole
			// document that is {} is just a document with no fields, and
			// recording it as one would make losing the last field read as two
			// changes instead of one.
			if path != "" {
				into[path] = "empty object"
			}
			return
		}
		for key, value := range t {
			walk(join(path, key), value, into)
		}
	case []any:
		if len(t) == 0 {
			// An empty list says nothing about what it holds, and guessing
			// would invent a contract nobody wrote.
			into[at(path)+"[]"] = "empty list"
			return
		}
		for _, item := range t {
			walk(path+"[]", item, into)
		}
	default:
		into[at(path)] = typeOf(v)
	}
}

func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	default:
		return "unknown"
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func at(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

// Compare reports what changed between two shapes, in path order.
//
// A field that is null on one side and typed on the other is not reported. A
// nullable field is ordinary, and flagging every one of them would bury the
// changes that matter under noise nobody can act on — which is how a drift
// report stops being read.
func Compare(before, after Shape) []Change {
	seen := map[string]bool{}
	var out []Change

	for path, was := range before {
		seen[path] = true
		now, still := after[path]
		switch {
		case !still:
			out = append(out, Change{Path: path, Kind: Gone, Was: was})
		case was != now && was != "null" && now != "null":
			out = append(out, Change{Path: path, Kind: Retyped, Was: was, Now: now})
		}
	}
	for path, now := range after {
		if !seen[path] {
			out = append(out, Change{Path: path, Kind: Added, Now: now})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Render draws a report the way a person reads it, and the way an agent can act
// on without parsing anything.
func Render(changes []Change) string {
	if len(changes) == 0 {
		return "The shape has not changed.\n"
	}
	var b strings.Builder
	for _, c := range changes {
		b.WriteString(c.Line() + "\n")
	}
	return b.String()
}

// Breaking reports whether anything here would break a caller.
//
// A field appearing is additive and safe. A field going away or changing type
// is what takes a client down, and the distinction is the difference between a
// report worth acting on and a list worth ignoring.
func Breaking(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Kind != Added {
			out = append(out, c)
		}
	}
	return out
}

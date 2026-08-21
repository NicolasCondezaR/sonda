package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/NicolasCondezaR/sonda/internal/flowdiff"
)

// bodyDiff is one aligned pair whose payloads were compared. It is returned
// beside the aligned tree rather than inside it, because reading a body means
// going back to the database for both calls and the tree has to be cheap to
// build even when nobody asks for payloads.
type bodyDiff struct {
	Signature string   `json:"signature"`
	A         int64    `json:"a"`
	B         int64    `json:"b"`
	Request   sideDiff `json:"request"`
	Response  sideDiff `json:"response"`
}

type flowDiffResult struct {
	flowdiff.Result
	Bodies []bodyDiff `json:"bodies"`

	// Rendered is the same comparison drawn as text. The terminal client shows
	// it as it comes and an agent can read it without walking the tree, which
	// is the same bargain the trace endpoint already makes.
	Rendered string `json:"rendered"`

	// Unmatched is the count the caller has to look at before believing any of
	// the rest: a comparison where most of the calls found no partner did not
	// find real differences, it found a normalisation that does not fit these
	// paths.
	Unmatched int `json:"unmatched"`
}

// flowDiffCalls compares two runs of the same flow, seeded by one call from
// each. Comparing two calls tells you why one request failed; comparing two
// runs tells you which of the fifteen calls behind a click is the one that
// changed, which is the question people actually arrive with.
func (s *Server) flowDiffCalls(w http.ResponseWriter, r *http.Request) {
	aID, err := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "a must be a call id from the first run")
		return
	}
	bID, err := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "b must be a call id from the second run")
		return
	}

	mode, ok := normalizeMode(r.URL.Query().Get("normalize"))
	if !ok {
		writeError(w, http.StatusBadRequest, "normalize must be strict, loose or off")
		return
	}
	bodies := r.URL.Query().Get("bodies")
	if bodies == "" {
		bodies = "first"
	}
	if bodies != "first" && bodies != "all" && bodies != "none" {
		writeError(w, http.StatusBadRequest, "bodies must be first, all or none")
		return
	}

	aTree, _, err := s.treeForCall(r.Context(), aID)
	if err != nil {
		writeTreeError(w, err, aID)
		return
	}
	bTree, _, err := s.treeForCall(r.Context(), bID)
	if err != nil {
		writeTreeError(w, err, bID)
		return
	}

	result := flowdiff.Compare(aTree, bTree, mode)
	out := flowDiffResult{
		Result:    result,
		Bodies:    []bodyDiff{},
		Rendered:  flowdiff.Render(result),
		Unmatched: result.OnlyInA + result.OnlyInB,
	}
	out.Bodies = append(out.Bodies, s.flowBodies(r.Context(), result, bodies)...)
	writeJSON(w, http.StatusOK, out)
}

func writeTreeError(w http.ResponseWriter, err error, id int64) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "no call with id "+strconv.FormatInt(id, 10))
	case errors.Is(err, errNoTree):
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		slog.Error("flowdiff: build tree", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the surrounding calls")
	}
}

func normalizeMode(raw string) (flowdiff.Normalize, bool) {
	switch flowdiff.Normalize(raw) {
	case "":
		return flowdiff.Strict, true
	case flowdiff.Strict, flowdiff.Loose, flowdiff.Off:
		return flowdiff.Normalize(raw), true
	}
	return "", false
}

// flowBodies compares the payloads of the pairs worth comparing.
//
// The default is the divergence and its direct children rather than everything,
// because a flow of forty calls means eighty payload reads and a wall of JSON
// that buries the one field that changed — and when the reader is a coding
// agent, it buries it inside a context window somebody is paying for.
func (s *Server) flowBodies(ctx context.Context, result flowdiff.Result, mode string) []bodyDiff {
	if mode == "none" {
		return nil
	}

	var out []bodyDiff
	add := func(p flowdiff.Pair) {
		if p.A == nil || p.B == nil {
			return
		}
		a, err := s.store.Get(ctx, p.A.ID)
		if err != nil {
			return
		}
		b, err := s.store.Get(ctx, p.B.ID)
		if err != nil {
			return
		}
		out = append(out, bodyDiff{
			Signature: p.Signature,
			A:         p.A.ID,
			B:         p.B.ID,
			Request:   s.sideChanges(ctx, a, b, true),
			Response:  s.sideChanges(ctx, a, b, false),
		})
	}

	if mode == "all" {
		walkPairs(result.Root, add)
		return out
	}

	if diverged, found := pairAt(result.Root, result.Divergence); found {
		add(diverged)
		for _, c := range diverged.Children {
			add(c)
		}
	}
	return out
}

func walkPairs(p flowdiff.Pair, fn func(flowdiff.Pair)) {
	fn(p)
	for _, c := range p.Children {
		walkPairs(c, fn)
	}
}

// pairAt follows a divergence path back down the aligned tree. The path is a
// list of signatures, and siblings can share one, so the first match at each
// level is taken — the same order the path was written in.
func pairAt(root flowdiff.Pair, path []string) (flowdiff.Pair, bool) {
	if len(path) == 0 || path[0] != root.Signature {
		return flowdiff.Pair{}, false
	}
	current := root
	for _, want := range path[1:] {
		next, found := flowdiff.Pair{}, false
		for _, c := range current.Children {
			if c.Signature == want {
				next, found = c, true
				break
			}
		}
		if !found {
			return flowdiff.Pair{}, false
		}
		current = next
	}
	return current, true
}

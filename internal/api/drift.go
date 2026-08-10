package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/NicolasCondezaR/sonda/internal/drift"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// driftForEndpoint answers the question no log answers: has the shape of this
// response changed since it was last working.
//
// The baseline is the oldest capture of the same endpoint rather than a stored
// schema. Sonda already holds the history, and a baseline nobody has to
// maintain is a baseline that is still there in three weeks.
func (s *Server) driftForEndpoint(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Two explicit calls, when the reader already knows which two.
	if q.Get("a") != "" || q.Get("b") != "" {
		a, errA := strconv.ParseInt(q.Get("a"), 10, 64)
		b, errB := strconv.ParseInt(q.Get("b"), 10, 64)
		if errA != nil || errB != nil {
			writeError(w, http.StatusBadRequest, "a and b must both be call ids")
			return
		}
		s.compareTwo(w, r, a, b)
		return
	}

	target := q.Get("target")
	if target == "" {
		writeError(w, http.StatusBadRequest, "pass a target, or a and b as call ids")
		return
	}

	calls, err := s.store.List(r.Context(), store.Filter{
		Target:  target,
		Path:    q.Get("path"),
		Method:  q.Get("method"),
		Project: s.projectFilter(),
		Limit:   maxWindowCalls,
	})
	if err != nil {
		slog.Error("drift: list", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the history")
		return
	}
	if len(calls) < 2 {
		writeError(w, http.StatusNotFound,
			"there are fewer than two captures of that endpoint, so there is nothing to compare it against")
		return
	}

	// Newest first out of the store, so the last entry is the baseline.
	s.compareTwo(w, r, calls[len(calls)-1].ID, calls[0].ID)
}

func (s *Server) compareTwo(w http.ResponseWriter, r *http.Request, oldest, newest int64) {
	before, ok := s.shapeOf(w, r, oldest)
	if !ok {
		return
	}
	after, ok := s.shapeOf(w, r, newest)
	if !ok {
		return
	}

	changes := drift.Compare(before, after)
	breaking := drift.Breaking(changes)
	if changes == nil {
		changes = []drift.Change{}
	}
	if breaking == nil {
		breaking = []drift.Change{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"baseline": oldest,
		"latest":   newest,
		"changes":  changes,
		"breaking": breaking,
		"rendered": drift.Render(changes),
		"note":     "A field appearing is additive and safe. A field going away or changing type is what takes a caller down.",
	})
}

// shapeOf reads one response as a shape, answering for itself when it cannot.
func (s *Server) shapeOf(w http.ResponseWriter, r *http.Request, id int64) (drift.Shape, bool) {
	call, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no call with id "+strconv.FormatInt(id, 10))
		return nil, false
	}
	shape, err := drift.Of(call.Response.Body)
	if err != nil {
		// Not a failure of the tool: this endpoint does not answer JSON, and
		// saying so beats an empty comparison that looks like agreement.
		writeError(w, http.StatusUnprocessableEntity,
			"call "+strconv.FormatInt(id, 10)+" did not answer JSON, so it has no shape to compare")
		return nil, false
	}
	return shape, true
}

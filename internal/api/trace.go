package api

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/trace"
	"google.golang.org/grpc/codes"
)

// window is how far either side of a call to look when there is no trace id to
// group by. Wide enough for a slow request that fans out, narrow enough that
// unrelated traffic from a minute ago does not wander into the tree.
const window = 60 * time.Second

// maxWindowCalls bounds the arranging, which compares calls against each other.
// A debugging window holds hundreds; a busy hour holds enough to matter.
const maxWindowCalls = 2000

// traceForCall answers the question a flat list cannot: this call was part of
// something larger — what else did that touch, and where did it break.
func (s *Server) traceForCall(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("call"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "call must be a call id")
		return
	}

	target, err := s.store.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no call with that id")
		return
	}
	if err != nil {
		slog.Error("trace: read call", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the call")
		return
	}

	filter := store.Filter{
		Project: s.projectFilter(),
		Limit:   maxWindowCalls,
		Since:   target.StartedAt.Add(-window),
		Until:   target.StartedAt.Add(window),
	}
	calls, err := s.store.List(r.Context(), filter)
	if err != nil {
		slog.Error("trace: list window", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the surrounding calls")
		return
	}

	// A trace id narrows the candidates to certainty. Without one, everything
	// in the window is a candidate and the nesting has to do the work.
	subject := make([]trace.Call, 0, len(calls))
	for _, c := range calls {
		if target.TraceID != "" && c.TraceID != target.TraceID {
			continue
		}
		subject = append(subject, toTraceCall(c))
	}

	for _, tree := range trace.Build(subject) {
		if containsCall(tree.Root, id) {
			writeJSON(w, http.StatusOK, map[string]any{
				"trace":     tree,
				"rendered":  trace.Render(tree),
				"of_window": len(calls),
			})
			return
		}
	}

	// The call is always in some tree — on its own if nothing else relates to
	// it — so reaching here means the window logic changed underneath us.
	writeError(w, http.StatusInternalServerError, "the call did not land in any tree")
}

func toTraceCall(c store.Summary) trace.Call {
	out := trace.Call{
		ID: c.ID, Target: c.Target, Method: c.Method, Path: c.Path,
		Status: c.Status, Started: c.StartedAt, Duration: c.Duration,
		TraceID: c.TraceID,
		Failed:  summaryFailed(c),
		Stubbed: c.StubOf != nil,
	}

	// What to say about a failure, most specific first. For gRPC and GraphQL
	// alike the HTTP status is 200 even when the call failed, so the status is
	// the one thing that does not carry the outcome.
	switch {
	case c.Error != "":
		out.Detail = c.Error
	case c.GRPCStatus != nil && *c.GRPCStatus != 0:
		out.Detail = codes.Code(*c.GRPCStatus).String()
		if c.GRPCMessage != "" {
			out.Detail += ": " + c.GRPCMessage
		}
	case c.GraphQLErrors > 0:
		out.Detail = fmt.Sprintf("%s: %d GraphQL error(s)", c.GraphQLOp, c.GraphQLErrors)
	}
	return out
}

// summaryFailed is the definition the rest of Sonda uses, applied to a
// summary. Keeping it identical is what stops the tree from calling something
// healthy that the field already flagged red.
//
// It is mirrored by store.faultPredicate in SQL, by isFault in the web client
// and by Call.Fault in the terminal one. Four copies is three too many, but
// they are four different languages reading three different shapes of the same
// record, and the only thing that keeps them honest is that each one is short
// enough to compare by eye.
func summaryFailed(c store.Summary) bool {
	if c.Error != "" {
		return true
	}
	// A GraphQL error arrives under HTTP 200 with no transport complaint. It is
	// checked before the status for exactly that reason.
	if c.GraphQLErrors > 0 {
		return true
	}
	if c.GRPCStatus != nil {
		return *c.GRPCStatus != 0
	}
	return c.Status >= 400
}

func containsCall(n *trace.Node, id int64) bool {
	if n.Call.ID == id {
		return true
	}
	for _, child := range n.Children {
		if containsCall(child, id) {
			return true
		}
	}
	return false
}

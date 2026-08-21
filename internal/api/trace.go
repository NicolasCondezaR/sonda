package api

import (
	"context"
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

// errNoTree means the call exists but did not land in any tree, which can only
// happen if the window logic changed underneath us: a call is always in some
// tree, on its own if nothing else relates to it.
var errNoTree = errors.New("the call did not land in any tree")

// treeForCall groups a call with everything that happened around it. Both the
// trace endpoint and the flow comparison need exactly this, and two copies of a
// window this fiddly would drift apart within a release.
func (s *Server) treeForCall(ctx context.Context, id int64) (trace.Tree, int, error) {
	target, err := s.store.Get(ctx, id)
	if err != nil {
		return trace.Tree{}, 0, err
	}

	filter := store.Filter{
		Project: s.projectFilter(),
		Limit:   maxWindowCalls,
		Since:   target.StartedAt.Add(-window),
		Until:   target.StartedAt.Add(window),
	}
	calls, err := s.store.List(ctx, filter)
	if err != nil {
		return trace.Tree{}, 0, err
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
			return tree, len(calls), nil
		}
	}
	return trace.Tree{}, len(calls), errNoTree
}

// traceForCall answers the question a flat list cannot: this call was part of
// something larger — what else did that touch, and where did it break.
func (s *Server) traceForCall(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("call"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "call must be a call id")
		return
	}

	tree, ofWindow, err := s.treeForCall(r.Context(), id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "no call with that id")
		return
	case errors.Is(err, errNoTree):
		slog.Error("trace: call landed in no tree", "id", id)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	case err != nil:
		slog.Error("trace: build tree", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the surrounding calls")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"trace":     tree,
		"rendered":  trace.Render(tree),
		"of_window": ofWindow,
	})
}

func toTraceCall(c store.Summary) trace.Call {
	out := trace.Call{
		ID: c.ID, Target: c.Target, Protocol: c.Protocol,
		Method: c.Method, Path: c.Path, Op: c.GraphQLOp,
		Status: c.Status, Started: c.StartedAt, Duration: c.Duration,
		TraceID: c.TraceID,
		Failed:  summaryFailed(c),
		Stubbed: c.StubOf != nil,
	}

	// What to say about a failure, most specific first. For gRPC and GraphQL
	// alike the HTTP status is 200 even when the call failed, so the status is
	// the one thing that does not carry the outcome.
	//
	// Each branch also states which of the four it produced. Three are prose and
	// one is a one-line SQL summary, they are indistinguishable by inspection,
	// and a consumer that has to tell them apart — MCP redaction does — can only
	// guess. This is the field that stops it guessing.
	switch {
	case c.Error != "":
		out.Detail, out.DetailKind = c.Error, trace.DetailError
	case c.GRPCStatus != nil && *c.GRPCStatus != 0:
		out.Detail = codes.Code(*c.GRPCStatus).String()
		if c.GRPCMessage != "" {
			out.Detail += ": " + c.GRPCMessage
		}
		out.DetailKind = trace.DetailGRPC
	case c.GraphQLErrors > 0:
		out.Detail = fmt.Sprintf("%s: %d GraphQL error(s)", c.GraphQLOp, c.GraphQLErrors)
		out.DetailKind = trace.DetailGraphQL
	case c.PostgresErrors > 0:
		out.Detail, out.DetailKind = c.PostgresSummary, trace.DetailPostgres
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
	// checked before the status for exactly that reason, and a Postgres session
	// has no HTTP status at all.
	if c.GraphQLErrors > 0 || c.PostgresErrors > 0 {
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

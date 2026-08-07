package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"sonda/internal/calldiff"
	"sonda/internal/config"
	"sonda/internal/store"
)

type diffResult struct {
	A diffSide `json:"a"`
	B diffSide `json:"b"`

	// Metadata differences are listed separately from payload ones because they
	// answer different questions: one is "did the outcome change", the other is
	// "did the data change".
	Metadata []calldiff.Change `json:"metadata"`
	Request  sideDiff          `json:"request"`
	Response sideDiff          `json:"response"`
}

type diffSide struct {
	ID       int64  `json:"id"`
	Target   string `json:"target"`
	Protocol string `json:"protocol"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Started  string `json:"started_at"`
}

type sideDiff struct {
	// Comparable is false when at least one side could not be decoded into
	// something with structure. Saying so beats a textual diff dressed up as a
	// structural one.
	Comparable bool              `json:"comparable"`
	Reason     string            `json:"reason,omitempty"`
	Identical  bool              `json:"identical"`
	Changes    []calldiff.Change `json:"changes"`
	Messages   []messageDiff     `json:"messages,omitempty"`
}

// messageDiff is the per-message view a streaming gRPC call needs: message 3 of
// 5 can differ while the rest match.
type messageDiff struct {
	Index      int               `json:"index"`
	OnlyIn     string            `json:"only_in,omitempty"`
	Comparable bool              `json:"comparable"`
	Reason     string            `json:"reason,omitempty"`
	Identical  bool              `json:"identical"`
	Changes    []calldiff.Change `json:"changes"`
}

func (s *Server) diffCalls(w http.ResponseWriter, r *http.Request) {
	aID, err := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "a must be a call id")
		return
	}
	bID, err := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "b must be a call id")
		return
	}

	a, err := s.store.Get(r.Context(), aID)
	if err != nil {
		writeCallLookupError(w, err, aID)
		return
	}
	b, err := s.store.Get(r.Context(), bID)
	if err != nil {
		writeCallLookupError(w, err, bID)
		return
	}

	writeJSON(w, http.StatusOK, diffResult{
		A:        toDiffSide(a),
		B:        toDiffSide(b),
		Metadata: metadataChanges(a, b),
		Request:  s.sideChanges(r.Context(), a, b, true),
		Response: s.sideChanges(r.Context(), a, b, false),
	})
}

func writeCallLookupError(w http.ResponseWriter, err error, id int64) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no call with id %d", id))
		return
	}
	writeError(w, http.StatusInternalServerError, "could not read call")
}

func toDiffSide(c *store.Call) diffSide {
	return diffSide{
		ID: c.ID, Target: c.Target, Protocol: c.Protocol,
		Method: c.Method, Path: c.Path, Started: c.StartedAt.Format(timeLayout),
	}
}

// metadataChanges compares the outcome, not the payload. Duration is left out
// on purpose: it differs on every replay and would be noise in a list whose job
// is to show what actually changed.
func metadataChanges(a, b *store.Call) []calldiff.Change {
	left := map[string]any{
		"path":        a.Path,
		"method":      a.Method,
		"http_status": a.Status,
		"error":       a.Error,
	}
	right := map[string]any{
		"path":        b.Path,
		"method":      b.Method,
		"http_status": b.Status,
		"error":       b.Error,
	}
	if a.GRPCStatus != nil || b.GRPCStatus != nil {
		left["grpc_status"] = grpcStatusText(a)
		right["grpc_status"] = grpcStatusText(b)
		left["grpc_message"] = a.GRPCMessage
		right["grpc_message"] = b.GRPCMessage
	}
	return calldiff.Structural(left, right)
}

func grpcStatusText(c *store.Call) any {
	if c.GRPCStatus == nil {
		return nil
	}
	return *c.GRPCStatus
}

func (s *Server) sideChanges(ctx context.Context, a, b *store.Call, request bool) sideDiff {
	aMsg, bMsg := a.Response, b.Response
	if request {
		aMsg, bMsg = a.Request, b.Request
	}

	if aMsg.Truncated || bMsg.Truncated {
		return sideDiff{
			Changes: []calldiff.Change{},
			Reason:  "one of the bodies was truncated when captured, so a comparison would be about what was stored rather than what was sent",
		}
	}

	if a.Protocol == config.ProtocolGRPC && b.Protocol == config.ProtocolGRPC {
		return s.grpcSideChanges(ctx, a, b, request)
	}

	changes, err := calldiff.StructuralJSON(aMsg.Body, bMsg.Body)
	if err != nil {
		// Not JSON on at least one side. Rather than pretend, say whether the
		// bytes are the same and leave it there.
		return sideDiff{
			Changes:   []calldiff.Change{},
			Reason:    "not JSON on both sides, so only the bytes were compared",
			Identical: string(aMsg.Body) == string(bMsg.Body),
		}
	}
	return sideDiff{Comparable: true, Identical: len(changes) == 0, Changes: changes}
}

func (s *Server) grpcSideChanges(ctx context.Context, a, b *store.Call, request bool) sideDiff {
	aView := s.buildGRPCView(ctx, a)
	bView := s.buildGRPCView(ctx, b)
	if aView == nil || bView == nil {
		return sideDiff{Changes: []calldiff.Change{}, Reason: "one side could not be read as a gRPC call"}
	}

	aMsgs, bMsgs := aView.Response, bView.Response
	if request {
		aMsgs, bMsgs = aView.Request, bView.Request
	}

	out := sideDiff{Comparable: true, Identical: true, Changes: []calldiff.Change{}}
	for i := range max(len(aMsgs), len(bMsgs)) {
		switch {
		case i >= len(bMsgs):
			out.Messages = append(out.Messages, messageDiff{Index: i, OnlyIn: "a", Changes: []calldiff.Change{}})
			out.Identical = false
		case i >= len(aMsgs):
			out.Messages = append(out.Messages, messageDiff{Index: i, OnlyIn: "b", Changes: []calldiff.Change{}})
			out.Identical = false
		default:
			m := compareMessages(i, aMsgs[i], bMsgs[i])
			if !m.Identical {
				out.Identical = false
			}
			out.Messages = append(out.Messages, m)
		}
	}
	return out
}

func compareMessages(index int, a, b messageView) messageDiff {
	// Without a schema there is no decoded document to compare structurally.
	// The wire-format view is a list of guesses, and diffing guesses would
	// produce a confident answer built on inference.
	if a.JSON == nil || b.JSON == nil {
		return messageDiff{
			Index:   index,
			Changes: []calldiff.Change{},
			Reason:  "no schema for this method, so the messages were not compared field by field",
		}
	}
	changes, err := calldiff.StructuralJSON(a.JSON, b.JSON)
	if err != nil {
		return messageDiff{Index: index, Changes: []calldiff.Change{}, Reason: err.Error()}
	}
	return messageDiff{
		Index: index, Comparable: true,
		Identical: len(changes) == 0, Changes: changes,
	}
}

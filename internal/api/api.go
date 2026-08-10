// Package api exposes the captured traffic over HTTP.
//
// This is the seam the SDD depends on: the web UI and the later TUI are two
// clients of this API, so neither one owns the query logic.
package api

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/fault"
	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/stub"
)

type Dropper interface {
	Dropped() int64
}

type Server struct {
	store   *store.Store
	dropped Dropper
	hub     *Hub
	rt      *runtime.Runtime

	// stubs is nil when nothing wired one in, which is how every test that
	// does not care about stubbing keeps working.
	stubs *stub.Registry

	// faults is nil the same way, and a nil one reports no rules.
	faults *fault.Registry
}

// WithStubs gives the server control of which services answer from recordings.
func (s *Server) WithStubs(r *stub.Registry) *Server { s.stubs = r; return s }

// WithFaults gives the server control of which services are broken on purpose.
func (s *Server) WithFaults(r *fault.Registry) *Server { s.faults = r; return s }

func New(s *store.Store, dropped Dropper, rt *runtime.Runtime) *Server {
	return &Server{store: s, dropped: dropped, rt: rt, hub: NewHub()}
}

// targets are the services of the project currently listening. Read from the
// runtime on every call rather than held here: a second copy of what is
// configured is a second thing that can be wrong.
func (s *Server) targets() []config.Target {
	active := s.rt.Active()
	if active == nil {
		return nil
	}
	out := make([]config.Target, 0, len(active.Services))
	for _, svc := range active.Services {
		out = append(out, config.Target{
			Name: svc.Name, Listen: svc.Listen,
			Upstream: svc.Upstream, Protocol: svc.Protocol,
		})
	}
	return out
}

func (s *Server) resolvers() Resolvers { return s.rt.Resolvers() }

// projectFilter scopes queries to the project whose ports are open, so a
// listing never mixes one system's traffic into another's.
func (s *Server) projectFilter() string { return s.rt.ActiveName() }

// Hub is the live-view fan-out; wire it to the recorder with OnStored.
func (s *Server) Hub() *Hub { return s.hub }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/calls", s.listCalls)
	mux.HandleFunc("GET /api/calls/{id}", s.getCall)
	mux.HandleFunc("GET /api/targets", s.listTargets)
	mux.HandleFunc("GET /api/schemas", s.listSchemas)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/stream", s.stream)
	mux.HandleFunc("POST /api/calls/{id}/replay", s.replayCall)
	mux.HandleFunc("GET /api/diff", s.diffCalls)
	mux.HandleFunc("GET /api/trace", s.traceForCall)
	mux.HandleFunc("GET /api/stub", s.stubState)
	mux.HandleFunc("POST /api/stub", s.setStub)
	mux.HandleFunc("GET /api/faults", s.faultState)
	mux.HandleFunc("POST /api/faults", s.setFault)
	mux.HandleFunc("GET /api/drift", s.driftForEndpoint)

	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.updateProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	mux.HandleFunc("POST /api/projects/{id}/activate", s.activateProject)
	mux.HandleFunc("POST /api/projects/deactivate", s.deactivateProjects)
	mux.HandleFunc("POST /api/projects/{id}/descriptor", s.uploadDescriptor)
	mux.HandleFunc("POST /api/projects/{id}/services", s.saveService)
	mux.HandleFunc("DELETE /api/services/{id}", s.deleteService)
	mux.HandleFunc("POST /api/discover", s.discoverServices)
	mux.HandleFunc("GET /api/runtime", s.runtimeStatus)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

type summaryJSON struct {
	ID           int64   `json:"id"`
	Target       string  `json:"target"`
	Protocol     string  `json:"protocol"`
	Method       string  `json:"method"`
	Path         string  `json:"path"`
	Status       int     `json:"status"`
	StartedAt    string  `json:"started_at"`
	DurationMS   float64 `json:"duration_ms"`
	Error        string  `json:"error,omitempty"`
	RequestSize  int64   `json:"request_size"`
	ResponseSize int64   `json:"response_size"`

	// For gRPC the HTTP status is 200 even when the call failed, so the real
	// outcome has to be in the listing or the listing lies.
	GRPCStatus     *int32 `json:"grpc_status,omitempty"`
	GRPCStatusText string `json:"grpc_status_text,omitempty"`
	GRPCMessage    string `json:"grpc_message,omitempty"`

	// GraphQL has both of gRPC's problems at once. Every operation is the same
	// POST to the same path, so GraphQLOp is what stops a service's whole lane
	// from reading as one repeated call; and a GraphQL failure arrives under
	// HTTP 200, so GraphQLErrors is what stops the listing calling it a
	// success.
	GraphQLOp     string `json:"graphql_op,omitempty"`
	GraphQLErrors int    `json:"graphql_errors,omitempty"`

	// Postgres has both problems again, and worse: every session to a database
	// is the same method and the same path, and a failed statement has no
	// status code anywhere — it is an ErrorResponse inside the stream.
	PostgresSummary string `json:"postgres_summary,omitempty"`
	PostgresErrors  int    `json:"postgres_errors,omitempty"`

	// ReplayOf turns "did the fix work?" into a diff instead of a memory
	// exercise, so it travels with the summary and reaches the live view.
	ReplayOf *int64 `json:"replay_of,omitempty"`

	// StubOf says the service was never called: this answer came out of a
	// recording. It has to reach every listing, because a stub that looks like
	// live traffic is the one thing that feature must never be.
	StubOf *int64 `json:"stub_of,omitempty"`

	// Injected says Sonda broke this call on purpose. It has to reach every
	// listing: a fault the field shows as the service's own sends someone
	// hunting a bug that does not exist.
	Injected bool `json:"injected,omitempty"`

	// TraceID is what groups this call with the rest of its request. Present
	// only when something upstream put it in the headers.
	TraceID string `json:"trace_id,omitempty"`
}

type messageJSON struct {
	Headers   http.Header `json:"headers"`
	Text      string      `json:"text,omitempty"`
	Base64    string      `json:"base64,omitempty"`
	Size      int64       `json:"size"`
	Stored    int         `json:"stored"`
	Truncated bool        `json:"truncated"`
}

type callJSON struct {
	summaryJSON
	ClientAddr       string      `json:"client_addr"`
	Request          messageJSON `json:"request"`
	Response         messageJSON `json:"response"`
	ResponseTrailers http.Header `json:"response_trailers,omitempty"`
	GRPC             *grpcView   `json:"grpc,omitempty"`

	// Socket, Stream, GraphQL and Postgres are the same idea as GRPC: what a
	// body means, read back out of the stored bytes when someone looks.
	Socket   *socketView   `json:"socket,omitempty"`
	Stream   *streamView   `json:"stream,omitempty"`
	GraphQL  *graphqlView  `json:"graphql,omitempty"`
	Postgres *postgresView `json:"postgres,omitempty"`
}

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{
		Target:     q.Get("target"),
		Method:     q.Get("method"),
		Path:       q.Get("path"),
		Protocol:   q.Get("protocol"),
		Search:     q.Get("q"),
		Project:    s.projectFilter(),
		FailedOnly: q.Get("failed") == "true",
	}

	if raw := q.Get("grpc_status"); raw != "" {
		code, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, "grpc_status must be a number")
			return
		}
		n := int32(code)
		f.GRPCStatus = &n
	}

	var err error
	if f.Status, err = intParam(q.Get("status")); err != nil {
		writeError(w, http.StatusBadRequest, "status must be a number")
		return
	}
	if f.Limit, err = intParam(q.Get("limit")); err != nil {
		writeError(w, http.StatusBadRequest, "limit must be a number")
		return
	}
	beforeID, err := intParam(q.Get("before_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "before_id must be a number")
		return
	}
	f.BeforeID = int64(beforeID)

	if f.Since, err = timeParam(q.Get("since")); err != nil {
		writeError(w, http.StatusBadRequest, "since must be an RFC3339 timestamp")
		return
	}
	if f.Until, err = timeParam(q.Get("until")); err != nil {
		writeError(w, http.StatusBadRequest, "until must be an RFC3339 timestamp")
		return
	}

	calls, err := s.store.List(r.Context(), f)
	if err != nil {
		slog.Error("list calls", "error", err)
		writeError(w, http.StatusInternalServerError, "could not list calls")
		return
	}

	out := make([]summaryJSON, 0, len(calls))
	for _, c := range calls {
		out = append(out, toSummary(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": out})
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a number")
		return
	}

	c, err := s.store.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no call with that id")
		return
	}
	if err != nil {
		slog.Error("get call", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read call")
		return
	}

	out := callJSON{
		summaryJSON:      toSummary(c.Summary()),
		ClientAddr:       c.ClientAddr,
		Request:          toMessage(c.Request),
		Response:         toMessage(c.Response),
		ResponseTrailers: c.ResponseTrailers,
	}
	if c.Protocol == config.ProtocolGRPC {
		out.GRPC = s.buildGRPCView(r.Context(), c)
	}
	if c.Protocol == config.ProtocolWebSocket {
		out.Socket = buildSocketView(c)
	}
	if c.Protocol == config.ProtocolPostgres {
		out.Postgres = buildPostgresView(c)
	}
	if isEventStream(c) {
		out.Stream = buildStreamView(c)
	}
	out.GraphQL = buildGraphQLView(c)
	writeJSON(w, http.StatusOK, out)
}

// listSchemas reports what each gRPC target's schema resolution produced. It
// answers the question the tool would otherwise leave hanging: field names are
// missing, and is that because reflection is off, the descriptor set is stale,
// or the service was down when Sonda asked?
func (s *Server) listSchemas(w http.ResponseWriter, r *http.Request) {
	type schemaStatus struct {
		Target        string `json:"target"`
		Source        string `json:"source"`
		DescriptorSet string `json:"descriptor_set,omitempty"`
		Reflection    bool   `json:"reflection"`
		Error         string `json:"error,omitempty"`
	}

	out := make([]schemaStatus, 0)
	for _, t := range s.targets() {
		if t.Protocol != config.ProtocolGRPC {
			continue
		}
		entry := schemaStatus{
			Target:        t.Name,
			DescriptorSet: t.DescriptorSet,
			Reflection:    t.ReflectionEnabled(),
		}
		if resolver := s.resolvers()[t.Name]; resolver != nil {
			source, err := resolver.Status(r.Context())
			entry.Source = string(source)
			if err != nil {
				entry.Error = err.Error()
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemas": out})
}

// channelColors is the logic-probe colour code, in its standard order, minus
// black — unreadable on the instrument ground.
//
// It is not decoration. With fifteen or more services on screen at once, each
// one needs an identity the eye can latch onto, and this is the ordering a
// developer who has ever wired a probe already knows by heart. Beyond nine
// targets the sequence repeats with a hollow mark, the way a second probe
// group is distinguished on a real instrument.
var channelColors = []string{
	"#8a5a3c", // brown
	"#c8443c", // red
	"#d97b2b", // orange
	"#d8b843", // yellow
	"#5fa858", // green
	"#4a86c8", // blue
	"#8b6bc4", // violet
	"#8a8f98", // grey
	"#d6dae0", // white
}

// Channel is a target's identity in the live view.
func channelFor(index int) (color string, hollow bool) {
	return channelColors[index%len(channelColors)], index >= len(channelColors)
}

func (s *Server) listTargets(w http.ResponseWriter, _ *http.Request) {
	type targetJSON struct {
		Name     string `json:"name"`
		Listen   string `json:"listen"`
		Upstream string `json:"upstream"`
		Protocol string `json:"protocol"`
		Color    string `json:"color"`
		Hollow   bool   `json:"hollow"`
	}
	targets := s.targets()
	out := make([]targetJSON, 0, len(targets))
	for i, t := range targets {
		color, hollow := channelFor(i)
		out = append(out, targetJSON{t.Name, t.Listen, t.Upstream, t.Protocol, color, hollow})
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context(), s.projectFilter())
	if err != nil {
		slog.Error("stats", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read stats")
		return
	}
	st.Dropped = s.dropped.Dropped()
	writeJSON(w, http.StatusOK, st)
}

// timeLayout keeps sub-second precision: two calls in the same burst have to be
// distinguishable, and rounding to the second would merge them.
const timeLayout = time.RFC3339Nano

func toSummary(c store.Summary) summaryJSON {
	out := summaryJSON{
		ID:            c.ID,
		Target:        c.Target,
		Protocol:      c.Protocol,
		Method:        c.Method,
		Path:          c.Path,
		Status:        c.Status,
		StartedAt:     c.StartedAt.Format(timeLayout),
		DurationMS:    float64(c.Duration.Microseconds()) / 1000,
		Error:         c.Error,
		RequestSize:   c.RequestSize,
		ResponseSize:  c.ResponseSize,
		GRPCStatus:    c.GRPCStatus,
		GRPCMessage:   c.GRPCMessage,
		ReplayOf:      c.ReplayOf,
		StubOf:        c.StubOf,
		Injected:      c.Injected,
		TraceID:       c.TraceID,
		GraphQLOp:     c.GraphQLOp,
		GraphQLErrors: c.GraphQLErrors,

		PostgresSummary: c.PostgresSummary,
		PostgresErrors:  c.PostgresErrors,
	}
	if c.GRPCStatus != nil {
		out.GRPCStatusText = codes.Code(*c.GRPCStatus).String()
	}
	return out
}

// toMessage renders a stored body without deciding what it means. UTF-8 goes
// out as text, anything else as base64 — pretty-printing and protobuf decoding
// belong to the view, not to storage.
func toMessage(m store.Message) messageJSON {
	out := messageJSON{
		Headers:   m.Headers,
		Size:      m.Size,
		Stored:    len(m.Body),
		Truncated: m.Truncated,
	}
	if len(m.Body) == 0 {
		return out
	}
	if utf8.Valid(m.Body) {
		out.Text = string(m.Body)
	} else {
		out.Base64 = base64.StdEncoding.EncodeToString(m.Body)
	}
	return out
}

func intParam(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func timeParam(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

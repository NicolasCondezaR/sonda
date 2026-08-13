// Package mcp exposes captured traffic to coding agents.
//
// The point is to close a loop that is otherwise manual: an agent writes code,
// runs it, and then has to be *told* what happened on the wire by a human
// copying a log. With this, it can ask — and what it gets back are the bytes
// that actually crossed, with protobuf decoded, rather than whatever somebody
// decided to log.
//
// Everything here is defined in terms of Sonda's own HTTP API rather than
// against the store directly. That is deliberate: replay and diff already live
// as handlers, the API is already tested, and `http.Handler` is an interface
// the standard library gave us for free. In-process it is the real API; for
// `sonda mcp` it is a handler that forwards to a running Sonda. One set of
// tools, two transports, no duplicated query logic.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// protocolVersion is the revision of MCP this server speaks, and what a client
// that asks for something unknown is told — the specification then has the
// client decide whether it can live with the answer.
const protocolVersion = "2025-11-25"

// supportedVersions are the revisions this surface actually satisfies, and the
// specification requires echoing the one the client asked for when it is among
// them. Nothing here uses anything the older two lack: the whole server is
// tools, and tools with annotations have been in every one of these.
var supportedVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2024-11-05": true,
}

// JSON-RPC error codes from the specification. Only the ones that can actually
// happen here are named.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message expects no reply. A JSON-RPC
// notification has no id, and answering one is a protocol violation that some
// clients treat as fatal.
func (r request) isNotification() bool { return len(r.ID) == 0 }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func result(id json.RawMessage, v any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: v}
}

func failure(id json.RawMessage, code int, format string, args ...any) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: fmt.Sprintf(format, args...)},
	}
}

// Server answers MCP requests about one Sonda.
//
// It holds no state of its own about a project. Which variable pointed at a
// service is kept with the service, in the store, and read back through the
// API: two places remembering it is how one of them ends up wrong, and the one
// in memory was always the one that forgot — a Sonda restarted between
// connecting and disconnecting could no longer undo its own edit.
type Server struct {
	api                 apiCaller
	version             string
	tools               []Tool
	readLocalSchemaFile func(string) ([]byte, error)
}

func New(api apiCaller, version string) *Server {
	s := &Server{api: api, version: version}
	s.tools = allTools(false)
	return s
}

// NewStdio builds the local adapter launched by `sonda mcp`.
//
// It is deliberately separate from New: only this process may turn a local
// descriptor path into bytes. The HTTP MCP endpoint uses New and therefore
// has neither the tool schema nor the filesystem reader for that capability.
// uploadSchemas also checks the request transport before invoking the reader,
// so accidentally serving this value over HTTP still cannot read a path.
func NewStdio(api apiCaller, version string) *Server {
	s := &Server{api: api, version: version, readLocalSchemaFile: readLocalDescriptorSet}
	s.tools = allTools(true)
	return s
}

// Handle dispatches one message. It returns nil when the message was a
// notification, which the transports use to decide whether to write anything
// back at all.
func (s *Server) Handle(ctx context.Context, raw []byte) *response {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return failure(nil, codeParse, "the message is not valid JSON: %v", err)
	}
	if req.JSONRPC != "2.0" {
		return failure(req.ID, codeInvalidRequest, "expected jsonrpc 2.0, got %q", req.JSONRPC)
	}

	switch req.Method {
	case "initialize":
		return result(req.ID, s.initialize(req.Params))

	case "tools/list":
		return result(req.ID, s.listTools())

	case "tools/call":
		return s.callTool(ctx, req)

	case "ping":
		return result(req.ID, map[string]any{})

	default:
		// Notifications the server does not care about — initialized,
		// cancelled, progress — are still well-formed messages, and a
		// notification never gets an answer, not even an error.
		if req.isNotification() {
			return nil
		}
		return failure(req.ID, codeMethodNotFound, "unknown method %q", req.Method)
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	return map[string]any{
		"protocolVersion": negotiate(params),
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "sonda",
			"title":   "Sonda",
			"version": s.version,
		},
		// The client shows this to the model. It is the one place to say what
		// the tools are *for*, so an agent reaches for them at the right
		// moment instead of guessing from seven names.
		"instructions": instructions,
	}
}

// negotiate answers with the version the client asked for whenever this server
// supports it, which the specification requires: a client told a version it did
// not ask for has to decide whether to disconnect, and doing that to a client
// this server is perfectly compatible with is a hang-up for nothing.
func negotiate(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && supportedVersions[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return protocolVersion
}

const instructions = `Sonda is a capturing proxy sitting between local services. It holds the real
bytes of every HTTP and gRPC call that crossed it, with protobuf decoded.

Reach for it when something failed and the reason is not in the code you can
see: recent_failures answers "what just broke", diff_calls answers "this one
worked and this one did not, what changed", and wait_for_call lets you trigger
something and then verify what actually went over the wire.

Credentials are never returned. Authorization headers, cookies and similar
fields come back as [redacted by Sonda], and this cannot be turned off.`

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The tools are shaped around what an agent asks while debugging, which is not
// the same as what Sonda's REST API exposes. "What just broke" is one question,
// not a filter combination the agent has to invent; "this one worked and this
// one did not" is a diff, not two fetches and a comparison it has to do in its
// own context.

type Tool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	Annotations map[string]any
	Run         func(ctx context.Context, s *Server, a args) (any, error)
}

// args gives typed reads over the untyped arguments a client sends. JSON
// numbers all arrive as float64, which is the single most common way to get
// this wrong.
type args map[string]any

func (a args) str(key string) string {
	if v, ok := a[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (a args) num(key string, fallback int) int {
	switch v := a[key].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func (a args) has(key string) bool { _, ok := a[key]; return ok }

func (a args) boolean(key string) bool {
	switch v := a[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

// readOnly marks a tool that cannot change anything. Clients use it to decide
// what to run without asking.
var readOnly = map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}

func allTools() []Tool {
	return append(readTools(), configureTools()...)
}

func readTools() []Tool {
	return []Tool{
		{
			Name:  "recent_failures",
			Title: "Recent failures",
			Description: "What has failed recently: transport errors, HTTP 4xx and 5xx, non-zero gRPC statuses, and GraphQL responses carrying errors, newest first. " +
				"The last two are the ones a status code hides — both report failure under HTTP 200. This is the first thing to ask when something is broken and you do not know where.",
			Schema: obj(map[string]any{
				"limit": prop("integer", "How many to return. Defaults to 20."),
			}),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				q := url.Values{}
				q.Set("failed", "true")
				q.Set("limit", strconv.Itoa(clampLimit(a.num("limit", 20))))
				return s.get(ctx, "/api/calls?"+q.Encode(), false)
			},
		},

		{
			Name:  "search_calls",
			Title: "Search captured calls",
			Description: "Find captured calls by service, method, path, protocol, status or free text in the bodies. Every filter is optional and they combine. " +
				"Sockets and event streams are captures like any other, so the same filters reach them. " +
				"GraphQL rides on http and every operation shares one path, so search it by operation name in text rather than by path.",
			Schema: obj(map[string]any{
				"service": prop("string", "Service name, as Sonda knows it — for example ms-auth."),
				"method":  prop("string", "HTTP method, or the gRPC method name."),
				"path":    prop("string", "Path or fragment of one."),
				// GraphQL is not in the enum because it is not a transport
				// Sonda proxies: a GraphQL service is configured as http and
				// its calls are stored as http. Offering it here would filter
				// on a value no capture can ever hold.
				"protocol": map[string]any{"type": "string", "enum": []string{"http", "grpc", "websocket"}, "description": "Restrict to one protocol. A websocket is one capture holding the whole conversation, not one per message. GraphQL is http."},
				"text":     prop("string", "Free text to look for inside the request and response bodies — a GraphQL operation name finds its calls this way."),
				"status":   prop("integer", "Exact HTTP status."),
				"failed":   prop("boolean", "Only calls that failed, including GraphQL responses carrying errors under HTTP 200."),
				"since_minutes": prop("integer",
					"Only calls from the last N minutes. Useful right after triggering something."),
				"limit": prop("integer", "How many to return. Defaults to 20, capped at 200."),
			}),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				q := url.Values{}
				setIf(q, "target", a.str("service"))
				setIf(q, "method", a.str("method"))
				setIf(q, "path", a.str("path"))
				setIf(q, "protocol", a.str("protocol"))
				setIf(q, "q", a.str("text"))
				if a.has("status") {
					setIf(q, "status", strconv.Itoa(a.num("status", 0)))
				}
				if a.boolean("failed") {
					q.Set("failed", "true")
				}
				if m := a.num("since_minutes", 0); m > 0 {
					q.Set("since", time.Now().Add(-time.Duration(m)*time.Minute).UTC().Format(time.RFC3339))
				}
				q.Set("limit", strconv.Itoa(clampLimit(a.num("limit", 20))))
				return s.get(ctx, "/api/calls?"+q.Encode(), false)
			},
		},

		{
			Name:  "get_call",
			Title: "Read one call",
			Description: "One captured call in full: headers, both bodies decoded, timing, gRPC status and trailers. " +
				"A WebSocket comes back as the frames of both directions, unmasked and labelled by kind, with the close code when there is one. " +
				"A server-sent event stream comes back as its events. " +
				"A GraphQL POST comes back as the operations it carried — type, name, the fields asked for, the variables sent, and any errors the response held, with their path and code. " +
				"Bodies are shortened unless you ask for detail.",
			Schema: obj(map[string]any{
				"id":     prop("integer", "The call id, as returned by the other tools."),
				"detail": prop("boolean", "Return bodies whole instead of shortened. Ask for this only when you need the full payload."),
			}, "id"),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				id := a.num("id", 0)
				if id <= 0 {
					return nil, fmt.Errorf("id is required and must be a positive number")
				}
				return s.get(ctx, "/api/calls/"+strconv.Itoa(id), a.boolean("detail"))
			},
		},

		{
			Name:  "trace_call",
			Title: "The whole request a call belonged to",
			Description: "Show every call that was part of the same request, arranged as a tree, with timings and which one failed. Answers \"where did it break and who was slow\" for an action that touched several services — the question a flat list cannot answer. " +
				"Calls are grouped by a trace id when the request carried one, and by timing when it did not; the answer says which, and a shape that was guessed is marked as guessed.",
			Schema: obj(map[string]any{
				"id": prop("integer", "Any call in the request. Usually the one that failed."),
			}, "id"),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				id := a.num("id", 0)
				if id <= 0 {
					return nil, fmt.Errorf("id is required and must be a call id")
				}
				return s.get(ctx, "/api/trace?call="+strconv.Itoa(id), false)
			},
		},

		{
			Name:  "contract_drift",
			Title: "Has this response changed shape",
			Description: "Compare the shape of a service's newest response against the oldest one Sonda holds for the same endpoint: fields that went away, fields that changed type, fields that appeared. " +
				"This is about shape, not values — two calls returning different prices are not drift; one returning a price as a number and the other as a string is. " +
				"Ask it when a caller broke and nothing in its own code changed.",
			Schema: obj(map[string]any{
				"service": prop("string", "Service name."),
				"path":    prop("string", "Restrict to one path, or a fragment of one."),
				"method":  prop("string", "Restrict to one method."),
				"a":       prop("integer", "Compare these two calls instead, when you already know which."),
				"b":       prop("integer", "The second call id."),
			}),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				q := url.Values{}
				if a.has("a") && a.has("b") {
					q.Set("a", strconv.Itoa(a.num("a", 0)))
					q.Set("b", strconv.Itoa(a.num("b", 0)))
				} else {
					service := a.str("service")
					if service == "" {
						return nil, fmt.Errorf("pass a service, or two call ids as a and b")
					}
					q.Set("target", service)
					setIf(q, "path", a.str("path"))
					setIf(q, "method", a.str("method"))
				}
				return s.get(ctx, "/api/drift?"+q.Encode(), false)
			},
		},

		{
			Name:        "diff_calls",
			Title:       "Compare two calls",
			Description: "Structurally compare two captured calls and report what changed: headers, bodies, status, timing. Answers \"this one worked and this one did not, why\" without you having to hold both payloads in context.",
			Schema: obj(map[string]any{
				"a": prop("integer", "Id of the first call, usually the one that worked."),
				"b": prop("integer", "Id of the second call, usually the one that failed."),
			}, "a", "b"),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				x, y := a.num("a", 0), a.num("b", 0)
				if x <= 0 || y <= 0 {
					return nil, fmt.Errorf("both a and b are required and must be call ids")
				}
				return s.get(ctx, fmt.Sprintf("/api/diff?a=%d&b=%d", x, y), false)
			},
		},

		{
			Name:        "list_services",
			Title:       "Services being observed",
			Description: "Which services Sonda is proxying right now, on which ports, and whether each one is actually listening. Also reports which project is active. Ask this first if you are unsure whether the traffic you expect is even being captured.",
			Schema:      obj(map[string]any{}),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				return s.get(ctx, "/api/projects", false)
			},
		},

		{
			Name:  "wait_for_call",
			Title: "Wait for a matching call",
			Description: "Block until a call matching the filters is captured, or until the timeout. Use it to verify a change: trigger the action, then wait for what should have crossed the wire. " +
				"Returns the matching calls, or reports that nothing arrived — which is itself an answer.",
			Schema: obj(map[string]any{
				"service":         prop("string", "Only calls to this service."),
				"path":            prop("string", "Only calls whose path contains this."),
				"method":          prop("string", "Only calls with this method."),
				"failed":          prop("boolean", "Only wait for a failure, GraphQL errors under HTTP 200 included."),
				"timeout_seconds": prop("integer", "How long to wait. Defaults to 30, capped at 120."),
			}),
			Annotations: readOnly,
			Run:         waitForCall,
		},

		{
			Name:  "replay_call",
			Title: "Replay a call",
			Description: "Send a captured call again, byte for byte, to the service it originally went to. Reproduces a bug without redoing whatever produced it. " +
				"This really hits the service and can change its state, so it is not a read. Refuses calls whose capture was truncated, since replaying a partial body would reproduce something that never happened.",
			Schema: obj(map[string]any{
				"id": prop("integer", "Id of the call to replay."),
			}, "id"),
			// Not read-only and destructive: the client should ask before
			// running this. Sonda cannot enforce that, but it can be honest
			// about what the tool does.
			Annotations: map[string]any{
				"readOnlyHint":    false,
				"destructiveHint": true,
				"idempotentHint":  false,
				"openWorldHint":   true,
			},
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				id := a.num("id", 0)
				if id <= 0 {
					return nil, fmt.Errorf("id is required and must be a positive number")
				}
				return s.post(ctx, fmt.Sprintf("/api/calls/%d/replay", id), nil)
			},
		},
	}
}

// waitForCall polls rather than subscribing. Sonda has a live stream, but it
// is server-sent events, and holding one open across an MCP request buys
// nothing here: the agent is going to wait either way, and a poll cannot leave
// a stream dangling if the client walks away mid-call.
func waitForCall(ctx context.Context, s *Server, a args) (any, error) {
	timeout := a.num("timeout_seconds", 30)
	if timeout > 120 {
		timeout = 120
	}
	if timeout < 1 {
		timeout = 1
	}

	q := url.Values{}
	setIf(q, "target", a.str("service"))
	setIf(q, "path", a.str("path"))
	setIf(q, "method", a.str("method"))
	if a.boolean("failed") {
		q.Set("failed", "true")
	}
	q.Set("limit", "20")
	// Only calls captured from now on. Without this the first poll would
	// return whatever happened to be there already and the wait would be a
	// lie.
	started := time.Now().UTC()
	q.Set("since", started.Format(time.RFC3339))

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		payload, err := s.raw(ctx, "GET", "/api/calls?"+q.Encode(), nil)
		if err == nil {
			var body struct {
				Calls []json.RawMessage `json:"calls"`
			}
			if json.Unmarshal(payload, &body) == nil && len(body.Calls) > 0 {
				cleaned, err := cleanJSON(payload, false)
				if err != nil {
					return nil, err
				}
				return map[string]any{"matched": true, "waited_seconds": int(time.Since(started).Seconds()), "result": cleaned}, nil
			}
		}

		select {
		case <-ctx.Done():
			// Nothing arriving is a finding, not a failure: it usually means
			// the caller was never pointed at Sonda in the first place.
			return map[string]any{
				"matched":        false,
				"waited_seconds": timeout,
				"hint":           "No matching call arrived. Check with list_services that the service is being observed, and that whatever makes the call is pointed at Sonda's port rather than the service's own.",
			}, nil
		case <-ticker.C:
		}
	}
}

func clampLimit(n int) int {
	if n <= 0 {
		return 20
	}
	if n > 200 {
		return 200
	}
	return n
}

func setIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

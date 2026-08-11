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

// boolean reads a flag that has already been checked against the schema, so
// anything that is not a bool is absent rather than false.
//
// It used to coerce, and only the literal "true" counted: set_stub with
// enabled:"1" passed the has() check, resolved to false, turned stubbing off
// and reported that it had worked. A wrong type is refused in callTool now,
// where it can be said out loud.
func (a args) boolean(key string) bool {
	v, _ := a[key].(bool)
	return v
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
				"limit": prop("integer", "How many to return. Defaults to 20, capped at 200."),
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
				"Sockets, event streams and Postgres statements are captures like any other, so the same filters reach them. " +
				"GraphQL rides on http and every operation shares one path, so search it by operation name in text rather than by path. " +
				"A Postgres capture is one statement: its path is the database, its method is STATEMENT, and its text is the SQL, the values bound to it and what the server answered — so a table or column name in text finds the statements that touched it.",
			Schema: obj(map[string]any{
				"service": prop("string", "Service name, as Sonda knows it — for example ms-auth."),
				"method":  prop("string", "HTTP method, or the gRPC method name."),
				"path":    prop("string", "Path or fragment of one."),
				// The enum is exactly the set of values the proxy writes into
				// the column. GraphQL is not one: a GraphQL service is
				// configured as http and its calls are stored as http, so
				// offering it would filter on a value no capture can hold.
				// Postgres is, because it genuinely is a separate transport
				// with its own listener and its own captures.
				"protocol": map[string]any{"type": "string", "enum": []string{"http", "grpc", "websocket", "postgres"}, "description": "Restrict to one protocol. A websocket capture is one whole conversation; a postgres capture is one statement. GraphQL is http."},
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
				"A Postgres statement comes back as the protocol messages of both directions: the SQL, its bind parameters, the rows described, the command tags and any server error with its SQLSTATE. " +
				"The first statement of a connection also carries that connection's opening — the startup parameters and which authentication mechanism was demanded — because those happened once and belong somewhere. " +
				"The password and the cancellation key were blanked when the bytes were captured, so they are not there to ask for. " +
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
			Name:  "list_services",
			Title: "Services being observed",
			Description: "Which services Sonda is proxying right now, on which ports, and whether each one is actually listening. Also reports which project is active. Ask this first if you are unsure whether the traffic you expect is even being captured. " +
				"Each service also reports tls — whether Sonda answers that port with a certificate — and insecure_skip_verify, which means the upstream's certificate is not being checked for that one service. " +
				"Each project reports has_descriptor and descriptor_name: that is the answer to whether its gRPC messages can be decoded at all, since a service serving no reflection has nothing else to read a schema from. " +
				"The answer also carries the two runtime switches, so they cannot be left on unnoticed: stubbed lists the services answering from recordings instead of being called, and faults lists the failures being injected.",
			Schema:      obj(map[string]any{}),
			Annotations: readOnly,
			Run:         listServices,
		},

		{
			Name:  "schema_status",
			Title: "Where gRPC field names are coming from",
			Description: "Per gRPC service: whether its schema came from the service's own reflection or from the project's descriptor set, which descriptor set that was, and the error when neither worked. " +
				"Ask it when messages come back without field names. It separates the three causes that look identical from the outside — reflection is off or unsupported, the descriptor set does not cover this service, or the service was down when Sonda asked — and only the last one fixes itself.",
			Schema:      obj(map[string]any{}),
			Annotations: readOnly,
			Run:         schemaStatus,
		},

		{
			Name:  "diagnose_silence",
			Title: "Why is nothing being captured",
			Description: "Why you are seeing nothing. Reports, per service: whether the listener actually bound, how many TCP connections that port has accepted, how many calls were captured and how long ago the last one was, and what the listener expects to speak — then names the cause. " +
				"Where the evidence does not separate two causes it says both and says what would tell them apart, because a confident wrong answer here sends you down a road with nothing at the end of it. " +
				"The connection count is the reading that matters most: connections with no captures means a client found the port and Sonda did not understand it — a TLS mismatch, or a protocol Sonda does not proxy. Zero connections means nothing arrived at all, and Sonda cannot see a client that never connected. " +
				"Ask this the moment a call you expected is not in the capture list, and before search_calls returns empty twice. " +
				"Set probe_upstreams to also dial each upstream once: that is traffic the user did not send, so it is off by default, it never happens on its own, and it goes straight to the service rather than through Sonda, so it cannot show up as a capture.",
			Schema: obj(map[string]any{
				"probe_upstreams": prop("boolean", "Also open and immediately close one TCP connection to each upstream, to find out whether the service behind it is up. No bytes are sent. Defaults to false."),
			}),
			// Read-only in the sense clients care about: it changes nothing in
			// Sonda and nothing in the user's services. openWorldHint is true
			// because with probe_upstreams it does touch the network.
			Annotations: map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": true},
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				if a.boolean("probe_upstreams") {
					return s.post(ctx, "/api/diagnose", nil)
				}
				return s.get(ctx, "/api/diagnose", false)
			},
		},

		{
			Name:  "trust_certificate",
			Title: "How to trust Sonda's certificate authority",
			Description: "Where Sonda's local certificate authority lives, what it identifies as, and the exact commands to trust it and to remove it again, per platform. " +
				"Sonda never installs it: modifying a machine's trust store is the user's decision, so hand these commands to them rather than running one. " +
				"Ask for this after setting a service to terminate TLS, or when a client refuses Sonda's certificate. The private key is not available here or anywhere else in this API.",
			Schema:      obj(map[string]any{}),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				return s.get(ctx, "/api/tls", false)
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

// schemaStatus reports where each gRPC service's field names came from, and
// says why when there is nothing to report.
//
// This reads the project whose ports are open, so before activate_project the
// honest answer is an empty list — and an empty list on its own reads as "no
// schemas resolved", which sends an agent looking for a descriptor set problem
// that does not exist. set_stub and break_service say the same thing through
// sequencing(); this one has no error to attach it to.
func schemaStatus(ctx context.Context, s *Server, _ args) (any, error) {
	out, err := s.get(ctx, "/api/schemas", false)
	if err != nil {
		return nil, err
	}
	body, ok := out.(map[string]any)
	if !ok {
		return out, nil
	}
	if list, _ := body["schemas"].([]any); len(list) == 0 {
		body["note"] = "Nothing to report, which is not the same as nothing working. " +
			"This reads only the project whose ports are open: if activate_project has not been called yet, that is the cause; otherwise the active project has no grpc service in it. " +
			"list_services says which project is active and what is in it."
	}
	return body, nil
}

// listServices answers with the configuration and the two runtime switches at
// once.
//
// Stubbing and fault injection are state a person forgets they turned on, and
// until now the only way for an agent to read either was to call the tool that
// changes it — which a client interrupts to ask permission for, so the cheapest
// way to learn "is anything stubbed" was to offer to stub something. They are
// folded in here rather than given two read-only tools of their own because
// this is already the call an agent makes to orient itself, and both are
// properties of exactly the services it lists.
func listServices(ctx context.Context, s *Server, _ args) (any, error) {
	out, err := s.get(ctx, "/api/projects", false)
	if err != nil {
		return nil, err
	}
	projects, ok := out.(map[string]any)
	if !ok {
		return out, nil
	}
	projects["stubbed"] = switchState(ctx, s, "/api/stub", "stubbed")
	projects["faults"] = switchState(ctx, s, "/api/faults", "faults")
	return projects, nil
}

// switchState reads one runtime switch. A Sonda that cannot answer says so in
// place of the value: an empty list would read as "nothing is stubbed", which
// is the one wrong answer this must never give.
func switchState(ctx context.Context, s *Server, path, field string) any {
	out, err := s.get(ctx, path, false)
	if err != nil {
		return map[string]any{"unknown": err.Error()}
	}
	if body, ok := out.(map[string]any); ok {
		if value, ok := body[field]; ok {
			return value
		}
	}
	return out
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
				"hint":           "No matching call arrived. Run diagnose_silence: it reports whether the listener bound, whether anything connected to the port at all, and which causes it cannot tell apart.",
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

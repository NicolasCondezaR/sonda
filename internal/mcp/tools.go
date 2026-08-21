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

func allTools(localSchemaFiles bool) []Tool {
	return append(readTools(), configureTools(localSchemaFiles)...)
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
				"Sockets, event streams, Postgres statements and AMQP units are captures like any other, so the same filters reach them. " +
				"GraphQL rides on http and every operation shares one path, so search it by operation name in text rather than by path. " +
				"A Postgres capture is one statement: its path is the database, its method is STATEMENT, and its text is the SQL, the values bound to it and what the server answered — so a table or column name in text finds the statements that touched it.",
			Schema: obj(map[string]any{
				"service": prop("string", "Service name, as Sonda knows it — for example ms-auth."),
				"method":  prop("string", "HTTP method, gRPC method name, Postgres row kind, or AMQP method such as basic.publish."),
				"path":    prop("string", "Path or fragment of one. For AMQP this is the route, queue, virtual host or channel."),
				// The enum is exactly the set of values the proxy writes into
				// the column. GraphQL is not one: a GraphQL service is
				// configured as http and its calls are stored as http, so
				// offering it would filter on a value no capture can hold.
				// Postgres is, because it genuinely is a separate transport
				// with its own listener and its own captures.
				"protocol": map[string]any{"type": "string", "enum": []string{"http", "grpc", "websocket", "postgres", "amqp"}, "description": "Restrict to one protocol. A websocket capture is one whole conversation; a postgres capture is one statement; an amqp capture is one method or content-bearing unit. GraphQL is http."},
				"text":     prop("string", "Free text to look for inside the request and response bodies — a GraphQL operation name finds its calls this way."),
				"status":   prop("integer", "Exact HTTP status."),
				"failed":   prop("boolean", "true returns only the calls that failed, including GraphQL responses carrying errors under HTTP 200; false returns only the calls that did not fail; leave it out and both come back."),
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
				setFlag(q, "failed", a, "failed")
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
			// This description used to inventory, per protocol, which fields
			// come back: WebSocket frames, SSE events, GraphQL operations,
			// Postgres messages, AMQP frames. About two hundred tokens of it,
			// paid on every request by every client that loads tool
			// definitions eagerly — to describe a payload the reader is about
			// to receive and can simply read. What cannot be discovered from
			// the answer is what stayed: that credentials were blanked at
			// capture time rather than withheld here, because an agent that
			// does not know this asks again with detail and pays twice for
			// nothing; and that lists and strings are shortened, because that
			// is the difference between a short answer and a missing one.
			Description: "One captured call in full: headers, both bodies decoded, timing, protocol-specific messages, and the terminal status. The shape follows the protocol — frames for a socket, operations for GraphQL, statements for Postgres, frames for AMQP. " +
				"Database passwords and AMQP SASL exchanges were blanked when the bytes were captured, so they are not here to ask for; the mechanism that was chosen is still named. " +
				"Long strings and long lists are shortened, and each says what it left out. Ask for detail only when you need the whole payload.",
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
			Name:  "diff_flows",
			Title: "Compare two runs of the same flow",
			Description: "Compare two whole requests — every call each of them set off — and report where they parted ways. Answers \"this worked yesterday and today it does not\", which comparing two single calls cannot: the call that changed is usually several hops down, or is a call that stopped being made at all. " +
				"Give it one call id from each run; the rest of both trees is found for you. The answer names the first divergence, lists what changed per aligned call, and lists the calls that exist in only one of the runs. " +
				"Calls are aligned by service, protocol, method and the shape of the path, so ids in the path do not stop two runs matching. Check unmatched before believing the rest: a high count means the paths did not align, not that everything changed. " +
				"certain is false when either run was grouped by timing instead of a real trace id, and same_entry is false when the two seeds were not even the same call.",
			Schema: obj(map[string]any{
				"a":         prop("integer", "Any call from the first run, usually the one that worked."),
				"b":         prop("integer", "Any call from the second run, usually the one that failed."),
				"normalize": prop("string", "How hard to look at a path segment before treating it as a value: strict (default), loose, or off. Use loose when ids in paths are unusual and nothing aligns; off when the routes carry no ids at all."),
				"bodies":    prop("string", "Which payloads to compare: first (default, the divergence and its direct children), all, or none. all is expensive on a wide flow and fills your context with JSON."),
			}, "a", "b"),
			Annotations: readOnly,
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				x, y := a.num("a", 0), a.num("b", 0)
				if x <= 0 || y <= 0 {
					return nil, fmt.Errorf("both a and b are required and must be call ids, one from each run")
				}
				q := url.Values{}
				q.Set("a", strconv.Itoa(x))
				q.Set("b", strconv.Itoa(y))
				setIf(q, "normalize", a.str("normalize"))
				setIf(q, "bodies", a.str("bodies"))
				return s.get(ctx, "/api/flowdiff?"+q.Encode(), false)
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
			Description: "Why you are seeing nothing. Reports, per service: whether the listener bound, how many TCP connections that port accepted, how many calls were captured and how long ago, what the listener expects to speak — then names the cause. Where the evidence does not separate two causes it says both, and says what would tell them apart. " +
				"The connection count is the reading that matters most: connections with no captures means a client found the port and Sonda did not understand it — a TLS mismatch, or a protocol Sonda does not proxy. Zero connections means nothing arrived at all, and Sonda cannot see a client that never connected. " +
				"Services that read the same are grouped so one reading is not repeated per service; the shape field in the answer says how to read it. " +
				"Ask this the moment a call you expected is not in the capture list, and before search_calls returns empty twice. " +
				"Set probe_upstreams to also dial each upstream once: that is traffic the user did not send, so it is off by default and goes straight to the service rather than through Sonda, so it cannot show up as a capture.",
			Schema: obj(map[string]any{
				"probe_upstreams": prop("boolean", "Also open and immediately close one TCP connection to each upstream, to find out whether the service behind it is up. No bytes are sent. Defaults to false."),
			}),
			// Read-only in the sense clients care about: it changes nothing in
			// Sonda and nothing in the user's services. openWorldHint is true
			// because with probe_upstreams it does touch the network.
			Annotations: map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": true},
			Run: func(ctx context.Context, s *Server, a args) (any, error) {
				var out any
				var err error
				if a.boolean("probe_upstreams") {
					out, err = s.post(ctx, "/api/diagnose", nil)
				} else {
					out, err = s.get(ctx, "/api/diagnose", false)
				}
				if err != nil {
					return nil, err
				}
				// Compacting happens here rather than in the API: the web and
				// the terminal render every service at no cost, and only the
				// trip into a model's context is expensive. See compact.go.
				if body, ok := out.(map[string]any); ok {
					return compactDiagnosis(body), nil
				}
				return out, nil
			},
		},

		{
			Name:  "trust_certificate",
			Title: "How to trust Sonda's certificate authority",
			Description: "Sonda's local certificate authority: the certificate itself in `certificate_pem`, where Sonda keeps it, what it identifies as, and the exact commands to trust it and to remove it again, per platform. " +
				"The commands name the path Sonda sees, which is not a path you can open when Sonda runs in a container — so write `certificate_pem` to a file of your own and use that path instead. When Sonda can tell it is containerised it also returns the `docker cp` that copies the file out. " +
				"Sonda never installs it: modifying a machine's trust store is the user's decision, so hand these commands to them rather than running one. " +
				"Ask for this after setting a service to terminate TLS, or when a client refuses Sonda's certificate. Only the public certificate is here; the private key is not available through this tool or anywhere else in this API.",
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
				"failed":          prop("boolean", "true waits only for a failure, GraphQL errors under HTTP 200 included; false waits only for a call that did not fail — the way to confirm a fix landed; leave it out and any matching call ends the wait."),
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
	list, _ := body["schemas"].([]any)
	if len(list) == 0 {
		body["note"] = "Nothing to report, which is not the same as nothing working. " +
			"This reads only the project whose ports are open: if activate_project has not been called yet, that is the cause; otherwise the active project has no grpc service in it. " +
			"list_services says which project is active and what is in it."
		return body, nil
	}
	if unresolved := unresolvedTargets(list); len(unresolved) > 0 {
		body["fix"] = map[string]any{
			"unresolved": unresolved,
			"steps": []string{
				"Compile the whole proto tree to one descriptor set: `buf build <proto dir> -o descriptors.binpb`, or `protoc --include_imports --descriptor_set_out=descriptors.binpb <files>`. A buf.yaml scoped to one module does not scope this: buf build takes the directory you hand it, so a repository whose lint config covers a single module still compiles every service from its proto root.",
				"Load it with upload_schemas. Through the stdio adapter pass path and let it read the file; a real descriptor set is hundreds of kilobytes, and base64 of that spends more of your context than the answer is worth.",
			},
			"then": "Call schema_status again. A service that stays unresolved after a descriptor set loads is not covered by it — check that you compiled from the proto root and not from one service's directory.",
		}
	}
	return body, nil
}

// unresolvedTargets names the gRPC services whose field names Sonda cannot
// produce, which is the whole reason an agent calls schema_status.
//
// Reporting the per-service source and stopping there answers "what happened"
// and leaves "so what do I run" to whoever already knows — and the ordinary
// case is a system serving no reflection anywhere, where every service is
// unresolved for the same single reason and the fix is one command.
func unresolvedTargets(list []any) []string {
	var out []string
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// An empty source is SourceNone: neither reflection nor a descriptor
		// set produced anything. An error alongside a source is a resolution
		// that worked and then went stale, which this does not second-guess.
		if source, _ := entry["source"].(string); source != "" {
			continue
		}
		if name, _ := entry["target"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
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
	setFlag(q, "failed", a, "failed")
	q.Set("limit", "20")
	// Only calls captured from now on. Without this the first poll would
	// return whatever happened to be there already and the wait would be a
	// lie.
	//
	// To the nanosecond, and not to the second: the column is microseconds, so a
	// second-precision bound is really the start of the current second and
	// matches traffic captured before the wait began. Returning a call that
	// happened first destroys the only thing this tool is for.
	started := time.Now().UTC()
	q.Set("since", started.Format(time.RFC3339Nano))

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
				// The endpoint is passed rather than assumed: redaction is
				// chosen by position now, and a call site that hardcodes which
				// position it is at holds that agreement in a comment instead
				// of in the code that polls.
				cleaned, err := cleanAnswer("/api/calls?"+q.Encode(), payload, false)
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

// setFlag forwards a three-state flag: absent stays absent, and false travels
// as false rather than being dropped. Dropping it is how failed:false used to
// mean "no filter" and hand back the failures the caller had just excluded.
//
// has() is safe as the test because checkTypes has already refused anything
// that is not a bool for a property the schema declares boolean.
func setFlag(q url.Values, key string, a args, arg string) {
	if a.has(arg) {
		q.Set(key, strconv.FormatBool(a.boolean(arg)))
	}
}

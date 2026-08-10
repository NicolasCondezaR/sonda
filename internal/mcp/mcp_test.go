package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI stands in for Sonda. It records what was asked so a test can assert
// the tool built the right request, and answers with whatever the test wants.
type fakeAPI struct {
	paths  []string
	status int
	body   string
}

func (f *fakeAPI) Call(_ context.Context, method, path string, _ []byte) (int, []byte, error) {
	f.paths = append(f.paths, method+" "+path)
	status := f.status
	if status == 0 {
		status = 200
	}
	return status, []byte(f.body), nil
}

func (f *fakeAPI) last() string {
	if len(f.paths) == 0 {
		return ""
	}
	return f.paths[len(f.paths)-1]
}

func send(t *testing.T, s *Server, msg string) *response {
	t.Helper()
	return s.Handle(context.Background(), []byte(msg))
}

// callTool runs a tool end to end and returns the text the client would show
// the model, plus whether it was flagged as an error.
func callTool(t *testing.T, s *Server, name, arguments string) (string, bool) {
	t.Helper()
	if arguments == "" {
		arguments = "{}"
	}
	resp := send(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+arguments+`}}`)
	if resp == nil {
		t.Fatal("a request got no response")
	}
	if resp.Error != nil {
		t.Fatalf("protocol error: %s", resp.Error.Message)
	}
	out := resp.Result.(map[string]any)
	content := out["content"].([]map[string]any)
	return content[0]["text"].(string), out["isError"].(bool)
}

// --- the protocol itself ---

func TestInitializeAnswersWithTheProtocolVersion(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	resp := send(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)

	res := resp.Result.(map[string]any)
	if got := res["protocolVersion"]; got != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", got, protocolVersion)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("the server does not advertise the tools capability, so no client will list them")
	}
}

// A notification has no id and must get no reply at all. Answering one is a
// protocol violation that some clients treat as fatal.
func TestNotificationsAreNotAnswered(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	for _, msg := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
	} {
		if resp := send(t, s, msg); resp != nil {
			t.Errorf("%s got a reply: %+v", msg, resp)
		}
	}
}

// An unknown method that *is* a request still needs an error, or the client
// waits forever for an id that will never come back.
func TestUnknownRequestGetsAnError(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	resp := send(t, s, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected an error response, got %+v", resp)
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("code = %d, want %d", resp.Error.Code, codeMethodNotFound)
	}
}

func TestBrokenJSONIsAParseError(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	resp := send(t, s, `{"jsonrpc":"2.0","id":1,`)
	if resp == nil || resp.Error == nil || resp.Error.Code != codeParse {
		t.Fatalf("expected a parse error, got %+v", resp)
	}
	// An error reply must still carry an id field, even a null one.
	if len(resp.ID) == 0 {
		t.Error("the error response has no id at all")
	}
}

func TestEveryToolIsListedWithASchema(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	resp := send(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	tools := resp.Result.(map[string]any)["tools"].([]map[string]any)

	// Named rather than counted: a test that only counts passes when a tool is
	// renamed out from under every client that was calling it.
	want := map[string]bool{
		"recent_failures": true, "search_calls": true, "get_call": true,
		"diff_calls": true, "trace_call": true, "contract_drift": true, "list_services": true, "wait_for_call": true,
		"trust_certificate": true,
		"replay_call":       true, "connect_project": true, "configure_service": true,
		"activate_project": true, "disconnect_project": true, "set_stub": true, "break_service": true,
	}
	got := map[string]bool{}
	for _, tool := range tools {
		got[tool["name"].(string)] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is not listed", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s is listed but not expected — add it here on purpose", name)
		}
	}

	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Error("a tool has no name")
		}
		if d, _ := tool["description"].(string); d == "" {
			t.Errorf("%s has no description, so a model cannot tell when to use it", name)
		}
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s has no inputSchema", name)
		}
	}
}

// The annotation is the only way a client knows to ask first, so exactly which
// tools carry it is a decision, not an accident.
//
//	replay_call         really hits a service
//	activate_project    opens ports and can pull the floor out mid-debug
//	disconnect_project  closes them
//
// Everything else either reads, or writes configuration that disturbs nobody
// until a project is activated.
func TestOnlyTheToolsThatChangeWhatIsRunningAskFirst(t *testing.T) {
	shouldAsk := map[string]bool{
		"replay_call": true, "activate_project": true, "disconnect_project": true,
		"set_stub": true, "break_service": true,
	}

	s := New(&fakeAPI{}, "test")
	tools := send(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).
		Result.(map[string]any)["tools"].([]map[string]any)

	for _, tool := range tools {
		name := tool["name"].(string)
		ann, _ := tool["annotations"].(map[string]any)
		if ann == nil {
			t.Errorf("%s has no annotations", name)
			continue
		}
		destructive, _ := ann["destructiveHint"].(bool)
		if shouldAsk[name] && !destructive {
			t.Errorf("%s changes what is running but is not marked destructive, so clients will run it without asking", name)
		}
		if !shouldAsk[name] && destructive {
			t.Errorf("%s is marked destructive but changes nothing that is running", name)
		}
	}
}

// A tool that fails comes back as a normal result with isError set, not as a
// JSON-RPC error: the model has to be able to read what went wrong and try
// something else instead of the client treating the session as broken.
func TestAToolFailureIsAResultNotAProtocolError(t *testing.T) {
	s := New(&fakeAPI{status: 404, body: `{"error":"no call with that id"}`}, "test")

	text, isError := callTool(t, s, "get_call", `{"id":999}`)
	if !isError {
		t.Error("a failing tool did not set isError")
	}
	if !strings.Contains(text, "no call with that id") {
		t.Errorf("the message from Sonda did not reach the model: %q", text)
	}
}

// --- the tools build the right requests ---

func TestToolsQueryTheExpectedEndpoints(t *testing.T) {
	cases := []struct {
		tool, arguments string
		wantContains    []string
	}{
		{"recent_failures", `{"limit":5}`, []string{"GET /api/calls?", "failed=true", "limit=5"}},
		{"search_calls", `{"service":"ms-auth","failed":true}`, []string{"target=ms-auth", "failed=true"}},
		{"search_calls", `{"text":"ORD-9","limit":999}`, []string{"q=ORD-9", "limit=200"}},
		{"get_call", `{"id":42}`, []string{"GET /api/calls/42"}},
		{"diff_calls", `{"a":1,"b":2}`, []string{"GET /api/diff?a=1&b=2"}},
		{"trace_call", `{"id":42}`, []string{"GET /api/trace?call=42"}},
		{"list_services", `{}`, []string{"GET /api/projects"}},
		{"replay_call", `{"id":42}`, []string{"POST /api/calls/42/replay"}},
	}

	for _, c := range cases {
		api := &fakeAPI{body: `{"calls":[]}`}
		s := New(api, "test")
		if _, isError := callTool(t, s, c.tool, c.arguments); isError {
			t.Errorf("%s reported an error", c.tool)
			continue
		}
		for _, want := range c.wantContains {
			if !strings.Contains(api.last(), want) {
				t.Errorf("%s requested %q, which does not contain %q", c.tool, api.last(), want)
			}
		}
	}
}

// An id is not optional, and a missing one must fail with something a model
// can act on rather than fetching call zero.
func TestToolsRejectAMissingID(t *testing.T) {
	for _, tool := range []string{"get_call", "replay_call", "diff_calls"} {
		api := &fakeAPI{body: `{}`}
		s := New(api, "test")
		_, isError := callTool(t, s, tool, `{}`)
		if !isError {
			t.Errorf("%s accepted a call with no id", tool)
		}
		if len(api.paths) != 0 {
			t.Errorf("%s reached the API anyway: %v", tool, api.paths)
		}
	}
}

// --- the HTTP transport ---

// The specification requires this and the reason is concrete: without it a
// page in the developer's own browser could reach this endpoint through DNS
// rebinding and read every captured token.
func TestAForeignOriginIsRefused(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cases := []struct {
		origin string
		want   int
	}{
		{"", http.StatusOK},                      // command-line clients send none
		{"http://localhost:3000", http.StatusOK}, //
		{"http://127.0.0.1:9000", http.StatusOK}, //
		{"https://evil.example.com", http.StatusForbidden},
		{"http://sonda.attacker.test", http.StatusForbidden},
	}

	for _, c := range cases {
		req, err := http.NewRequest(http.MethodPost, srv.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("Origin %q got %d, want %d", c.origin, resp.StatusCode, c.want)
		}
	}
}

func TestANotificationOverHTTPGetsNoBody(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
}

func TestGETIsNotAllowed(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// --- the pipe transport ---

func TestStdioRoundTrip(t *testing.T) {
	s := New(&fakeAPI{body: `{"calls":[]}`}, "test")
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out strings.Builder

	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Two requests, one notification: exactly two lines, or the notification
	// leaked a reply into the stream.
	if len(lines) != 2 {
		t.Fatalf("%d lines written, want 2:\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		var resp response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Errorf("a line is not valid JSON: %v", err)
		}
		if resp.JSONRPC != "2.0" {
			t.Errorf("line missing jsonrpc 2.0: %s", line)
		}
	}
}

// A protocol Sonda captures but the schema does not offer is a protocol the
// agent cannot ask about. The enum has to keep up with what the proxy learned
// to speak, or the tool quietly narrows what the model can see.
func TestTheProtocolFilterOffersEveryProtocolSondaCaptures(t *testing.T) {
	s := New(&fakeAPI{}, "test")
	tools := send(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).
		Result.(map[string]any)["tools"].([]map[string]any)

	for _, tool := range tools {
		if tool["name"] != "search_calls" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		enum := props["protocol"].(map[string]any)["enum"].([]string)

		offered := map[string]bool{}
		for _, p := range enum {
			offered[p] = true
		}
		for _, want := range []string{"http", "grpc", "websocket"} {
			if !offered[want] {
				t.Errorf("search_calls cannot filter for %q", want)
			}
		}
		return
	}
	t.Fatal("search_calls is not listed")
}

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// routedAPI answers differently per endpoint, which the configuration tools
// need: connect_project alone talks to discover, projects and services in one
// run, and a fake that returns the same body to all three proves nothing.
type routedAPI struct {
	routes map[string]string
	calls  []string
	bodies []string

	// rejectBodiesWith makes one specific request fail. Matching on the path is
	// not enough: every service is saved to the same endpoint, and the case
	// worth testing is one of twenty failing while the rest go through.
	rejectBodiesWith string
}

func (r *routedAPI) Call(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	r.calls = append(r.calls, method+" "+path)
	r.bodies = append(r.bodies, string(body))
	if r.rejectBodiesWith != "" && strings.Contains(string(body), r.rejectBodiesWith) {
		return 400, []byte(`{"error":"this project already has a service called that"}`), nil
	}
	for prefix, answer := range r.routes {
		if strings.HasPrefix(method+" "+path, prefix) {
			return 200, []byte(answer), nil
		}
	}
	return 404, []byte(`{"error":"no route in the fake for that"}`), nil
}

func (r *routedAPI) called(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// A .env the way a real one looks: the service entries mixed in with the noise
// discovery is supposed to drop.
const discovered = `{"found":[
  {"name":"ms-auth","upstream":"http://localhost:50052","protocol":"grpc",
   "listen":"127.0.0.1:9152","key":"MS_AUTH_GRPC_URL","original":"localhost:50052"},
  {"name":"ms-admin","upstream":"http://localhost:50053","protocol":"grpc",
   "listen":"127.0.0.1:9153","key":"MS_ADMIN_GRPC_URL","original":"localhost:50053",
   "port_taken":true,"port_error":"address already in use"}
]}`

func connectFake() *routedAPI {
	return &routedAPI{routes: map[string]string{
		"POST /api/discover": discovered,
		"POST /api/projects": `{"id":3,"name":"core-delpagroup"}`,
		"GET /api/projects":  `{"projects":[{"id":3,"name":"core-delpagroup","active":false,"services":[]}]}`,
	}}
}

func connect(t *testing.T, api *routedAPI) map[string]any {
	t.Helper()
	s := New(api, "test")
	text, isError := callTool(t, s, "connect_project",
		`{"name":"core-delpagroup","files":[{"filename":".env","content":"MS_AUTH_GRPC_URL=localhost:50052"}]}`)
	if isError {
		t.Fatalf("connect_project failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The patch is the whole point of the tool. Sonda cannot repoint a caller, so
// what it hands back is the edit that makes the traffic flow through it — and
// an edit missing the old value is one nobody can reverse or verify.
func TestConnectProjectReturnsTheEditToApply(t *testing.T) {
	out := connect(t, connectFake())

	changes, ok := out["changes"].(map[string]any)
	if !ok {
		t.Fatalf("no changes in the answer: %v", out)
	}
	entry, ok := changes["MS_AUTH_GRPC_URL"].(map[string]any)
	if !ok {
		t.Fatalf("MS_AUTH_GRPC_URL is not in the patch: %v", changes)
	}
	if entry["from"] != "localhost:50052" {
		t.Errorf(`from = %v, want the value as it is written today`, entry["from"])
	}
	if entry["to"] != "127.0.0.1:9152" {
		t.Errorf("to = %v, want Sonda's port", entry["to"])
	}
	if out["next_steps"] == nil {
		t.Error("the answer does not say what to do with the patch")
	}
}

// Creating configuration disturbs nobody; opening ports can. The split is the
// permission decision this tool exists under, and it has to hold here.
func TestConnectProjectDoesNotOpenPorts(t *testing.T) {
	api := connectFake()
	out := connect(t, api)

	if active, _ := out["active"].(bool); active {
		t.Error("connect_project reported the project as active")
	}
	if api.called("POST /api/projects/3/activate") {
		t.Error("connect_project activated the project by itself")
	}
	if !api.called("POST /api/projects/3/services") {
		t.Error("connect_project did not add any service")
	}
}

// A port already taken is normal in a monorepo, and it must not read as a
// failed connection: the other services still get set up, and the one that
// clashed is reported beside its name.
func TestAClashingPortIsReportedNotFatal(t *testing.T) {
	out := connect(t, connectFake())

	if count, _ := out["services"].(float64); count != 2 {
		t.Errorf("%v services added, want both", out["services"])
	}
	problems, ok := out["problems"].([]any)
	if !ok || len(problems) == 0 {
		t.Fatalf("the clash was not reported: %v", out)
	}
	first, _ := problems[0].(map[string]any)
	if first["service"] != "ms-admin" {
		t.Errorf("the problem is attributed to %v, want ms-admin", first["service"])
	}
}

func TestConnectProjectNeedsSomethingToRead(t *testing.T) {
	s := New(&routedAPI{routes: map[string]string{"POST /api/discover": `{"found":[]}`}}, "test")

	text, isError := callTool(t, s, "connect_project",
		`{"name":"vacio","files":[{"filename":".env","content":"NADA=1"}]}`)
	if !isError {
		t.Fatal("a project with no services was created anyway")
	}
	// The message has to say what Sonda was looking for, or the agent retries
	// with the same file.
	if !strings.Contains(text, "MS_AUTH_GRPC_URL") {
		t.Errorf("the error does not show what a service entry looks like: %s", text)
	}
}

// Without the inverse, an agent that repointed a .env and then disconnected
// leaves the environment pointing at ports nobody is listening on.
func TestDisconnectReturnsTheInversePatch(t *testing.T) {
	api := &routedAPI{routes: map[string]string{
		"GET /api/projects": `{"projects":[{"id":3,"name":"core-delpagroup","active":true,"services":[
		  {"name":"ms-auth","listen":"127.0.0.1:9152","upstream":"http://localhost:50052","protocol":"grpc"},
		  {"name":"web","listen":"127.0.0.1:9100","upstream":"http://localhost:3000","protocol":"http"}
		]}]}`,
		"POST /api/projects/deactivate": `{"projects":[]}`,
	}}

	s := New(api, "test")
	text, isError := callTool(t, s, "disconnect_project", `{}`)
	if isError {
		t.Fatalf("disconnect failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}

	changes, _ := out["changes"].(map[string]any)
	grpc, ok := changes["MS_AUTH_GRPC_URL"].(map[string]any)
	if !ok {
		t.Fatalf("the gRPC service is not in the inverse patch: %v", changes)
	}
	// Exactly the reverse of what connect handed out.
	if grpc["from"] != "127.0.0.1:9152" || grpc["to"] != "localhost:50052" {
		t.Errorf("inverse patch is %v, want Sonda's port back to the real one", grpc)
	}
	// An HTTP service gets _URL, not _GRPC_URL — the same rule discovery used
	// to read the names apart in the first place.
	if _, ok := changes["WEB_URL"]; !ok {
		t.Errorf("the HTTP service is missing or misnamed: %v", changes)
	}
	if !api.called("POST /api/projects/deactivate") {
		t.Error("nothing was actually deactivated")
	}
}

// Tools take the name a person would say. An unknown one has to list what does
// exist, or the agent guesses again.
func TestAnUnknownProjectNameSaysWhatExists(t *testing.T) {
	api := &routedAPI{routes: map[string]string{
		"GET /api/projects": `{"projects":[{"id":1,"name":"core-delpagroup"},{"id":2,"name":"relay"}]}`,
	}}
	s := New(api, "test")

	text, isError := callTool(t, s, "activate_project", `{"project":"no-existe"}`)
	if !isError {
		t.Fatal("an unknown project was accepted")
	}
	for _, want := range []string{"core-delpagroup", "relay"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error does not mention %s: %s", want, text)
		}
	}
	if api.called("POST /api/projects/1/activate") {
		t.Error("something was activated despite the name being unknown")
	}
}

// Activation answers with the whole project listing. What was asked is whether
// the ports opened, and a busy one must not read as a failed activation.
func TestActivationReportsWhatReallyOpened(t *testing.T) {
	api := &routedAPI{routes: map[string]string{
		"GET /api/projects": `{"projects":[{"id":3,"name":"core-delpagroup","active":true,"services":[]}]}`,
		"POST /api/projects/3/activate": `{"projects":[{"id":3,"name":"core-delpagroup","active":true,"services":[
		  {"name":"ms-auth","running":true},
		  {"name":"ms-admin","running":false,"error":"address already in use"}
		]}]}`,
	}}

	s := New(api, "test")
	text, isError := callTool(t, s, "activate_project", `{"project":"core-delpagroup"}`)
	if isError {
		t.Fatalf("activation failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}

	if listening, _ := out["listening"].(float64); listening != 1 {
		t.Errorf("listening = %v, want 1", out["listening"])
	}
	if of, _ := out["of"].(float64); of != 2 {
		t.Errorf("of = %v, want 2", out["of"])
	}
	failed, ok := out["did_not_open"].([]any)
	if !ok || len(failed) != 1 {
		t.Fatalf("the port that did not open is not reported: %v", out)
	}
	entry, _ := failed[0].(map[string]any)
	if entry["problem"] == "" {
		t.Error("the failure does not say why")
	}
}

// One service Sonda cannot save must not lose the other twenty. In a monorepo
// this is the ordinary case, not the edge one — and a connect that gives up on
// the first problem is a connect nobody finishes.
func TestOneUnsavableServiceDoesNotLoseTheRest(t *testing.T) {
	api := connectFake()
	api.rejectBodiesWith = `"name":"ms-admin"`

	out := connect(t, api)

	if count, _ := out["services"].(float64); count != 1 {
		t.Errorf("%v services added, want the one that could be saved", out["services"])
	}
	problems, ok := out["problems"].([]any)
	if !ok || len(problems) != 1 {
		t.Fatalf("the failure was not reported: %v", out)
	}
	first, _ := problems[0].(map[string]any)
	if first["service"] != "ms-admin" {
		t.Errorf("blamed %v, want ms-admin", first["service"])
	}
	// The one that worked still has to be in the patch, or the agent applies a
	// mapping that is missing the service it can actually observe.
	changes, _ := out["changes"].(map[string]any)
	if _, ok := changes["MS_AUTH_GRPC_URL"]; !ok {
		t.Errorf("the service that saved fine is missing from the patch: %v", changes)
	}
}

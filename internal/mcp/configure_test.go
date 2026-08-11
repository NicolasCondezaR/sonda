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
// leaves the environment pointing at ports nobody is listening on. The inverse
// has to name the variable that was really there — discovery accepts _ADDR,
// _ADDRESS, _HOST and _HTTP_URL as readily as _URL, so a name derived from the
// service instead would undo a variable the project never had while leaving the
// real one aimed at a port that just closed.
func TestConnectThenDisconnectNamesTheVariableThatWasReallyThere(t *testing.T) {
	api := &routedAPI{routes: map[string]string{
		"POST /api/discover": `{"found":[
		  {"name":"ms-auth","upstream":"http://localhost:50052","protocol":"grpc",
		   "listen":"127.0.0.1:9152","key":"MS_AUTH_ADDR","original":"localhost:50052"}
		]}`,
		"POST /api/projects": `{"id":3,"name":"core-delpagroup"}`,
		"GET /api/projects": `{"projects":[{"id":3,"name":"core-delpagroup","active":true,"services":[
		  {"id":7,"name":"ms-auth","listen":"127.0.0.1:9152","upstream":"http://localhost:50052",
		   "protocol":"grpc","env_key":"MS_AUTH_ADDR"}
		]}]}`,
		"POST /api/projects/deactivate": `{"projects":[]}`,
	}}
	s := New(api, "test")

	if text, isError := callTool(t, s, "connect_project",
		`{"name":"core-delpagroup","files":[{"filename":".env","content":"MS_AUTH_ADDR=localhost:50052"}]}`); isError {
		t.Fatalf("connect_project failed: %s", text)
	}
	// The name has to reach the service that is being created: it is the only
	// moment it is known, and anywhere else it would have to be guessed back.
	if key, _ := saved(t, api)["env_key"].(string); key != "MS_AUTH_ADDR" {
		t.Errorf("the service was saved with env_key %q, want the variable discovery read", key)
	}

	text, isError := callTool(t, s, "disconnect_project", `{}`)
	if isError {
		t.Fatalf("disconnect failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}

	changes, _ := out["changes"].(map[string]any)
	entry, ok := changes["MS_AUTH_ADDR"].(map[string]any)
	if !ok {
		t.Fatalf("the variable the project actually has is not in the inverse patch: %v", changes)
	}
	if entry["from"] != "127.0.0.1:9152" || entry["to"] != "localhost:50052" {
		t.Errorf("inverse patch is %v, want Sonda's port back to the real one", entry)
	}
	// The derived name is the bug: writing it would set something nothing reads.
	if _, invented := changes["MS_AUTH_GRPC_URL"]; invented {
		t.Errorf("a variable was invented from the service name: %v", changes)
	}
	if out["restore_by_hand"] != nil {
		t.Errorf("a service Sonda does know the variable for was handed back: %v", out["restore_by_hand"])
	}
	if !api.called("POST /api/projects/deactivate") {
		t.Error("nothing was actually deactivated")
	}
}

// A service added by hand, or read out of a compose file, never had a variable
// Sonda saw. Neither does anything at all after a restart. Naming one anyway is
// the failure this tool exists to prevent, so it says what it knows instead.
func TestDisconnectWillNotInventAVariableItNeverSaw(t *testing.T) {
	api := &routedAPI{routes: map[string]string{
		"GET /api/projects": `{"projects":[{"id":3,"name":"core-delpagroup","active":true,"services":[
		  {"name":"ms-auth","listen":"127.0.0.1:9152","upstream":"http://localhost:50052","protocol":"grpc"}
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

	if changes, _ := out["changes"].(map[string]any); len(changes) != 0 {
		t.Errorf("a variable was guessed: %v", changes)
	}
	byHand, _ := out["restore_by_hand"].([]any)
	if len(byHand) != 1 {
		t.Fatalf("the service was not reported as the agent's to restore: %v", out)
	}
	entry, _ := byHand[0].(map[string]any)
	// Saying "I do not know" is only useful with the two addresses that let the
	// agent go and find it.
	if entry["was_listening_on"] != "127.0.0.1:9152" || entry["point_back_at"] != "localhost:50052" {
		t.Errorf("the gap does not say what to search for and what to put back: %v", entry)
	}
	if problem, _ := entry["problem"].(string); problem == "" {
		t.Error("the gap does not say why Sonda cannot name the variable")
	}
	if next, _ := out["next_steps"].(string); !strings.Contains(next, "restore_by_hand") {
		t.Errorf("next_steps does not point at the gap: %q", next)
	}
}

// oneService is a project with something already in it, which is the state
// every "fix that port" call is made against.
func oneService() *routedAPI {
	return &routedAPI{routes: map[string]string{
		"GET /api/projects": `{"projects":[{"id":3,"name":"core-delpagroup","active":false,"services":[
		  {"id":7,"name":"ms-auth","listen":"127.0.0.1:9152","upstream":"http://localhost:50052","protocol":"grpc","reflection":true}
		]}]}`,
		"POST /api/projects/3/services":   `{"projects":[]}`,
		"DELETE /api/services/7":          `{"projects":[]}`,
		"POST /api/projects/3/descriptor": `{"stored":4,"services":2,"name":"descriptors.binpb"}`,
	}}
}

// saved returns the body of the last save, which is where the difference
// between changing a service and adding one actually lives.
func saved(t *testing.T, api *routedAPI) map[string]any {
	t.Helper()
	for i := len(api.calls) - 1; i >= 0; i-- {
		if !strings.Contains(api.calls[i], "/services") {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(api.bodies[i]), &body); err != nil {
			t.Fatalf("the save body is not JSON: %v", err)
		}
		return body
	}
	t.Fatal("nothing was saved")
	return nil
}

// The tool says it can change a service that is already there. Without the id
// the API inserts, the insert collides with UNIQUE(project_id, name), and every
// attempt to move a port Sonda could not take answers 400 — which is exactly
// what connect_project tells the agent to do about a busy port.
func TestConfigureServiceChangesTheServiceThatIsAlreadyThere(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	text, isError := callTool(t, s, "configure_service",
		`{"project":"core-delpagroup","name":"ms-auth","listen":"127.0.0.1:9252","upstream":"http://localhost:50052","protocol":"grpc"}`)
	if isError {
		t.Fatalf("configure_service failed: %s", text)
	}

	body := saved(t, api)
	if id, _ := body["id"].(float64); int64(id) != 7 {
		t.Errorf("saved with id %v, want 7 — without it the API inserts and the name collides", body["id"])
	}
	if body["listen"] != "127.0.0.1:9252" {
		t.Errorf("listen = %v, want the new port", body["listen"])
	}
	// Moving a port must not silently turn schema resolution off for a service
	// that had it on.
	if reflection, _ := body["reflection"].(bool); !reflection {
		t.Errorf("reflection = %v, want the setting the service already had", body["reflection"])
	}
}

// A name the project does not have is an addition, and an addition must not
// carry an id: the API would then update a service that does not exist.
func TestConfigureServiceAddsANameTheProjectDoesNotHave(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	text, isError := callTool(t, s, "configure_service",
		`{"project":"core-delpagroup","name":"ms-admin","listen":"127.0.0.1:9153","upstream":"http://localhost:50053","protocol":"grpc"}`)
	if isError {
		t.Fatalf("configure_service failed: %s", text)
	}

	body := saved(t, api)
	if _, ok := body["id"]; ok {
		t.Errorf("a new service was saved with an id: %v", body)
	}
	// The same default connect_project uses. A service that does serve
	// reflection gets it by asking for it.
	if reflection, _ := body["reflection"].(bool); reflection {
		t.Errorf("reflection = %v, want false unless asked for", body["reflection"])
	}
}

func TestConfigureServiceTakesReflectionFromTheAgent(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	if _, isError := callTool(t, s, "configure_service",
		`{"project":"core-delpagroup","name":"ms-auth","listen":"127.0.0.1:9152","upstream":"http://localhost:50052","protocol":"grpc","reflection":false}`); isError {
		t.Fatal("configure_service failed")
	}
	if reflection, _ := saved(t, api)["reflection"].(bool); reflection {
		t.Error("reflection:false was ignored, so the agent cannot turn it off")
	}
}

// Deleting is the repair path a person has in the web interface. Without it an
// agent that cannot fix a service in place has nowhere to go.
func TestRemoveServiceDeletesByNameAndSaysWhatToPutBack(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	text, isError := callTool(t, s, "remove_service", `{"project":"core-delpagroup","name":"ms-auth"}`)
	if isError {
		t.Fatalf("remove_service failed: %s", text)
	}
	if !api.called("DELETE /api/services/7") {
		t.Errorf("the service was not deleted by its id: %v", api.calls)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	// The port is gone, so whatever was aimed at it needs the real address.
	if out["point_back_at"] != "localhost:50052" {
		t.Errorf("point_back_at = %v, want the real service", out["point_back_at"])
	}
}

func TestRemoveServiceSaysWhatTheProjectDoesHave(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	text, isError := callTool(t, s, "remove_service", `{"project":"core-delpagroup","name":"ms-atuh"}`)
	if !isError {
		t.Fatal("a service that does not exist was removed anyway")
	}
	if !strings.Contains(text, "ms-auth") {
		t.Errorf("the error does not say what the project has: %s", text)
	}
	if api.called("DELETE ") {
		t.Errorf("something was deleted despite the name being wrong: %v", api.calls)
	}
}

// Without this an agent can wire up a whole gRPC system where no service serves
// reflection — the ordinary case — and every message comes back undecoded.
func TestUploadSchemasSendsTheDecodedBytes(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	// "sonda" in base64, split across lines the way a long one arrives.
	text, isError := callTool(t, s, "upload_schemas",
		`{"project":"core-delpagroup","filename":"delpa.binpb","content_base64":"c29u\nZGE="}`)
	if isError {
		t.Fatalf("upload_schemas failed: %s", text)
	}

	if !api.called("POST /api/projects/3/descriptor?name=delpa.binpb") {
		t.Errorf("the descriptor set did not reach the project: %v", api.calls)
	}
	if got := api.bodies[len(api.bodies)-1]; got != "sonda" {
		t.Errorf("the endpoint received %q, want the decoded bytes", got)
	}
}

func TestUploadSchemasRefusesSomethingThatIsNotBase64(t *testing.T) {
	api := oneService()
	s := New(api, "test")

	text, isError := callTool(t, s, "upload_schemas",
		`{"project":"core-delpagroup","filename":"delpa.binpb","content_base64":"not base64!!"}`)
	if !isError {
		t.Fatal("garbage was accepted as a descriptor set")
	}
	if !strings.Contains(text, "base64") {
		t.Errorf("the error does not say what was wrong: %s", text)
	}
	if api.called("POST /api/projects/3/descriptor") {
		t.Error("garbage was uploaded anyway")
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

package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/api"
	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

type noDrops struct{}

func (noDrops) Dropped() int64 { return 0 }

type noRecorder struct{}

func (noRecorder) Record(*store.Call) {}

// sonda builds the real thing — real SQLite file, real API — and hands back the
// MCP server in front of it. Reopening the same path is what a restart is, and
// a fake that keeps its answers in a map cannot tell the two apart.
func sonda(t *testing.T, path string) *Server {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Without listeners: this test is about what Sonda remembers, and binding
	// twenty ports to prove it would only make it flaky.
	rt := runtime.NewWithoutListeners(db, noRecorder{}, 1<<20)
	if err := rt.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return New(Local{Handler: api.New(db, noDrops{}, rt).Handler()}, "test")
}

// The workflow this exists for: connect in the morning, restart the machine,
// disconnect in the evening. The variable that pointed at each service is the
// one thing that cannot be worked out again afterwards, so it has to outlive the
// process — and a service that never had one has to stay honest about it in the
// same breath.
func TestTheUndoPatchSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonda.db")
	s := sonda(t, path)

	if text, isError := callTool(t, s, "connect_project", `{
		"name":"core-delpagroup",
		"files":[
		  {"filename":".env","content":"MS_AUTH_ADDR=localhost:50052\n"},
		  {"filename":"compose.yaml","content":"services:\n  legacy-api:\n    ports:\n      - \"3000:3000\"\n"}
		]}`); isError {
		t.Fatalf("connect_project failed: %s", text)
	}
	if text, isError := callTool(t, s, "activate_project", `{"project":"core-delpagroup"}`); isError {
		t.Fatalf("activate_project failed: %s", text)
	}

	// The restart. Everything the old process held is gone; only the file is
	// left, which is exactly the situation that used to lose the variable.
	s = sonda(t, path)

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
		t.Fatalf("the variable was forgotten across the restart: %v", out)
	}
	if entry["from"] != "127.0.0.1:9152" || entry["to"] != "localhost:50052" {
		t.Errorf("inverse patch is %v, want Sonda's port back to the real one", entry)
	}
	if _, invented := changes["MS_AUTH_GRPC_URL"]; invented {
		t.Errorf("a variable was derived from the service name: %v", changes)
	}

	// The compose service never had a variable, and a column to put one in is
	// not a reason to acquire one: it comes back as the agent's to restore.
	byHand, _ := out["restore_by_hand"].([]any)
	if len(byHand) != 1 {
		t.Fatalf("the service with no variable was not handed back: %v", out)
	}
	gap, _ := byHand[0].(map[string]any)
	if gap["service"] != "legacy-api" {
		t.Errorf("the wrong service is reported as unknown: %v", gap)
	}
	if gap["point_back_at"] != "127.0.0.1:3000" {
		t.Errorf("the gap does not say what to put back: %v", gap)
	}
}

// listed reads the project listing back through the tools, which is the only
// view an agent has of what a call actually did.
func listed(t *testing.T, s *Server) map[string]any {
	t.Helper()
	text, isError := callTool(t, s, "list_services", `{}`)
	if isError {
		t.Fatalf("list_services failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// project returns one project and its services out of that listing.
func project(t *testing.T, s *Server, name string) map[string]any {
	t.Helper()
	for _, entry := range listed(t, s)["projects"].([]any) {
		p, _ := entry.(map[string]any)
		if p["name"] == name {
			return p
		}
	}
	return nil
}

func service(t *testing.T, p map[string]any, name string) map[string]any {
	t.Helper()
	services, _ := p["services"].([]any)
	for _, entry := range services {
		svc, _ := entry.(map[string]any)
		if svc["name"] == name {
			return svc
		}
	}
	return nil
}

// Re-running is what connect_project's own answer tells the agent to do: apply
// the patch, and when something is wrong, edit the file and ask again. It used
// to answer a bare 409 with no way forward — there is no delete_project and no
// rename_project — so the agent was stuck with a project it could neither
// extend nor remove.
//
// Against the real store, because this is about identity: the fake enforces no
// unique constraint and accepts any id, so a run that would collide in SQLite
// passes there.
func TestConnectingTheSameProjectAgainAddsToItInsteadOfRefusing(t *testing.T) {
	s := sonda(t, filepath.Join(t.TempDir(), "sonda.db"))

	if text, isError := callTool(t, s, "connect_project",
		`{"name":"core-delpagroup","files":[{"filename":".env","content":"MS_AUTH_ADDR=localhost:50052\n"}]}`); isError {
		t.Fatalf("connect_project failed: %s", text)
	}
	// A setting no configuration file can express, made after connecting. It is
	// the one an update has no excuse to lose.
	if text, isError := callTool(t, s, "configure_service",
		`{"project":"core-delpagroup","name":"ms-auth","tls":true}`); isError {
		t.Fatalf("configure_service failed: %s", text)
	}

	// The second run: the same .env with one service added to it.
	text, isError := callTool(t, s, "connect_project",
		`{"name":"core-delpagroup","files":[{"filename":".env","content":"MS_AUTH_ADDR=localhost:50052\nMS_ADMIN_ADDR=localhost:50053\n"}]}`)
	if isError {
		t.Fatalf("connecting the same project again was refused: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if out["reused_existing_project"] == nil {
		t.Errorf("the answer does not say it added to a project that was already there: %v", out)
	}
	if added, _ := out["added"].(float64); added != 1 {
		t.Errorf("added = %v, want the one service the file gained", out["added"])
	}
	if updated, _ := out["updated"].(float64); updated != 1 {
		t.Errorf("updated = %v, want the one that was already there", out["updated"])
	}

	// One project, not two, and nothing about the first service undone.
	if count := len(listed(t, s)["projects"].([]any)); count != 1 {
		t.Errorf("%d projects, want the one that was asked for", count)
	}
	p := project(t, s, "core-delpagroup")
	auth := service(t, p, "ms-auth")
	if auth == nil {
		t.Fatalf("ms-auth disappeared: %v", p)
	}
	if tls, _ := auth["tls"].(bool); !tls {
		t.Errorf("connecting again turned TLS off: %v", auth)
	}
	if auth["upstream"] != "http://localhost:50052" {
		t.Errorf("upstream = %v, want the real service", auth["upstream"])
	}
	if service(t, p, "ms-admin") == nil {
		t.Errorf("the service the file gained was not added: %v", p)
	}
}

// A run where nothing could be saved used to leave the project row behind,
// created before the first service was tried and never rolled back — an empty
// project the agent has no tool to remove.
func TestAConnectThatSavesNothingLeavesNoProjectBehind(t *testing.T) {
	s := sonda(t, filepath.Join(t.TempDir(), "sonda.db"))

	// The suggestion for port 9152 is 127.0.0.1:9152, so this one entry asks
	// Sonda to forward a port to itself, which the store refuses.
	text, isError := callTool(t, s, "connect_project",
		`{"name":"sin-suerte","files":[{"filename":".env","content":"MS_X_URL=127.0.0.1:9152\n"}]}`)
	if !isError {
		t.Fatalf("a service that cannot exist was accepted: %s", text)
	}
	if project(t, s, "sin-suerte") != nil {
		t.Error("the failed run left an empty project behind")
	}
}

// Only the last two digits of the real port reach the suggestion, and each file
// is read on its own — so two services in two files were handed one address,
// both were created, and which of them got the port was map iteration order.
func TestTwoFilesAreNotGivenOneListenAddress(t *testing.T) {
	s := sonda(t, filepath.Join(t.TempDir(), "sonda.db"))

	text, isError := callTool(t, s, "connect_project", `{"name":"dos-archivos","files":[
	  {"filename":".env","content":"MS_A_URL=localhost:3001\n"},
	  {"filename":"otro.env","content":"MS_B_URL=localhost:50001\n"}
	]}`)
	if isError {
		t.Fatalf("connect_project failed: %s", text)
	}

	p := project(t, s, "dos-archivos")
	services, _ := p["services"].([]any)
	if len(services) != 2 {
		t.Fatalf("%d services saved, want both: %v", len(services), p)
	}
	first, _ := services[0].(map[string]any)
	second, _ := services[1].(map[string]any)
	if first["listen"] == second["listen"] {
		t.Errorf("both services were given %v", first["listen"])
	}
}

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

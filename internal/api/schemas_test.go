package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// A service that serves no reflection must not be reported as serving it.
//
// The schema report exists to answer "field names are missing, and is that
// reflection being off, a stale descriptor set, or the service being down?" —
// so an answer of "reflection" for a service that has none sends the reader
// looking in the one place the problem is not.
//
// It said that for every gRPC service, because the report was built from a
// hand-written struct literal that never carried Reflection. The field is a
// *bool that reads as "on" when nil, so the omission did not read as missing
// data; it read as a confident yes.
func TestAServiceWithoutReflectionIsNotReportedAsHavingIt(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	p, err := db.CreateProject(ctx, "grpc-without-reflection")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveService(ctx, store.Service{
		ProjectID: p.ID, Name: "pricing", Listen: "127.0.0.1:0",
		Upstream: "http://127.0.0.1:65001", Protocol: "grpc",
		Reflection: false,
	}); err != nil {
		t.Fatal(err)
	}
	// The descriptor set belongs to the project, not to the service, which is
	// why only a conversion that can see the project can report it.
	if err := db.SetDescriptorSet(ctx, p.ID, "shop.binpb", []byte{0x00}); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	rt := runtime.New(db, noRecorder{}, 1<<20).WithCADir(dir)
	if err := rt.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	h := New(db, noDrops{}, rt).Handler()

	code, body := get(t, h, "/api/schemas")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	list, _ := body["schemas"].([]any)
	if len(list) != 1 {
		t.Fatalf("%d services reported, want the one gRPC service", len(list))
	}
	entry, _ := list[0].(map[string]any)
	if on, _ := entry["reflection"].(bool); on {
		t.Error("a service configured without reflection is reported as serving it")
	}
	if name, _ := entry["descriptor_set"].(string); name == "" {
		t.Error("the descriptor set the project carries is not named in the report")
	}
}

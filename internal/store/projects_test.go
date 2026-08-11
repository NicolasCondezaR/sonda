package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// The variable a service was pointed by is the one thing that cannot be
// re-derived later: MS_AUTH_ADDR and MS_AUTH_URL are both valid and only the
// file knew which. Connecting a project in the morning and disconnecting it in
// the evening is the ordinary workflow, and a machine reboots in between, so a
// key that only lived in memory turned every service into one to restore by
// hand.
func TestTheVariableAServiceWasPointedBySurvivesARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateProject(ctx, "core-delpagroup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveService(ctx, Service{
		ProjectID: project.ID, Name: "ms-auth", Listen: "127.0.0.1:9152",
		Upstream: "http://localhost:50052", Protocol: "grpc", EnvKey: "MS_AUTH_ADDR",
	}); err != nil {
		t.Fatal(err)
	}
	// The restart, taken as a real one: the file is closed and opened again
	// rather than the in-process state being cleared.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	svc := onlyService(t, s, project.ID)
	if svc.EnvKey != "MS_AUTH_ADDR" {
		t.Errorf("env key = %q after reopening, want MS_AUTH_ADDR", svc.EnvKey)
	}
}

// A service read out of a compose file, or typed in by hand, never had a
// variable. Having somewhere to put one must not produce one: an empty key is
// the honest answer that makes disconnect_project hand the service back instead
// of writing a name nothing reads.
func TestAServiceWithNoVariableNeverAcquiresOne(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	project, err := s.CreateProject(ctx, "compose-only")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.SaveService(ctx, Service{
		ProjectID: project.ID, Name: "api", Listen: "127.0.0.1:9100",
		Upstream: "http://127.0.0.1:3000", Protocol: "http",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := onlyService(t, s, project.ID).EnvKey; got != "" {
		t.Fatalf("a service that never had a variable was stored with %q", got)
	}

	// Moving a busy port is the repair connect_project tells the agent to make,
	// and it must not invent a key on the way through either.
	if _, err := s.SaveService(ctx, Service{
		ID: id, ProjectID: project.ID, Name: "api", Listen: "127.0.0.1:9200",
		Upstream: "http://127.0.0.1:3000", Protocol: "http",
	}); err != nil {
		t.Fatal(err)
	}
	if got := onlyService(t, s, project.ID).EnvKey; got != "" {
		t.Errorf("moving the port gave the service a variable out of nowhere: %q", got)
	}
}

// Reconnecting reads the file again, so a variable that was renamed there wins.
// A save that carries none — the web form, or configure_service moving a port —
// is not evidence that the variable vanished, and erasing it would put the
// service back into restore-by-hand.
func TestSavingRefreshesTheVariableAndAnEmptyOneKeepsIt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	project, err := s.CreateProject(ctx, "core-delpagroup")
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{
		ProjectID: project.ID, Name: "ms-auth", Listen: "127.0.0.1:9152",
		Upstream: "http://localhost:50052", Protocol: "grpc", EnvKey: "MS_AUTH_ADDR",
	}
	id, err := s.SaveService(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	svc.ID = id

	renamed := svc
	renamed.EnvKey = "MS_AUTH_GRPC_URL"
	if _, err := s.SaveService(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if got := onlyService(t, s, project.ID).EnvKey; got != "MS_AUTH_GRPC_URL" {
		t.Errorf("env key = %q, want the name the file has today", got)
	}

	silent := svc
	silent.EnvKey = ""
	silent.Listen = "127.0.0.1:9252"
	if _, err := s.SaveService(ctx, silent); err != nil {
		t.Fatal(err)
	}
	if got := onlyService(t, s, project.ID).EnvKey; got != "MS_AUTH_GRPC_URL" {
		t.Errorf("env key = %q after a save that did not carry one, want it kept", got)
	}
}

// An existing sonda.db has services written before the column existed. They come
// back as "Sonda does not know", which is the truth about them, and everything
// else about them survives the upgrade.
func TestAnOlderDatabaseUpgradesWithoutAKeyAndWithoutLosingAnything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "older.db")

	// The services table as an earlier version created it: no env_key at all.
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE,
			active INTEGER NOT NULL DEFAULT 0, descriptor_set BLOB,
			descriptor_name TEXT NOT NULL DEFAULT '',
			descriptor_updated INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
		CREATE TABLE services (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL, name TEXT NOT NULL, listen TEXT NOT NULL,
			upstream TEXT NOT NULL, protocol TEXT NOT NULL,
			reflection INTEGER NOT NULL DEFAULT 1, position INTEGER NOT NULL DEFAULT 0,
			UNIQUE(project_id, name));
		INSERT INTO projects (id, name, created_at) VALUES (1, 'legacy', 0);
		INSERT INTO services (project_id, name, listen, upstream, protocol)
		VALUES (1, 'ms-auth', '127.0.0.1:9152', 'http://localhost:50052', 'grpc');`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("an existing database did not upgrade: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	svc := onlyService(t, s, 1)
	if svc.EnvKey != "" {
		t.Errorf("the migration invented %q for a row that never had one", svc.EnvKey)
	}
	if svc.Name != "ms-auth" || svc.Listen != "127.0.0.1:9152" || svc.Upstream != "http://localhost:50052" {
		t.Errorf("the upgrade lost part of the service: %+v", svc)
	}
}

func onlyService(t *testing.T, s *Store, projectID int64) Service {
	t.Helper()
	services, err := s.services(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 {
		t.Fatalf("%d services, want 1", len(services))
	}
	return services[0]
}

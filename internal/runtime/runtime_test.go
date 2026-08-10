package runtime

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// These tests cover the seam, not the pieces. The store is tested against its
// own schema and the supervisor against its own sockets; what neither of them
// can catch is a Reconcile that reads the configuration correctly and then
// opens the wrong ports, crosses two services, or leaves a port from the
// previous project listening. That only shows up when both halves run together,
// which until now happened solely by hand in a browser.

func openStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// upstream answers with a fixed body, so a test can tell *which* service a port
// reached. A check that only asserts "something answered" passes just as
// happily when two services are wired to each other's upstream.
func upstream(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type svc struct {
	name     string
	listen   string
	upstream string
	protocol string
}

func project(t *testing.T, db *store.Store, name string, services ...svc) int64 {
	t.Helper()
	ctx := context.Background()
	p, err := db.CreateProject(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range services {
		protocol := s.protocol
		if protocol == "" {
			protocol = config.ProtocolHTTP
		}
		if _, err := db.SaveService(ctx, store.Service{
			ProjectID: p.ID,
			Name:      s.name,
			Listen:    s.listen,
			Upstream:  s.upstream,
			Protocol:  protocol,
			Position:  i,
		}); err != nil {
			t.Fatalf("save %s: %v", s.name, err)
		}
	}
	return p.ID
}

func get(t *testing.T, addr string) (string, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// recorder collects captures off the proxy goroutine. The proxy records after
// it has finished writing the response, so a client can hold the body before
// Record runs — reading a field directly would be a race, and a flaky one.
type recorder struct{ calls chan *store.Call }

func newRecorder() *recorder { return &recorder{calls: make(chan *store.Call, 16)} }

func (r *recorder) Record(c *store.Call) {
	select {
	case r.calls <- c:
	default:
	}
}

func (r *recorder) wait(t *testing.T) *store.Call {
	t.Helper()
	select {
	case c := <-r.calls:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no capture arrived")
		return nil
	}
}

func reconcile(t *testing.T, rt *Runtime) {
	t.Helper()
	if err := rt.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func activate(t *testing.T, db *store.Store, id int64) {
	t.Helper()
	if err := db.ActivateProject(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestActivatingAProjectOpensItsPorts(t *testing.T) {
	db := openStore(t)
	auth, admin := freePort(t), freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "ms-auth", listen: auth, upstream: upstream(t, "auth")},
		svc{name: "ms-admin", listen: admin, upstream: upstream(t, "admin")},
	)

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	// A project that exists but is not active must not be listening. A fresh
	// install reconciles before anything is activated.
	reconcile(t, rt)
	if _, err := get(t, auth); err == nil {
		t.Error("a port is open before any project was activated")
	}
	if rt.Active() != nil {
		t.Error("a project is reported active when none is")
	}

	activate(t, db, id)
	reconcile(t, rt)

	// Each port must reach its own upstream. Opening the right number of ports
	// but crossing them is worse than opening none.
	if body, err := get(t, auth); err != nil || body != "auth" {
		t.Errorf("ms-auth answered %q, %v", body, err)
	}
	if body, err := get(t, admin); err != nil || body != "admin" {
		t.Errorf("ms-admin answered %q, %v", body, err)
	}
	if rt.ActiveName() != "monorepo" {
		t.Errorf("active project is %q, want monorepo", rt.ActiveName())
	}
	if got := len(rt.Status()); got != 2 {
		t.Errorf("status reports %d listeners, want 2", got)
	}
}

// Switching projects is the operation the whole grouping exists for, and the
// dangerous half is the closing one: a port left open from the previous project
// keeps accepting traffic and forwarding it to the wrong system, silently.
func TestSwitchingProjectsClosesTheOldPorts(t *testing.T) {
	db := openStore(t)
	oldPort, newPort := freePort(t), freePort(t)

	first := project(t, db, "primero",
		svc{name: "api", listen: oldPort, upstream: upstream(t, "primero")})
	second := project(t, db, "segundo",
		svc{name: "api", listen: newPort, upstream: upstream(t, "segundo")})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, first)
	reconcile(t, rt)
	if body, _ := get(t, oldPort); body != "primero" {
		t.Fatalf("the first project did not come up: %q", body)
	}

	activate(t, db, second)
	reconcile(t, rt)

	if _, err := get(t, oldPort); err == nil {
		t.Error("the previous project's port is still answering")
	}
	if body, err := get(t, newPort); err != nil || body != "segundo" {
		t.Errorf("the new project answered %q, %v", body, err)
	}
	if rt.ActiveName() != "segundo" {
		t.Errorf("active project is %q, want segundo", rt.ActiveName())
	}
}

// Two projects may claim the same port — only one listens at a time, and that
// is precisely why they are switchable. The rebind has to survive the handover.
func TestTwoProjectsCanShareAPortAcrossASwitch(t *testing.T) {
	db := openStore(t)
	shared := freePort(t)

	first := project(t, db, "primero",
		svc{name: "api", listen: shared, upstream: upstream(t, "primero")})
	second := project(t, db, "segundo",
		svc{name: "api", listen: shared, upstream: upstream(t, "segundo")})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, first)
	reconcile(t, rt)
	if body, _ := get(t, shared); body != "primero" {
		t.Fatalf("the first project did not come up: %q", body)
	}

	activate(t, db, second)
	reconcile(t, rt)

	// If the socket were not released before the rebind, this is where it would
	// fail with "address already in use" — and only for a user switching fast.
	for _, st := range rt.Status() {
		if !st.Running {
			t.Fatalf("rebinding %s failed: %s", st.Listen, st.Error)
		}
	}
	if body, err := get(t, shared); err != nil || body != "segundo" {
		t.Errorf("after the switch the shared port answered %q, %v", body, err)
	}
}

func TestDeactivatingClosesEverything(t *testing.T) {
	db := openStore(t)
	one, two := freePort(t), freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "a", listen: one, upstream: upstream(t, "a")},
		svc{name: "b", listen: two, upstream: upstream(t, "b")},
	)

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)
	if _, err := get(t, one); err != nil {
		t.Fatalf("the project did not come up: %v", err)
	}

	if err := db.DeactivateProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	reconcile(t, rt)

	for _, addr := range []string{one, two} {
		if _, err := get(t, addr); err == nil {
			t.Errorf("%s is still answering after deactivating", addr)
		}
	}
	if rt.Active() != nil {
		t.Error("a project is still reported active")
	}
	if rt.ActiveName() != "" {
		t.Errorf("the capture tag is still %q", rt.ActiveName())
	}
	if got := len(rt.Resolvers()); got != 0 {
		t.Errorf("%d resolvers survived deactivation", got)
	}
}

// One service on a port something else already holds must not take the rest of
// the project down with it. In a twenty-one service monorepo this is the normal
// case, not the edge one.
func TestOneBusyPortDoesNotStopTheRest(t *testing.T) {
	db := openStore(t)

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	healthy := freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "ocupado", listen: taken.Addr().String(), upstream: upstream(t, "nunca")},
		svc{name: "sano", listen: healthy, upstream: upstream(t, "sano")},
	)

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	// A busy port is reported, never returned as an error: one clash must not
	// look like a failed activation.
	reconcile(t, rt)

	if body, err := get(t, healthy); err != nil || body != "sano" {
		t.Errorf("the healthy service did not answer: %q %v", body, err)
	}

	var failed int
	for _, st := range rt.Status() {
		if !st.Running {
			failed++
			if st.Error == "" {
				t.Error("a failed listener does not say why")
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d listeners reported as failed, want 1", failed)
	}
}

// Without the tag, switching projects pours one system's traffic into
// another's field.
func TestCapturesAreTaggedWithTheActiveProject(t *testing.T) {
	db := openStore(t)
	rec := newRecorder()
	addr := freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "api", listen: addr, upstream: upstream(t, "hola")})

	rt := New(db, rec, 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)

	if _, err := get(t, addr); err != nil {
		t.Fatal(err)
	}
	call := rec.wait(t)
	if call.Project != "monorepo" {
		t.Errorf("capture tagged %q, want monorepo", call.Project)
	}
	if call.Target != "api" {
		t.Errorf("capture target is %q, want api", call.Target)
	}
}

// The tag has to follow the switch. A capture taken after switching but stamped
// with the previous project is the exact mix-up the field is there to prevent,
// and it survives a naive implementation that stamps once at startup.
func TestTheTagFollowsTheSwitch(t *testing.T) {
	db := openStore(t)
	rec := newRecorder()
	addr := freePort(t)

	first := project(t, db, "primero",
		svc{name: "api", listen: addr, upstream: upstream(t, "x")})
	second := project(t, db, "segundo",
		svc{name: "api", listen: addr, upstream: upstream(t, "x")})

	rt := New(db, rec, 1<<20)
	defer rt.Stop()

	activate(t, db, first)
	reconcile(t, rt)
	if _, err := get(t, addr); err != nil {
		t.Fatal(err)
	}
	if got := rec.wait(t).Project; got != "primero" {
		t.Fatalf("capture tagged %q, want primero", got)
	}

	activate(t, db, second)
	reconcile(t, rt)
	if _, err := get(t, addr); err != nil {
		t.Fatal(err)
	}
	if got := rec.wait(t).Project; got != "segundo" {
		t.Errorf("after switching, capture tagged %q, want segundo", got)
	}
}

// Reconcile runs after every single mutation — saving a service, uploading a
// descriptor set, renaming the project. Running it again with nothing changed
// must be a no-op, not a restart of everything that happens to work.
func TestReconcileIsIdempotent(t *testing.T) {
	db := openStore(t)
	addr := freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "api", listen: addr, upstream: upstream(t, "hola")})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	for i := 0; i < 3; i++ {
		reconcile(t, rt)
		if body, err := get(t, addr); err != nil || body != "hola" {
			t.Fatalf("reconcile %d left the port answering %q, %v", i+1, body, err)
		}
	}
	if got := len(rt.Status()); got != 1 {
		t.Errorf("status reports %d listeners after three reconciles, want 1", got)
	}
}

// Moving a service to another port has to free the old one. A listening socket
// cannot be rebound, so this is a stop and a start — and forgetting the stop
// leaves a port that still accepts traffic nobody is looking at.
func TestMovingAServiceReleasesTheOldPort(t *testing.T) {
	db := openStore(t)
	from, to := freePort(t), freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "api", listen: from, upstream: upstream(t, "hola")})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)
	if _, err := get(t, from); err != nil {
		t.Fatal(err)
	}

	active, err := db.ActiveProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	moved := active.Services[0]
	moved.Listen = to
	if _, err := db.SaveService(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	reconcile(t, rt)

	if _, err := get(t, from); err == nil {
		t.Error("the old port is still answering")
	}
	if body, err := get(t, to); err != nil || body != "hola" {
		t.Errorf("the new port answered %q, %v", body, err)
	}
}

// Renaming a service keys off its id, so the port must not go down. A rename
// that drops live connections turns an edit in a form into a broken request in
// whatever was running.
func TestRenamingAServiceKeepsItsPortOpen(t *testing.T) {
	db := openStore(t)
	addr := freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "viejo", listen: addr, upstream: upstream(t, "hola")})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)

	active, err := db.ActiveProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	renamed := active.Services[0]
	renamed.Name = "nuevo"
	if _, err := db.SaveService(context.Background(), renamed); err != nil {
		t.Fatal(err)
	}
	reconcile(t, rt)

	if body, err := get(t, addr); err != nil || body != "hola" {
		t.Errorf("after the rename the port answered %q, %v", body, err)
	}
	// The capture tag follows the new name, or the field would still show the
	// old one after the edit.
	rec := newRecorder()
	rt2 := New(db, rec, 1<<20)
	defer rt2.Stop()
	rt.Stop()
	reconcile(t, rt2)
	if _, err := get(t, addr); err != nil {
		t.Fatal(err)
	}
	if got := rec.wait(t).Target; got != "nuevo" {
		t.Errorf("capture target is %q, want nuevo", got)
	}
}

// The descriptor set belongs to the project, but only gRPC services can use it.
// Building a resolver for an HTTP service would be harmless and misleading:
// the interface reads this map to say where a schema came from.
func TestOnlyGRPCServicesGetResolvers(t *testing.T) {
	db := openStore(t)
	id := project(t, db, "monorepo",
		svc{name: "web", listen: freePort(t), upstream: upstream(t, "web"),
			protocol: config.ProtocolHTTP},
		svc{name: "ms-auth", listen: freePort(t), upstream: upstream(t, "auth"),
			protocol: config.ProtocolGRPC},
	)

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)

	resolvers := rt.Resolvers()
	if _, ok := resolvers["ms-auth"]; !ok {
		t.Error("the gRPC service has no resolver")
	}
	if _, ok := resolvers["web"]; ok {
		t.Error("an HTTP service was given a protobuf resolver")
	}
	if len(resolvers) != 1 {
		t.Errorf("%d resolvers built, want 1", len(resolvers))
	}
}

// Uploading a descriptor set has to reach the running services. It arrives
// through the same Reconcile as everything else, and the resolvers are rebuilt
// rather than patched, so this checks the rebuild actually happens.
func TestUploadingADescriptorSetRebuildsTheResolvers(t *testing.T) {
	db := openStore(t)
	id := project(t, db, "monorepo",
		svc{name: "ms-auth", listen: freePort(t), upstream: upstream(t, "auth"),
			protocol: config.ProtocolGRPC})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)

	before := rt.Resolvers()["ms-auth"]
	if before == nil {
		t.Fatal("no resolver for the gRPC service")
	}

	// The bytes do not have to be a valid descriptor set here: the API parses
	// before storing, and what is under test is that a change propagates.
	if err := db.SetDescriptorSet(context.Background(), id, "d.binpb", []byte("nuevo")); err != nil {
		t.Fatal(err)
	}
	reconcile(t, rt)

	if after := rt.Resolvers()["ms-auth"]; after == before {
		t.Error("the resolver was not rebuilt after the descriptor set changed")
	}
	if got := rt.Active().DescriptorName; got != "d.binpb" {
		t.Errorf("the active project reports descriptor %q", got)
	}
}

// Deleting the active project is a legitimate way to stop everything, and it
// leaves the store with no active row at all — a different path from
// deactivating, and one that must not panic on a nil project.
func TestDeletingTheActiveProjectClosesItsPorts(t *testing.T) {
	db := openStore(t)
	addr := freePort(t)
	id := project(t, db, "monorepo",
		svc{name: "api", listen: addr, upstream: upstream(t, "hola")})

	rt := New(db, newRecorder(), 1<<20)
	defer rt.Stop()

	activate(t, db, id)
	reconcile(t, rt)
	if _, err := get(t, addr); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteProject(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	reconcile(t, rt)

	if _, err := get(t, addr); err == nil {
		t.Error("the port survived deleting the project that owned it")
	}
	if rt.Active() != nil {
		t.Error("a deleted project is still reported active")
	}
}

// A Postgres service is the only one that reaches the supervisor as a raw
// connection handler rather than an HTTP one. Reconcile choosing the wrong kind
// would open a port that answers nothing, and nothing else in the stack can
// catch it: the store validates the row and the supervisor serves whatever it
// is handed.
func TestAPostgresServiceGetsARawListener(t *testing.T) {
	db := openStore(t)

	// A stand-in database that says who it is, so the test proves the port
	// reached this service and not merely that something answered.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(conn, "the database")
			conn.Close()
		}
	}()

	listen := freePort(t)
	id := project(t, db, "shop", svc{
		name:     "orders-db",
		listen:   listen,
		upstream: "postgres://" + ln.Addr().String(),
		protocol: config.ProtocolPostgres,
	})
	activate(t, db, id)

	rec := newRecorder()
	rt := New(db, rec, 1<<20)
	defer rt.Stop()
	reconcile(t, rt)

	conn, err := net.DialTimeout("tcp", listen, 2*time.Second)
	if err != nil {
		t.Fatalf("nothing is listening on the postgres port: %v", err)
	}
	// A deadline, not patience: an HTTP listener on this port accepts and then
	// waits for a request that is never coming.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(got) != "the database" {
		t.Fatalf("read %q, %v", got, err)
	}

	call := rec.wait(t)
	if call.Protocol != config.ProtocolPostgres {
		t.Errorf("protocol = %q", call.Protocol)
	}
	if call.Target != "orders-db" {
		t.Errorf("target = %q", call.Target)
	}
}

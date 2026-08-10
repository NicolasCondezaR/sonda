package supervisor

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func echo(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body)
	})
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

// dial reads whatever a raw listener greets with. The deadline is not patience:
// a port wired to the wrong kind accepts and then says nothing.
func dial(t *testing.T, addr string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read from %s: %v", addr, err)
	}
	return string(got)
}

func TestApplyStartsAndStops(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("first")}})

	body, err := get(t, addr)
	if err != nil || body != "first" {
		t.Fatalf("listener did not answer: %q %v", body, err)
	}

	// Removed from the desired set, so the port has to close.
	s.Apply(nil)
	if _, err := get(t, addr); err == nil {
		t.Error("the port is still answering after being removed")
	}
}

// Switching projects rebinds ports within milliseconds. If Shutdown returns
// before the socket is released, the next Apply fails with "address already in
// use" — and it would fail exactly when the user switches, never in a test that
// waits.
func TestPortIsFreeImmediatelyAfterStopping(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("first")}})
	if _, err := get(t, addr); err != nil {
		t.Fatal(err)
	}

	// The same address, a different listener: exactly what switching projects
	// does when two projects observe the same service.
	status := s.Apply([]Desired{{Key: "b", Listen: addr, Handler: echo("second")}})
	for _, st := range status {
		if !st.Running {
			t.Fatalf("rebinding %s failed: %s", st.Listen, st.Error)
		}
	}

	body, err := get(t, addr)
	if err != nil || body != "second" {
		t.Errorf("after rebinding got %q, %v", body, err)
	}
}

// One busy port must not stop the rest of the project from being observed.
func TestOneBusyPortDoesNotStopTheOthers(t *testing.T) {
	s := New()
	defer s.StopAll()

	// Held by something else entirely.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	good := freePort(t)
	status := s.Apply([]Desired{
		{Key: "busy", Listen: taken.Addr().String(), Handler: echo("never")},
		{Key: "good", Listen: good, Handler: echo("fine")},
	})

	byKey := map[string]Status{}
	for _, st := range status {
		byKey[st.Key] = st
	}
	if byKey["busy"].Running {
		t.Error("a port already taken was reported as running")
	}
	if byKey["busy"].Error == "" {
		t.Error("the failure should say why, not just that it failed")
	}
	if !byKey["good"].Running {
		t.Fatal("the healthy service did not start")
	}
	if body, err := get(t, good); err != nil || body != "fine" {
		t.Errorf("the healthy service did not answer: %q %v", body, err)
	}
}

// A listener that keeps its key and address is left alone. Restarting it would
// drop live connections every time an unrelated service is edited.
func TestUnchangedListenersAreNotRestarted(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("first")}})

	// A second service appears; the first must not be touched.
	other := freePort(t)
	s.Apply([]Desired{
		{Key: "a", Listen: addr, Handler: echo("first")},
		{Key: "b", Listen: other, Handler: echo("second")},
	})

	if body, _ := get(t, addr); body != "first" {
		t.Errorf("the untouched listener changed: %q", body)
	}
	if body, _ := get(t, other); body != "second" {
		t.Errorf("the new listener did not start: %q", body)
	}
}

// Editing a service's upstream without moving its port is the ordinary edit,
// and the one where a stale listener lies: the port stays open, every interface
// reports the new target, and the traffic keeps going to the old one.
func TestEditingAListenerWithoutMovingItsPort(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("old upstream")}})
	if body, err := get(t, addr); err != nil || body != "old upstream" {
		t.Fatalf("the listener did not come up: %q %v", body, err)
	}

	// Same key, same address, rebuilt handler: the port must not be rebound and
	// the traffic must reach the new target.
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("new upstream")}})
	if body, err := get(t, addr); err != nil || body != "new upstream" {
		t.Errorf("after the edit the port answered %q, %v", body, err)
	}
}

// The raw path inherits the same semantics, so it inherits the same lie.
func TestEditingARawListenerWithoutMovingItsPort(t *testing.T) {
	s := New()
	defer s.StopAll()

	greet := func(body string) func(net.Conn) {
		return func(c net.Conn) { defer c.Close(); io.WriteString(c, body) }
	}

	addr := freePort(t)
	s.Apply([]Desired{{Key: "db", Listen: addr, Serve: greet("old database")}})
	if got := dial(t, addr); got != "old database" {
		t.Fatalf("the raw listener did not come up: %q", got)
	}

	s.Apply([]Desired{{Key: "db", Listen: addr, Serve: greet("new database")}})
	if got := dial(t, addr); got != "new database" {
		t.Errorf("after the edit the raw port answered %q", got)
	}
}

// Changing a service's protocol keeps its key and may keep its port, but an
// http.Server cannot be swapped into speaking framed bytes. This one has to be
// a restart, or the port answers nothing while reporting itself healthy.
func TestSwitchingBetweenHTTPAndRawOnTheSamePort(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("http")}})
	if body, err := get(t, addr); err != nil || body != "http" {
		t.Fatalf("http listener did not come up: %q %v", body, err)
	}

	s.Apply([]Desired{{Key: "a", Listen: addr, Serve: func(c net.Conn) {
		defer c.Close()
		io.WriteString(c, "raw")
	}}})
	if got := dial(t, addr); got != "raw" {
		t.Errorf("after switching to raw the port answered %q", got)
	}

	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("http again")}})
	if body, err := get(t, addr); err != nil || body != "http again" {
		t.Errorf("after switching back the port answered %q, %v", body, err)
	}
}

// Reconcile runs while the proxy is serving — saving a service does not pause
// traffic — so the swap and the requests genuinely overlap.
func TestSwappingWhileRequestsAreInFlight(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("v0")}})

	stop := make(chan struct{})
	var callers sync.WaitGroup
	for i := 0; i < 4; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				body, err := get(t, addr)
				if err != nil {
					continue // a rebind of the socket is not what is under test
				}
				if !strings.HasPrefix(body, "v") {
					// t.Error is safe from a goroutine; t.Fatal is not.
					t.Errorf("a request read a torn handler: %q", body)
					return
				}
			}
		}()
	}

	for i := 1; i <= 50; i++ {
		s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo(fmt.Sprintf("v%d", i))}})
	}
	close(stop)
	callers.Wait()

	if body, err := get(t, addr); err != nil || body != "v50" {
		t.Errorf("after the last swap the port answered %q, %v", body, err)
	}
}

func TestMovingAServiceToAnotherPort(t *testing.T) {
	s := New()
	defer s.StopAll()

	from, to := freePort(t), freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: from, Handler: echo("x")}})
	s.Apply([]Desired{{Key: "a", Listen: to, Handler: echo("x")}})

	if _, err := get(t, from); err == nil {
		t.Error("the old port is still answering")
	}
	if body, err := get(t, to); err != nil || body != "x" {
		t.Errorf("the new port did not answer: %q %v", body, err)
	}
}

func TestStopAllClosesEverything(t *testing.T) {
	s := New()
	addrs := []string{freePort(t), freePort(t), freePort(t)}

	desired := make([]Desired, 0, len(addrs))
	for i, addr := range addrs {
		desired = append(desired, Desired{Key: fmt.Sprint(i), Listen: addr, Handler: echo("x")})
	}
	s.Apply(desired)
	s.StopAll()

	for _, addr := range addrs {
		if _, err := get(t, addr); err == nil {
			t.Errorf("%s is still answering after StopAll", addr)
		}
	}
	if len(s.Status()) != 0 {
		t.Error("the supervisor still reports listeners after StopAll")
	}
}

func TestProbeReportsAvailability(t *testing.T) {
	free := freePort(t)
	if err := Probe(free); err != nil {
		t.Errorf("a free port was reported as taken: %v", err)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := Probe(taken.Addr().String()); err == nil {
		t.Error("a taken port was reported as available")
	}
}

// A raw listener is the second kind this package carries, and it has to obey
// the same two promises as the first: the reported state is what is really
// listening, and a stopped port is genuinely free afterwards.
func TestARawListenerServesAndReleasesItsPort(t *testing.T) {
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	status := s.Apply([]Desired{{
		Key:    "db",
		Listen: addr,
		Serve: func(c net.Conn) {
			defer c.Close()
			io.WriteString(c, "postgres")
		},
	}})
	if len(status) != 1 || !status[0].Running {
		t.Fatalf("status = %+v", status)
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// A deadline, not patience: a listener wired to the wrong kind accepts the
	// connection and then says nothing, and a test that waits it out reports
	// the regression thirty seconds late.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(got) != "postgres" {
		t.Fatalf("read %q, %v", got, err)
	}

	// Dropped from the desired set, the port must be rebindable at once: the
	// next Apply of another project may want it.
	s.Apply(nil)
	if err := Probe(addr); err != nil {
		t.Errorf("the port was not released: %v", err)
	}
}

// The two kinds sit side by side in one project, and one must not disturb the
// other.
func TestBothKindsOfListenerRunTogether(t *testing.T) {
	s := New()
	defer s.StopAll()

	web, db := freePort(t), freePort(t)
	s.Apply([]Desired{
		{Key: "web", Listen: web, Handler: echo("http")},
		{Key: "db", Listen: db, Serve: func(c net.Conn) { defer c.Close(); io.WriteString(c, "raw") }},
	})

	if body, err := get(t, web); err != nil || body != "http" {
		t.Errorf("http listener: %q, %v", body, err)
	}
	conn, err := net.DialTimeout("tcp", db, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, _ := io.ReadAll(conn)
	conn.Close()
	if string(got) != "raw" {
		t.Errorf("raw listener: %q", got)
	}
}

package supervisor

import (
	"fmt"
	"io"
	"net"
	"net/http"
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

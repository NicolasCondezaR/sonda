package stub

import (
	"sync"
	"testing"
)

// The default has to be off. A registry that stubs anything nobody asked for
// would be the worst possible bug in this package: every service quietly
// answering from last week.
func TestNothingIsStubbedByDefault(t *testing.T) {
	r := New(nil)
	if r.On("ms-rates") {
		t.Error("a fresh registry is already stubbing")
	}
	if len(r.Active()) != 0 {
		t.Errorf("a fresh registry lists %v as stubbed", r.Active())
	}
}

func TestTurningOneOnLeavesTheRestAlone(t *testing.T) {
	r := New(nil)
	r.Set("ms-rates", true)

	if !r.On("ms-rates") {
		t.Error("the service that was turned on is not stubbed")
	}
	if r.On("ms-auth") {
		t.Error("turning one service on stubbed another")
	}
	if got := r.Active(); len(got) != 1 || got[0] != "ms-rates" {
		t.Errorf("Active() = %v, want just ms-rates", got)
	}
}

func TestTurningOffReturnsToLiveTraffic(t *testing.T) {
	r := New(nil)
	r.Set("ms-rates", true)
	r.Set("ms-rates", false)

	if r.On("ms-rates") {
		t.Error("the service is still stubbed after being turned off")
	}
	if len(r.Active()) != 0 {
		t.Errorf("Active() = %v, want nothing", r.Active())
	}
}

// The one button that undoes a state you may not remember setting.
func TestClearPutsEverythingBack(t *testing.T) {
	r := New(nil)
	for _, s := range []string{"ms-rates", "ms-auth", "ms-seo"} {
		r.Set(s, true)
	}
	r.Clear()

	if len(r.Active()) != 0 {
		t.Errorf("Active() = %v after Clear", r.Active())
	}
	for _, s := range []string{"ms-rates", "ms-auth", "ms-seo"} {
		if r.On(s) {
			t.Errorf("%s survived Clear", s)
		}
	}
}

// Sorted, so the interface and the agent get a stable answer instead of Go's
// randomised map order.
func TestActiveIsSorted(t *testing.T) {
	r := New(nil)
	for _, s := range []string{"ms-seo", "ms-auth", "ms-rates"} {
		r.Set(s, true)
	}
	got := r.Active()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("Active() is not sorted: %v", got)
		}
	}
}

// A nil registry is what every proxy built without one holds, and it must
// answer no rather than panic.
func TestANilRegistryIsSafe(t *testing.T) {
	var r *Registry
	if r.On("anything") {
		t.Error("a nil registry claims to be stubbing")
	}
	if r.Active() != nil {
		t.Error("a nil registry listed something")
	}
	if call, err := r.Match(t.Context(), "a", "GET", "/", nil); call != nil || err != nil {
		t.Errorf("a nil registry matched something: %v %v", call, err)
	}
}

// Toggling happens from an HTTP handler while the proxy reads it on every
// request, so the two really do race in production.
func TestConcurrentUseIsSafe(t *testing.T) {
	r := New(nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); r.Set("ms-rates", true) }()
		go func() { defer wg.Done(); _ = r.On("ms-rates") }()
		go func() { defer wg.Done(); _ = r.Active() }()
	}
	wg.Wait()
}

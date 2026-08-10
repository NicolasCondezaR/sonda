package fault

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Nothing is broken unless somebody said so. A registry that injects a fault
// nobody asked for would be the worst possible bug in this package.
func TestNothingFailsByDefault(t *testing.T) {
	r := New()
	if r.Next("ms-rates").Injects() {
		t.Error("a fresh registry is injecting faults")
	}
	if len(r.Services()) != 0 {
		t.Errorf("a fresh registry lists %v", r.Services())
	}
}

// The whole reason this is a count and not a probability: the same sequence
// twice. A rule you cannot reproduce is a rule you cannot debug against.
func TestOneInThreeIsASequenceNotACoinToss(t *testing.T) {
	r := New()
	if err := r.Set("ms-rates", Rule{Status: 503, OneIn: 3}); err != nil {
		t.Fatal(err)
	}

	var got []bool
	for i := 0; i < 9; i++ {
		got = append(got, r.Next("ms-rates").Injects())
	}
	want := []bool{false, false, true, false, false, true, false, false, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d injected=%v, want %v (whole run: %v)", i+1, got[i], want[i], got)
		}
	}
}

func TestOneInOneHitsEveryCall(t *testing.T) {
	r := New()
	r.Set("ms-rates", Rule{Status: 500})
	for i := 0; i < 5; i++ {
		if !r.Next("ms-rates").Injects() {
			t.Fatalf("call %d was let through", i+1)
		}
	}
}

// A rule only touches the service it names.
func TestARuleOnlyTouchesItsOwnService(t *testing.T) {
	r := New()
	r.Set("ms-rates", Rule{Cut: true})
	if r.Next("ms-auth").Injects() {
		t.Error("a rule on one service reached another")
	}
}

// Replacing a rule restarts its schedule. Inheriting the old count would make
// the first call after an edit fire at a moment nobody chose.
func TestReplacingARuleRestartsItsSchedule(t *testing.T) {
	r := New()
	r.Set("ms-rates", Rule{Status: 503, OneIn: 3})
	r.Next("ms-rates")
	r.Next("ms-rates") // the next one would have fired

	r.Set("ms-rates", Rule{Status: 503, OneIn: 3})
	if r.Next("ms-rates").Injects() {
		t.Error("the replaced rule kept the old count")
	}
}

func TestClearingPutsTheServiceBack(t *testing.T) {
	r := New()
	r.Set("ms-rates", Rule{Cut: true})
	r.Clear("ms-rates")
	if r.Next("ms-rates").Injects() {
		t.Error("the service is still failing after being cleared")
	}

	r.Set("a", Rule{Cut: true})
	r.Set("b", Rule{Cut: true})
	r.ClearAll()
	if len(r.Services()) != 0 {
		t.Errorf("ClearAll left %v", r.Services())
	}
}

// A rule that does nothing is a mistake, and accepting it silently means a
// service that behaves normally while somebody waits for it to break.
func TestARuleThatDoesNothingIsRefused(t *testing.T) {
	r := New()
	for _, bad := range []Rule{
		{},
		{LatencyMS: -1},
		{Status: 99},
		{Status: 700},
		{LatencyMS: 300000},
	} {
		if err := r.Set("ms-rates", bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if len(r.Services()) != 0 {
		t.Error("a refused rule was stored anyway")
	}
}

// Every injected failure has to be able to say it was injected, or the field
// shows Sonda's own interference as if the service had produced it.
func TestEveryActionExplainsItself(t *testing.T) {
	for _, a := range []Action{
		{Cut: true},
		{Status: 503},
		{Delay: 2 * time.Second},
		{Status: 503, Delay: time.Second},
	} {
		reason := a.Reason()
		if !strings.Contains(reason, "Sonda") || !strings.Contains(reason, "purpose") {
			t.Errorf("%+v explains itself as %q, which does not say it was deliberate", a, reason)
		}
	}
}

func TestDescribeReadsLikeTheRule(t *testing.T) {
	cases := []struct {
		rule Rule
		want []string
	}{
		{Rule{LatencyMS: 2000}, []string{"+2000ms"}},
		{Rule{Status: 503}, []string{"HTTP 503"}},
		{Rule{Cut: true}, []string{"connection cut"}},
		{Rule{LatencyMS: 500, Status: 500, OneIn: 3}, []string{"+500ms", "HTTP 500", "one call in 3"}},
		// Cutting outranks a status: there is no answer to carry one.
		{Rule{Cut: true, Status: 500}, []string{"connection cut"}},
	}
	for _, c := range cases {
		got := c.rule.Describe()
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%+v described as %q, missing %q", c.rule, got, want)
			}
		}
	}
	if (Rule{Cut: true, Status: 500}).Describe() != "connection cut" {
		t.Error("a cut rule also advertised a status it can never send")
	}
}

// Rules are set from an HTTP handler while the proxy reads them on every call.
func TestConcurrentUseIsSafe(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); r.Set("ms-rates", Rule{Status: 500}) }()
		go func() { defer wg.Done(); r.Next("ms-rates") }()
		go func() { defer wg.Done(); _ = r.Rules() }()
	}
	wg.Wait()
}

func TestANilRegistryIsSafe(t *testing.T) {
	var r *Registry
	if r.Next("anything").Injects() {
		t.Error("a nil registry injected a fault")
	}
	if r.Services() != nil || len(r.Rules()) != 0 {
		t.Error("a nil registry listed something")
	}
}

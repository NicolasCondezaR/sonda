package trigger

import (
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

func at(base time.Time, offset time.Duration) time.Time { return base.Add(offset) }

func call(id int64, target, method, path string, status int, started time.Time) store.Summary {
	return store.Summary{
		ID: id, Target: target, Protocol: "http", Method: method, Path: path,
		Status: status, StartedAt: started,
	}
}

// armed returns a registry whose clock is fixed, so "after arming" is a fact in
// these tests rather than a race with the wall clock.
func armed(t *testing.T, c Condition, now time.Time) *Registry {
	t.Helper()
	r := &Registry{now: func() time.Time { return now }}
	if err := r.Arm(c); err != nil {
		t.Fatal(err)
	}
	return r
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

func TestATriggerFiresOnTheCallThatCrosses(t *testing.T) {
	now := time.Now().UTC()
	r := armed(t, Condition{Service: "ms-rates", Failed: yes()}, now)

	if fired := r.Observe(call(1, "ms-rates", "GET", "/rates", 200, at(now, time.Second))); fired != nil {
		t.Fatal("a call that did not fail fired a trigger armed on failures")
	}
	fired := r.Observe(call(2, "ms-rates", "GET", "/rates", 500, at(now, 2*time.Second)))
	if fired == nil {
		t.Fatal("the failure never fired the trigger")
	}
	if fired.CallID != 2 {
		t.Errorf("fired on call %d, want the one that crossed", fired.CallID)
	}
}

// The lesson wait_for_call already learned: a bound that reaches backwards
// answers with something that had already happened.
func TestATriggerNeverMatchesBackwards(t *testing.T) {
	now := time.Now().UTC()
	r := armed(t, Condition{Service: "ms-rates"}, now)

	before := r.Observe(call(1, "ms-rates", "GET", "/rates", 500, at(now, -time.Second)))
	if before != nil {
		t.Fatal("a call captured before the arming fired the trigger")
	}
	// The same instant does not count either: it was already in flight.
	if same := r.Observe(call(2, "ms-rates", "GET", "/rates", 500, now)); same != nil {
		t.Fatal("a call captured at the moment of arming fired the trigger")
	}
}

func TestSingleFiresOnceAndKeepsTheMoment(t *testing.T) {
	now := time.Now().UTC()
	r := armed(t, Condition{Service: "ms-rates"}, now)

	first := r.Observe(call(1, "ms-rates", "GET", "/a", 200, at(now, time.Second)))
	second := r.Observe(call(2, "ms-rates", "GET", "/b", 200, at(now, 2*time.Second)))

	if first == nil || second != nil {
		t.Fatal("single mode fired more than once")
	}
	state := r.State()
	if state.Armed {
		t.Error("single mode stayed armed after firing")
	}
	if state.Fired == nil || state.Fired.CallID != 1 {
		t.Error("the moment was lost when the trigger disarmed itself — that is the whole bargain of single mode")
	}
	if state.Count != 1 {
		t.Errorf("count = %d, want 1", state.Count)
	}
}

func TestNormalStaysArmedAndCounts(t *testing.T) {
	now := time.Now().UTC()
	r := armed(t, Condition{Service: "ms-rates", Mode: Normal}, now)

	r.Observe(call(1, "ms-rates", "GET", "/a", 200, at(now, time.Second)))
	r.Observe(call(2, "ms-rates", "GET", "/b", 200, at(now, 2*time.Second)))

	state := r.State()
	if !state.Armed {
		t.Error("normal mode disarmed itself")
	}
	if state.Count != 2 {
		t.Errorf("count = %d, want both crossings", state.Count)
	}
	if state.Fired.CallID != 2 {
		t.Error("normal mode did not keep the most recent crossing")
	}
}

func TestRearmingForgetsThePreviousMoment(t *testing.T) {
	now := time.Now().UTC()
	r := armed(t, Condition{Service: "ms-rates"}, now)
	r.Observe(call(1, "ms-rates", "GET", "/a", 200, at(now, time.Second)))

	if err := r.Arm(Condition{Service: "ms-billing"}); err != nil {
		t.Fatal(err)
	}
	if state := r.State(); state.Fired != nil {
		t.Error("a moment recorded under the previous condition survived a re-arm, which makes it a lie about the current one")
	}
}

func TestClearingKeepsNothing(t *testing.T) {
	now := time.Now().UTC()
	r := armed(t, Condition{Service: "ms-rates"}, now)
	r.Observe(call(1, "ms-rates", "GET", "/a", 200, at(now, time.Second)))

	r.Clear()
	state := r.State()
	if state.Armed || state.Fired != nil || state.Condition != nil {
		t.Error("clearing left something behind to read")
	}
	if fired := r.Observe(call(2, "ms-rates", "GET", "/b", 200, at(now, 2*time.Second))); fired != nil {
		t.Error("a cleared trigger still fired")
	}
}

func TestAnEmptyConditionIsRefused(t *testing.T) {
	if err := (&Registry{now: time.Now}).Arm(Condition{}); err == nil {
		t.Error("an empty condition was armed; it fires on the next call whatever it is")
	}
	if err := (&Registry{now: time.Now}).Arm(Condition{Service: "x", Mode: "burst"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if err := (&Registry{now: time.Now}).Arm(Condition{Status: 42}); err == nil {
		t.Error("42 was accepted as an HTTP status")
	}
}

func TestMatching(t *testing.T) {
	now := time.Now().UTC()
	subject := call(1, "ms-rates", "POST", "/orders/ORD-1", 503, at(now, time.Second))

	cases := []struct {
		name string
		cond Condition
		want bool
	}{
		{"by service", Condition{Service: "ms-rates"}, true},
		{"service is not case sensitive", Condition{Service: "MS-RATES"}, true},
		{"another service", Condition{Service: "ms-billing"}, false},
		{"path is a substring, the way the search field works", Condition{Path: "orders"}, true},
		{"path that is not there", Condition{Path: "invoices"}, false},
		{"by status", Condition{Status: 503}, true},
		{"by method and status together", Condition{Method: "POST", Status: 503}, true},
		{"one part not matching fails the whole", Condition{Method: "GET", Status: 503}, false},
		{"by protocol", Condition{Protocol: "http"}, true},
		{"failed", Condition{Failed: yes()}, true},
		{"did not fail", Condition{Failed: no()}, false},
	}
	for _, c := range cases {
		if got := c.cond.Matches(subject); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

// A GraphQL error answers HTTP 200. If the trigger disagreed with the field
// about what a failure is, the disagreement would happen while nobody is
// watching — which is exactly when a trigger is doing its job.
func TestTheTriggerAgreesWithTheFieldAboutFailure(t *testing.T) {
	now := time.Now().UTC()
	graphql := store.Summary{ID: 1, Target: "bff", Protocol: "http", Status: 200,
		GraphQLErrors: 1, StartedAt: at(now, time.Second)}

	r := armed(t, Condition{Failed: yes()}, now)
	if fired := r.Observe(graphql); fired == nil {
		t.Error("a GraphQL error under HTTP 200 did not fire a trigger armed on failures")
	}
}

func TestDescribeSaysWhatIsArmedAndForHowLong(t *testing.T) {
	single := Condition{Service: "ms-rates", Failed: yes()}.Describe()
	if single != "ms-rates, failed (fires once)" {
		t.Errorf("Describe = %q", single)
	}
	repeating := Condition{Path: "/orders", Mode: Normal}.Describe()
	if repeating != "path contains /orders (stays armed)" {
		t.Errorf("Describe = %q", repeating)
	}
}

// Package fault makes a service misbehave on purpose.
//
// Retry logic, timeouts and degradation are written once and then never
// exercised, because making a real service fail on demand is awkward enough
// that nobody does it. Sonda is already in the path of every call, so it can do
// it for free — and it is the only place that can do it without touching the
// service or the client.
//
// Two decisions shape everything here.
//
// The first is that faults are deterministic. "One in every three" is a rule
// you can reproduce; "thirty-three per cent of the time" is a rule that behaves
// differently every run and turns a failing test into a coin toss. A counter
// gives the same sequence twice, which is what makes a fault worth debugging
// against.
//
// The second is that a fault is never silent. Every injected failure is
// recorded as one, so the field never shows Sonda's own interference as if the
// service had produced it.
package fault

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Rule is what to do to one service.
type Rule struct {
	// LatencyMS is added before the call is forwarded. The service still
	// answers normally; it just takes longer, which is what a timeout is
	// supposed to catch.
	LatencyMS int `json:"latency_ms,omitempty"`

	// Status short-circuits the call with this HTTP status instead of
	// forwarding it. The service is never reached.
	Status int `json:"status,omitempty"`

	// Cut drops the connection without answering at all — the failure a
	// well-written client handles differently from a 500, and the one almost
	// nobody tests.
	Cut bool `json:"cut,omitempty"`

	// OneIn applies the rule to one call in every N. One means every call.
	// Deliberately a count and not a probability: a sequence you can reproduce
	// is worth more here than one that is statistically fair.
	OneIn int `json:"one_in,omitempty"`
}

// armed is a rule plus the count that drives its schedule.
//
// The counter lives here and not on Rule because Rule is passed by value —
// callers build one and hand it over — and a value type carrying an atomic is
// a copy waiting to lose its state, which is exactly what vet objects to.
type armed struct {
	rule  Rule
	calls atomic.Int64
}

// Describe says what the rule does, in the words the interfaces show.
func (r Rule) Describe() string {
	var parts []string
	if r.LatencyMS > 0 {
		parts = append(parts, fmt.Sprintf("+%dms", r.LatencyMS))
	}
	if r.Cut {
		parts = append(parts, "connection cut")
	} else if r.Status > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", r.Status))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	if r.OneIn > 1 {
		out += fmt.Sprintf(", one call in %d", r.OneIn)
	}
	return out
}

// Valid rejects a rule that would do nothing or something impossible, so a
// mistake is a message rather than a service that quietly behaves normally.
func (r Rule) Valid() error {
	if r.LatencyMS < 0 {
		return fmt.Errorf("latency cannot be negative")
	}
	if r.LatencyMS > 120_000 {
		return fmt.Errorf("latency above two minutes is almost certainly a typo")
	}
	if r.Status != 0 && (r.Status < 100 || r.Status > 599) {
		return fmt.Errorf("%d is not an HTTP status", r.Status)
	}
	if r.OneIn < 0 {
		return fmt.Errorf("one_in cannot be negative")
	}
	if r.LatencyMS == 0 && r.Status == 0 && !r.Cut {
		return fmt.Errorf("the rule does nothing: set a latency, a status, or cut")
	}
	return nil
}

// Action is what to do with one particular call.
type Action struct {
	Delay  time.Duration
	Status int
	Cut    bool
}

// Injects reports whether this action changes anything.
func (a Action) Injects() bool { return a.Delay > 0 || a.Status > 0 || a.Cut }

// Reason is the sentence recorded on the capture, so a reader is never left
// wondering whether the service or the tool produced the failure.
func (a Action) Reason() string {
	switch {
	case a.Cut:
		return "connection cut by Sonda on purpose"
	case a.Status > 0 && a.Delay > 0:
		return fmt.Sprintf("answered %d by Sonda on purpose, after %s of injected delay", a.Status, a.Delay)
	case a.Status > 0:
		return fmt.Sprintf("answered %d by Sonda on purpose", a.Status)
	default:
		return fmt.Sprintf("%s of delay added by Sonda on purpose", a.Delay)
	}
}

// Registry holds the rules in force.
//
// In memory and forgotten on restart, exactly like stubbing and for the same
// reason: a service that has been failing since Tuesday because of a rule
// nobody remembers setting is a worse afternoon than the bug being chased.
type Registry struct {
	mu    sync.RWMutex
	rules map[string]*armed
}

func New() *Registry { return &Registry{rules: map[string]*armed{}} }

func (r *Registry) Set(service string, rule Rule) error {
	if err := rule.Valid(); err != nil {
		return err
	}
	if rule.OneIn < 1 {
		rule.OneIn = 1
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// A fresh entry, so changing a rule restarts its schedule rather than
	// inheriting a count from the rule it replaced.
	r.rules[service] = &armed{rule: rule}
	return nil
}

func (r *Registry) Clear(service string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rules, service)
}

func (r *Registry) ClearAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = map[string]*armed{}
}

// Rules is what is in force, for an interface to show. A nil registry has none,
// which is how a proxy built without one behaves.
func (r *Registry) Rules() map[string]string {
	if r == nil {
		return map[string]string{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]string, len(r.rules))
	for service, a := range r.rules {
		out[service] = a.rule.Describe()
	}
	return out
}

// Services lists what is being broken, sorted, so every surface reports the
// same order.
func (r *Registry) Services() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.rules))
	for service := range r.rules {
		out = append(out, service)
	}
	sort.Strings(out)
	return out
}

// Next decides what to do with the call about to be made.
//
// It counts every call, including the ones it lets through, because "one in
// three" has to mean one in three calls and not one in three matches.
func (r *Registry) Next(service string) Action {
	if r == nil {
		return Action{}
	}
	r.mu.RLock()
	a := r.rules[service]
	r.mu.RUnlock()
	if a == nil {
		return Action{}
	}

	n := a.calls.Add(1)
	if a.rule.OneIn > 1 && n%int64(a.rule.OneIn) != 0 {
		return Action{}
	}
	return Action{
		Delay:  time.Duration(a.rule.LatencyMS) * time.Millisecond,
		Status: a.rule.Status,
		Cut:    a.rule.Cut,
	}
}

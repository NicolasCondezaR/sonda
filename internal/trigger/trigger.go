// Package trigger arms a condition and records the moment it is crossed.
//
// A field with cursors measures what you already caught. It does nothing for
// the failure that happens twice an hour while you are looking somewhere else,
// which is the failure people actually open a debugger for. An instrument
// answers that with a trigger: name the condition, walk away, and come back to
// the moment it fired.
//
// Three decisions shape everything here.
//
// There is one trigger, not one per service. Faults and stubs are armed per
// service because they act on a service; a trigger acts on the instrument, the
// way a scope has one trigger and not one per channel.
//
// A trigger never matches backwards. It sees only calls stored after it was
// armed, to the nanosecond — the same lesson wait_for_call learned, where a
// bound rounded to the second matched traffic from before the wait began and
// answered with something that had already happened.
//
// And firing only records. Freezing a view, selecting a call, drawing a banner
// are consequences each surface decides for itself, because the one that must
// never happen is a trigger taking the view away from someone who is reading
// it.
package trigger

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Mode is how a trigger behaves once it has fired, in the vocabulary of the
// instrument it is borrowed from.
type Mode string

const (
	// Single disarms itself the moment it fires. This is the default, and it
	// is what makes "tell me when this happens again" usable: the answer is
	// one moment, still there when you come back to it.
	Single Mode = "single"
	// Normal stays armed and keeps counting. Useful while narrowing something
	// down, and noisier by design.
	Normal Mode = "normal"
)

// Condition is what has to cross for the trigger to fire.
//
// The fields are the ones store.Filter already searches by, deliberately: the
// vocabulary a person already uses to find a call is the vocabulary they should
// use to wait for one, and inventing a second one would mean two things to
// learn and two to keep in step.
type Condition struct {
	Service string `json:"service,omitempty"`
	Method  string `json:"method,omitempty"`
	// Path matches as a substring, the way the search field does. A trigger you
	// have to spell exactly is one you arm wrongly and then wait on forever.
	Path     string `json:"path,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Status   int    `json:"status,omitempty"`

	// Failed takes three states: nil matches anything, true only failures —
	// GraphQL errors under HTTP 200 included — and false only calls that did
	// not fail, which is how you wait for a fix to land rather than for the
	// next break.
	Failed *bool `json:"failed,omitempty"`

	Mode Mode `json:"mode,omitempty"`
}

// Valid refuses the conditions that would never fire, or would fire on
// everything.
func (c Condition) Valid() error {
	if c.Mode != "" && c.Mode != Single && c.Mode != Normal {
		return fmt.Errorf("mode must be %q or %q", Single, Normal)
	}
	if c.Status != 0 && (c.Status < 100 || c.Status > 599) {
		return fmt.Errorf("status %d is not an HTTP status", c.Status)
	}
	if c.Service == "" && c.Method == "" && c.Path == "" &&
		c.Protocol == "" && c.Status == 0 && c.Failed == nil {
		// An empty condition fires on the first call that crosses, which is
		// never what anybody meant and is indistinguishable from a bug in the
		// matching.
		return fmt.Errorf("a trigger needs at least one condition, or it fires on the next call whatever it is")
	}
	return nil
}

// Describe says what is armed in one line, for a surface that has one line.
func (c Condition) Describe() string {
	var parts []string
	if c.Service != "" {
		parts = append(parts, c.Service)
	}
	if c.Method != "" {
		parts = append(parts, c.Method)
	}
	if c.Path != "" {
		parts = append(parts, "path contains "+c.Path)
	}
	if c.Protocol != "" {
		parts = append(parts, c.Protocol)
	}
	if c.Status != 0 {
		parts = append(parts, fmt.Sprintf("status %d", c.Status))
	}
	if c.Failed != nil {
		if *c.Failed {
			parts = append(parts, "failed")
		} else {
			parts = append(parts, "did not fail")
		}
	}
	line := strings.Join(parts, ", ")
	if c.mode() == Normal {
		return line + " (stays armed)"
	}
	return line + " (fires once)"
}

func (c Condition) mode() Mode {
	if c.Mode == Normal {
		return Normal
	}
	return Single
}

// Matches is the whole of the evaluation, and it is string comparison on
// purpose. It runs on the goroutine that persists captures, so it may not
// query, allocate much, or block: a trigger that slowed down storage would
// cost the capture it exists to catch.
func (c Condition) Matches(s store.Summary) bool {
	if c.Service != "" && !strings.EqualFold(c.Service, s.Target) {
		return false
	}
	if c.Method != "" && !strings.EqualFold(c.Method, s.Method) {
		return false
	}
	if c.Path != "" && !strings.Contains(strings.ToLower(s.Path), strings.ToLower(c.Path)) {
		return false
	}
	if c.Protocol != "" && !strings.EqualFold(c.Protocol, s.Protocol) {
		return false
	}
	if c.Status != 0 && c.Status != s.Status {
		return false
	}
	if c.Failed != nil && *c.Failed != Failed(s) {
		return false
	}
	return true
}

// Failed is the same definition the rest of Sonda uses, and it is here so the
// trigger cannot drift from the field: a call the interface paints red and the
// trigger considers healthy would be the worst kind of disagreement, because
// the reader is not there to see it happen.
//
// It mirrors store.faultPredicate in SQL, summaryFailed in the API, isFault in
// the web client and Call.Fault in the terminal one.
func Failed(s store.Summary) bool {
	if s.Error != "" {
		return true
	}
	// A GraphQL error arrives under HTTP 200 with no transport complaint, and
	// a Postgres session has no HTTP status at all.
	if s.GraphQLErrors > 0 || s.PostgresErrors > 0 {
		return true
	}
	if s.GRPCStatus != nil {
		return *s.GRPCStatus != 0
	}
	return s.Status >= 400
}

// Fired is the moment a condition was crossed.
type Fired struct {
	At     time.Time `json:"at"`
	CallID int64     `json:"call_id"`
	Target string    `json:"target"`
	Path   string    `json:"path,omitempty"`
}

// State is everything a surface needs to draw the trigger.
type State struct {
	Armed     bool       `json:"armed"`
	Condition *Condition `json:"condition,omitempty"`
	Describe  string     `json:"describe,omitempty"`
	ArmedAt   *time.Time `json:"armed_at,omitempty"`
	Fired     *Fired     `json:"fired,omitempty"`
	// Count is how many times this arming has fired. Always 0 or 1 in single
	// mode, which is the point of single mode.
	Count int `json:"count"`
}

// Registry holds the one armed trigger.
//
// In memory and not persisted, the same as stubs and injected faults: an
// instrument that came back from a restart still armed would fire on something
// nobody was waiting for any more.
type Registry struct {
	mu        sync.RWMutex
	condition *Condition
	armedAt   time.Time
	fired     *Fired
	count     int

	// now is swapped in tests. Time is the one input this package cannot
	// observe honestly from outside.
	now func() time.Time
}

func New() *Registry { return &Registry{now: time.Now} }

// Arm replaces whatever was armed, and clears any previous firing along with
// it: a moment recorded under one condition would be a lie under the next.
func (r *Registry) Arm(c Condition) error {
	if err := c.Valid(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Mode = c.mode()
	r.condition, r.armedAt, r.fired, r.count = &c, r.now().UTC(), nil, 0
	return nil
}

// Clear disarms, and keeps nothing. Reading a fired moment after disarming
// would be reading about a trigger that is no longer there.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.condition, r.fired, r.count = nil, nil, 0
}

// Observe is called for every stored call. It reports the firing when this call
// is the one that crossed, and nil the rest of the time — which is almost
// always, so the fast path is one read lock and a nil check.
func (r *Registry) Observe(s store.Summary) *Fired {
	r.mu.RLock()
	armed := r.condition
	armedAt := r.armedAt
	r.mu.RUnlock()
	if armed == nil {
		return nil
	}

	// Never backwards. A call stored before the arming is not evidence that the
	// condition happened while anyone was watching for it.
	if !s.StartedAt.After(armedAt) {
		return nil
	}
	if !armed.Matches(s) {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-checked under the write lock: two captures can cross at once, and in
	// single mode exactly one of them is the moment.
	if r.condition == nil || (r.condition.mode() == Single && r.fired != nil) {
		return nil
	}
	fired := &Fired{At: s.StartedAt.UTC(), CallID: s.ID, Target: s.Target, Path: s.Path}
	r.fired = fired
	r.count++
	if r.condition.mode() == Single {
		// Disarmed, but the moment stays readable — that is the whole bargain
		// of single mode.
		r.condition.Mode = Single
	}
	return fired
}

// State is what every surface reads.
func (r *Registry) State() State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.condition == nil {
		return State{}
	}
	armedAt := r.armedAt
	condition := *r.condition
	out := State{
		Armed:     condition.mode() == Normal || r.fired == nil,
		Condition: &condition,
		Describe:  condition.Describe(),
		ArmedAt:   &armedAt,
		Fired:     r.fired,
		Count:     r.count,
	}
	return out
}

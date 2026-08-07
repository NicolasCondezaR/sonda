package api

import (
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

func i32(v int32) *int32 { return &v }

// The tree must agree with the field about what is broken. If they disagree,
// one of the two screens is lying, and the tree is the one people will believe
// because it looks more considered.
func TestTheTreeUsesTheSameDefinitionOfFailureAsEverythingElse(t *testing.T) {
	ok := int32(0)
	cases := []struct {
		name string
		call store.Summary
		want bool
	}{
		{"plain success", store.Summary{Status: 200}, false},
		{"http error", store.Summary{Status: 500}, true},
		{"not found", store.Summary{Status: 404}, true},
		{"transport failure", store.Summary{Error: "connection refused"}, true},
		// gRPC answers 200 even when it failed, so the code is the only thing
		// carrying the outcome.
		{"grpc failure under a 200", store.Summary{Status: 200, GRPCStatus: i32(3)}, true},
		{"grpc success under a 200", store.Summary{Status: 200, GRPCStatus: &ok}, false},
	}
	for _, c := range cases {
		if got := summaryFailed(c.call); got != c.want {
			t.Errorf("%s: failed = %v, want %v", c.name, got, c.want)
		}
	}
}

// A node that says only "failed" sends the reader back to the detail view,
// which is the trip the tree exists to save.
func TestAFailedNodeCarriesWhyItFailed(t *testing.T) {
	cases := []struct {
		name string
		call store.Summary
		want string
	}{
		{"transport", store.Summary{Error: "connection refused"}, "connection refused"},
		{"grpc with a message", store.Summary{Status: 200, GRPCStatus: i32(3), GRPCMessage: "falta sku"},
			"InvalidArgument: falta sku"},
		{"grpc without one", store.Summary{Status: 200, GRPCStatus: i32(7)}, "PermissionDenied"},
	}
	for _, c := range cases {
		if got := toTraceCall(c.call).Detail; got != c.want {
			t.Errorf("%s: detail = %q, want %q", c.name, got, c.want)
		}
	}

	// And a healthy call says nothing, rather than an empty label.
	if got := toTraceCall(store.Summary{Status: 200}).Detail; got != "" {
		t.Errorf("a successful call carries a detail: %q", got)
	}
}

func TestTheTraceCallKeepsWhatTheTreeNeeds(t *testing.T) {
	started := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	got := toTraceCall(store.Summary{
		ID: 42, Target: "ms-auth", Method: "POST", Path: "/v1/token",
		StartedAt: started, Duration: 120 * time.Millisecond, TraceID: "abc",
	})

	if got.ID != 42 || got.Target != "ms-auth" || got.TraceID != "abc" {
		t.Errorf("identity was lost: %+v", got)
	}
	// Timing is the whole basis of the arranging; losing it silently would
	// produce a flat tree that looks fine.
	if !got.Started.Equal(started) || got.Duration != 120*time.Millisecond {
		t.Errorf("timing was lost: %v %v", got.Started, got.Duration)
	}
}

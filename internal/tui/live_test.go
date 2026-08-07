package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAgainstLiveAPI renders a frame from a running sonda.
//
// Every other test in this package feeds the model hand-made structs, which
// proves the rendering but not the part most likely to be wrong: that the JSON
// coming off the API actually lands in the fields this package reads. Set
// SONDA_API to run it.
//
//	SONDA_API=http://127.0.0.1:9000 go test ./internal/tui/ -run Live -v
func TestAgainstLiveAPI(t *testing.T) {
	base := os.Getenv("SONDA_API")
	if base == "" {
		t.Skip("set SONDA_API to check the terminal client against a running sonda")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := NewClient(base)
	m := New(ctx, client, nil)

	targets, err := client.Targets(ctx)
	if err != nil {
		t.Fatalf("could not read targets: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("the API reports no targets, so there is nothing to draw")
	}
	m.targets = targets

	// The whole field, so the frame has something in it.
	m.failedOnly = false
	calls, err := client.Calls(ctx, false, "", 30*time.Minute, 500)
	if err != nil {
		t.Fatalf("could not read calls: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("no captures in the last thirty minutes; send some traffic first")
	}
	m.calls = calls
	m.live = true
	m.now = time.Now()

	if m.stats, err = client.Stats(ctx); err != nil {
		t.Fatalf("could not read stats: %v", err)
	}

	// Timestamps must survive the round trip, or every call piles into one
	// column and the field is a lie.
	for _, call := range calls {
		if call.Started().IsZero() {
			t.Fatalf("call %d came back with an unparsed timestamp %q", call.ID, call.StartedAt)
		}
	}

	// A real capture, read end to end.
	detail, err := client.Detail(ctx, calls[0].ID)
	if err != nil {
		t.Fatalf("could not read call %d: %v", calls[0].ID, err)
	}
	if detail.Path != calls[0].Path {
		t.Errorf("the detail is a different call: %q vs %q", detail.Path, calls[0].Path)
	}
	m.detail = detail

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	frame := updated.(Model).View()

	for i, line := range strings.Split(frame, "\n") {
		if got := len([]rune(stripANSI(line))); got > 120 {
			t.Errorf("line %d is %d characters wide", i, got)
		}
	}
	// Something has to be drawn on the field, or the geometry is off.
	plain := stripANSI(frame)
	if !strings.Contains(plain, markCall) && !strings.Contains(plain, markFault) &&
		!strings.Contains(plain, markMixed) {
		t.Error("captures were loaded but nothing was drawn on the field")
	}

	t.Log("\n" + frame)
}

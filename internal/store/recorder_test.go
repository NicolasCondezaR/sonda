package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Ctrl+C must not throw away the capture that was just taken — which is
// routinely the one being debugged.
func TestRecorderDrainsBufferOnShutdown(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rec := NewRecorder(s, 100)
	for i := 0; i < 20; i++ {
		rec.Record(sampleCall("api", "GET", "/orders", 200, nil))
	}

	// Cancelled before Run even starts: the buffered calls still have to land.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		rec.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	rec.Wait()

	st, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Calls != 20 {
		t.Errorf("%d calls survived the shutdown, want 20", st.Calls)
	}
	if rec.Dropped() != 0 {
		t.Errorf("dropped %d calls during a clean shutdown", rec.Dropped())
	}
}

// Losing captures is acceptable under pressure; lying about it is not.
func TestRecorderCountsDropsWhenBufferIsFull(t *testing.T) {
	rec := NewRecorder(nil, 2) // never drained, so the buffer fills immediately
	for i := 0; i < 10; i++ {
		rec.Record(sampleCall("api", "GET", "/orders", 200, nil))
	}
	if rec.Dropped() != 8 {
		t.Errorf("dropped = %d, want 8", rec.Dropped())
	}
}

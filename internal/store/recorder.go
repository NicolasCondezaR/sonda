package store

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Recorder decouples capture from proxying.
//
// The proxy must never wait on the database. A slow or locked SQLite file has
// to cost captured calls, never latency on the request the developer is
// actually debugging — a debugger that changes the timing of the system it
// observes is worse than no debugger. Record therefore never blocks: when the
// buffer is full the call is dropped and counted, and the drop count is
// reported by the API so the loss is visible instead of silent.
type Recorder struct {
	store   *Store
	calls   chan *Call
	dropped atomic.Int64
	drained chan struct{}

	// stored is notified after a call reaches the database, which is what the
	// live view subscribes to. It fires on the recorder's own goroutine, so an
	// implementation that blocks stalls persistence — the hub that consumes it
	// drops rather than waits, for the same reason Record does.
	stored func(*Call)
}

func NewRecorder(s *Store, buffer int) *Recorder {
	return &Recorder{
		store:   s,
		calls:   make(chan *Call, buffer),
		drained: make(chan struct{}),
	}
}

// OnStored registers the live-view notification. Call it before Run.
func (r *Recorder) OnStored(fn func(*Call)) { r.stored = fn }

func (r *Recorder) Record(c *Call) {
	select {
	case r.calls <- c:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// Run writes buffered calls until ctx is cancelled, then drains what is left so
// a Ctrl+C does not throw away the capture that was just made.
func (r *Recorder) Run(ctx context.Context) {
	defer close(r.drained)
	for {
		select {
		case c := <-r.calls:
			r.write(context.WithoutCancel(ctx), c)
		case <-ctx.Done():
			for {
				select {
				case c := <-r.calls:
					r.write(context.WithoutCancel(ctx), c)
				default:
					return
				}
			}
		}
	}
}

// Wait blocks until Run has finished draining.
func (r *Recorder) Wait() { <-r.drained }

func (r *Recorder) write(ctx context.Context, c *Call) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := r.store.Insert(ctx, c); err != nil {
		r.dropped.Add(1)
		slog.Error("could not persist captured call",
			"target", c.Target, "method", c.Method, "path", c.Path, "error", err)
		return
	}
	// Only after it is durable: the live view must never show a call that a
	// reload would not find.
	if r.stored != nil {
		r.stored(c)
	}
}

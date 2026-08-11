package store

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func pgFrame(typ byte, body ...[]byte) []byte {
	var payload []byte
	for _, part := range body {
		payload = append(payload, part...)
	}
	out := append([]byte{typ}, make([]byte, 4)...)
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)+4))
	return append(out, payload...)
}

func pgStartupFrame(pairs ...string) []byte {
	body := binary.BigEndian.AppendUint32(nil, 196608)
	for _, p := range pairs {
		body = append(body, append([]byte(p), 0)...)
	}
	body = append(body, 0)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...)
}

func z(s string) []byte { return append([]byte(s), 0) }

func pgSession(sent, received []byte) *Call {
	return &Call{
		Target:     "orders-db",
		Protocol:   "postgres",
		Method:     "SESSION",
		ClientAddr: "127.0.0.1:5555",
		StartedAt:  time.Now().UTC(),
		Duration:   3 * time.Millisecond,
		Request:    Message{Body: sent, Size: int64(len(sent))},
		Response:   Message{Body: received, Size: int64(len(received))},
	}
}

// A listing carries no bodies, so without a reading taken on insert every
// session to a database shows up as the same row repeated.
func TestASessionIsDescribedFromItsStream(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	call := pgSession(
		append(pgStartupFrame("user", "app", "database", "orders"),
			pgFrame('Q', z("SELECT id FROM orders WHERE total > 100"))...),
		append(pgFrame('C', z("SELECT 12")), pgFrame('Z', []byte{'I'})...),
	)
	id, err := s.Insert(ctx, call)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// The path is the database, read off the startup message rather than made
	// up: there is no URL in this protocol to borrow one from.
	if got.Path != "orders" {
		t.Errorf("path = %q, want the database name", got.Path)
	}
	if got.PostgresSummary != "SELECT id FROM orders WHERE total > 100 -> SELECT 12" {
		t.Errorf("summary = %q", got.PostgresSummary)
	}
	if got.PostgresErrors != 0 {
		t.Errorf("errors = %d, want 0", got.PostgresErrors)
	}
}

// A failed statement has no status code anywhere. If the count did not reach
// the column, the fault filter and the channel rail would both call this
// session healthy — which is the failure someone opened Sonda to find.
func TestAServerErrorMakesASessionAFault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Insert(ctx, pgSession(
		append(pgStartupFrame("user", "app", "database", "orders"),
			pgFrame('Q', z("SELECT * FROM ordrs"))...),
		pgFrame('E',
			append([]byte{'S'}, z("ERROR")...),
			append([]byte{'C'}, z("42P01")...),
			append([]byte{'M'}, z(`relation "ordrs" does not exist`)...),
			[]byte{0}),
	)); err != nil {
		t.Fatal(err)
	}

	failed, err := s.List(ctx, Filter{Failed: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 {
		t.Fatalf("%d failed calls, want 1: a SQL error is a failure", len(failed))
	}
	if failed[0].PostgresErrors != 1 {
		t.Errorf("errors = %d, want 1", failed[0].PostgresErrors)
	}
	if failed[0].PostgresSummary != `error 42P01: relation "ordrs" does not exist` {
		t.Errorf("summary = %q", failed[0].PostgresSummary)
	}

	// And the rail has to agree with the field, or one of them is lying.
	stats, err := s.Stats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.ByTarget) != 1 || stats.ByTarget[0].Faults != 1 {
		t.Errorf("by target = %+v, want one fault", stats.ByTarget)
	}
}

// A Postgres stream is full of NUL bytes, so the body itself is treated as
// binary and never indexed. Without the summary in the index the SQL would be
// unfindable, and finding calls is the point of the tool.
func TestTheStatementIsSearchable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.Insert(ctx, pgSession(
		append(pgStartupFrame("user", "app", "database", "orders"),
			pgFrame('Q', z("SELECT id FROM shipments"))...),
		pgFrame('C', z("SELECT 3")),
	)); err != nil {
		t.Fatal(err)
	}

	found, err := s.List(ctx, Filter{Search: "shipments"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("%d results, want 1", len(found))
	}
}

// The summary is one line cut at ninety characters. A capture is one statement
// now, so what makes it findable has to be the statement itself — the table
// named halfway down a formatted query, and the value that was bound to it.
func TestTheWholeStatementIsSearchableNotJustItsSummary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sql := "SELECT o.id, o.total, o.created_at, o.customer_id, o.status, o.currency " +
		"FROM orders o JOIN shipments_archive a ON a.order_id = o.id WHERE o.reference = $1"
	parse := pgFrame('P', z(""), z(sql), []byte{0, 0})
	bind := pgFrame('B', z(""), z(""),
		[]byte{0, 0}, // no format codes: everything is text
		[]byte{0, 1}, // one parameter
		binary.BigEndian.AppendUint32(nil, uint32(len("ORD-4821"))), []byte("ORD-4821"),
		[]byte{0, 0}) // no result format codes

	if _, err := s.Insert(ctx, pgSession(
		append(pgStartupFrame("user", "app", "database", "orders"), append(parse, bind...)...),
		append(pgFrame('C', z("SELECT 1")), pgFrame('Z', []byte{'I'})...),
	)); err != nil {
		t.Fatal(err)
	}

	for _, term := range []string{"shipments_archive", "ORD-4821"} {
		found, err := s.List(ctx, Filter{Search: term})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != 1 {
			t.Errorf("%d results for %q, want 1", len(found), term)
		}
	}
}

// Nothing but a Postgres capture may be read as one. An HTTP body that happens
// to start with plausible bytes must not come back with a database name.
func TestOnlyAPostgresCaptureIsReadAsOne(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	call := sampleCall("api", "POST", "/orders", 200, pgStartupFrame("user", "app", "database", "orders"))
	id, err := s.Insert(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/orders" || got.PostgresSummary != "" {
		t.Errorf("an http capture was read as a session: path=%q summary=%q", got.Path, got.PostgresSummary)
	}
}

package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/trace"
)

// The protocol is written by hand here for the reason the WebSocket tests give:
// a test that borrows the encoder from a Postgres driver proves the driver and
// Sonda agree, not that Sonda reads the wire.

const dbPassword = "hunter2-must-never-be-stored"

func pgMsg(typ byte, body ...[]byte) []byte {
	var payload []byte
	for _, part := range body {
		payload = append(payload, part...)
	}
	out := append([]byte{typ}, make([]byte, 4)...)
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)+4))
	return append(out, payload...)
}

func pgStartup(pairs ...string) []byte {
	body := binary.BigEndian.AppendUint32(nil, 196608) // protocol 3.0
	for _, p := range pairs {
		body = append(body, append([]byte(p), 0)...)
	}
	body = append(body, 0)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...)
}

func pgText(s string) []byte { return append([]byte(s), 0) }

func pgU32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func pgU16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

func pgCat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// startScriptedPostgres is an upstream that answers message by message, the way
// a backend does. The dump-the-whole-reply fake below cannot drive a
// conversation with two statements in it: the second question only exists
// because the first was answered.
//
// answer returns what the server sends back for one client message, or nil for
// silence. A startup message is reported as type 0, which no typed message can
// be.
func startScriptedPostgres(t *testing.T, answer func(typ byte, body []byte) []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go scriptedSession(conn, answer)
		}
	}()
	return ln.Addr().String()
}

func scriptedSession(conn net.Conn, answer func(typ byte, body []byte) []byte) {
	defer conn.Close()

	// The first message has no type byte: a bare length, then the body.
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	body := make([]byte, int(binary.BigEndian.Uint32(head))-4)
	if _, err := io.ReadFull(conn, body); err != nil {
		return
	}
	if reply := answer(0, body); reply != nil {
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}

	head = make([]byte, 5)
	for {
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		body := make([]byte, int(binary.BigEndian.Uint32(head[1:]))-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		if head[0] == 'X' {
			return
		}
		if reply := answer(head[0], body); reply != nil {
			if _, err := conn.Write(reply); err != nil {
				return
			}
		}
	}
}

// pgBackend is an ordinary database: it authenticates without asking for a
// password and answers every simple query with one command tag.
func pgBackend(typ byte, body []byte) []byte {
	switch typ {
	case 0:
		return pgCat(
			pgMsg('R', pgU32(0)),
			pgMsg('S', pgText("server_version"), pgText("17")),
			pgMsg('Z', []byte{'I'}),
		)
	case 'Q':
		return pgCat(pgMsg('C', pgText("SELECT 1")), pgMsg('Z', []byte{'I'}))
	case 'S':
		return pgMsg('Z', []byte{'I'})
	}
	return nil
}

// readUntilReady consumes messages until the server says it is ready again,
// which is what a driver does and what makes the next statement a new cycle.
func readUntilReady(conn net.Conn) error {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	head := make([]byte, 5)
	for {
		if _, err := io.ReadFull(conn, head); err != nil {
			return err
		}
		body := make([]byte, int(binary.BigEndian.Uint32(head[1:]))-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		if head[0] == 'Z' {
			return nil
		}
	}
}

// settledCalls waits for the captures a connection owes and then insists it
// owes no more. Counting the moment the last one arrives would pass a session
// that also writes a phantom row a heartbeat later — which is exactly what a
// statement boundary drawn in the wrong place produces.
func settledCalls(t *testing.T, rec *collector, want int) []*store.Call {
	t.Helper()
	calls := waitForCalls(t, rec, want)
	time.Sleep(200 * time.Millisecond)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != want {
		for i, c := range rec.calls {
			t.Logf("capture %d: %s %s error=%q %d bytes sent", i, c.Method, c.Path, c.Error, c.Request.Size)
		}
		t.Fatalf("%d captures, want exactly %d", len(rec.calls), want)
	}
	return calls
}

func waitForCalls(t *testing.T, rec *collector, want int) []*store.Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		got := len(rec.calls)
		rec.mu.Unlock()
		if got >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d calls were recorded, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]*store.Call, len(rec.calls))
	copy(out, rec.calls)
	return out
}

// fakePostgres is an upstream that runs one cleartext-password handshake and
// answers one query. It keeps everything it received, so a test can prove the
// bytes Sonda forwarded are the bytes the client sent — blanking the capture
// must not blank the wire.
type fakePostgres struct {
	addr     string
	received chan []byte
}

func startFakePostgres(t *testing.T, reply []byte) *fakePostgres {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	f := &fakePostgres{addr: ln.Addr().String(), received: make(chan []byte, 1)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := conn.Write(reply); err != nil {
			return
		}
		// Read until the client half-closes, which is what Sonda does when the
		// client goes away.
		got, _ := io.ReadAll(conn)
		f.received <- got
	}()
	return f
}

// servePostgresOnce wires a proxy in front of the fake database and returns the
// address a client should dial.
func servePostgresOnce(t *testing.T, upstream string, rec Recorder, maxBody int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	target := config.Target{
		Name:     "orders-db",
		Listen:   ln.Addr().String(),
		Upstream: "postgres://" + upstream,
		Protocol: config.ProtocolPostgres,
	}
	p := New(target, maxBody, rec, nil, nil)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		p.ServePostgres(conn)
	}()
	return ln.Addr().String()
}

// serverScript is a whole backend side of a session: the password demand, the
// key data, and the answer to one query.
func serverScript() []byte {
	var out []byte
	out = append(out, pgMsg('R', pgU32(3))...)                               // AuthenticationCleartextPassword
	out = append(out, pgMsg('R', pgU32(0))...)                               // AuthenticationOk
	out = append(out, pgMsg('K', pgU32(4242), pgU32(0xDEADBEEF))...)         // BackendKeyData
	out = append(out, pgMsg('S', pgText("server_version"), pgText("17"))...) // ParameterStatus
	out = append(out, pgMsg('Z', []byte{'I'})...)                            // ReadyForQuery
	out = append(out, pgMsg('C', pgText("SELECT 1"))...)                     // CommandComplete
	return out
}

// clientScript is the frontend side: connect, authenticate, ask one question.
func clientScript() []byte {
	var out []byte
	out = append(out, pgStartup("user", "app", "database", "orders")...)
	out = append(out, pgMsg('p', pgText(dbPassword))...)
	out = append(out, pgMsg('Q', pgText("SELECT id FROM orders WHERE total > 100"))...)
	out = append(out, pgMsg('X')...) // Terminate
	return out
}

// This is the test the whole change is built around. A password that reaches
// sonda.db is a password in a plaintext file that an agent can read over MCP,
// and no amount of care at display time takes it back out again.
func TestThePasswordIsNotInTheStoredCapture(t *testing.T) {
	upstream := startFakePostgres(t, serverScript())
	rec := &collector{}
	addr := servePostgresOnce(t, upstream.addr, rec, 64<<10)

	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(clientScript()); err != nil {
		t.Fatal(err)
	}
	client.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, client)
	client.Close()

	call := waitForCall(t, rec)

	// Straight through the real database, because that is the file the secret
	// would sit in.
	db, err := store.Open(filepath.Join(t.TempDir(), "sonda.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := db.Insert(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string][]byte{
		"request":  stored.Request.Body,
		"response": stored.Response.Body,
	} {
		if bytes.Contains(body, []byte(dbPassword)) {
			t.Errorf("the password is in the stored %s bytes", name)
		}
	}
	// The cancellation key is a credential too, and it comes back the other way.
	if bytes.Contains(stored.Response.Body, pgU32(0xDEADBEEF)) {
		t.Error("the cancellation key is in the stored response bytes")
	}

	// Everything that is not a secret has to survive, or the capture stopped
	// being a record of what crossed.
	if !bytes.Contains(stored.Request.Body, []byte("SELECT id FROM orders")) {
		t.Error("the statement was lost from the capture")
	}
	if !bytes.Contains(stored.Request.Body, []byte("orders")) {
		t.Error("the startup parameters were lost from the capture")
	}
}

// Blanking rewrites the copy, never the wire. If it touched what was forwarded,
// the database would reject every login and Sonda would be the bug.
func TestTheRealPasswordStillReachesTheDatabase(t *testing.T) {
	upstream := startFakePostgres(t, serverScript())
	rec := &collector{}
	addr := servePostgresOnce(t, upstream.addr, rec, 64<<10)

	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	sent := clientScript()
	if _, err := client.Write(sent); err != nil {
		t.Fatal(err)
	}
	client.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, client)
	client.Close()

	select {
	case got := <-upstream.received:
		if !bytes.Equal(got, sent) {
			t.Errorf("the upstream received %d bytes, the client sent %d, and they differ", len(got), len(sent))
		}
		if !bytes.Contains(got, []byte(dbPassword)) {
			t.Error("the real password never reached the database, so nobody could log in")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never finished reading")
	}
}

// A session that cannot open is still a session that happened, and one with
// nothing written down is the hardest kind of failure to chase.
func TestAnUnreachableDatabaseIsStillRecorded(t *testing.T) {
	// A port nothing is on: opened and closed, so the address is real and free.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := dead.Addr().String()
	dead.Close()

	rec := &collector{}
	front := servePostgresOnce(t, addr, rec, 1024)

	conn, err := net.Dial("tcp", front)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(pgStartup("user", "app"))
	io.Copy(io.Discard, conn)
	conn.Close()

	call := waitForCall(t, rec)
	if call.Error == "" {
		t.Error("the transport failure was not recorded")
	}
	if call.Protocol != config.ProtocolPostgres || call.Method != postgresMethod {
		t.Errorf("protocol=%q method=%q", call.Protocol, call.Method)
	}
}

// The point of the whole feature. Behind a pool a connection carries hours of
// an application's SQL, and a row per connection is a row nobody can hang off a
// request. One row per statement is what makes an N+1 forty rows instead of a
// suspicion.
func TestEachStatementIsItsOwnCapture(t *testing.T) {
	rec := &collector{}
	addr := servePostgresOnce(t, startScriptedPostgres(t, pgBackend), rec, 64<<10)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(pgStartup("user", "app", "database", "orders"))
	if err := readUntilReady(conn); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT id FROM orders", "UPDATE orders SET total = 1"} {
		conn.Write(pgMsg('Q', pgText(sql)))
		if err := readUntilReady(conn); err != nil {
			t.Fatal(err)
		}
	}
	conn.Write(pgMsg('X'))
	conn.Close()

	calls := settledCalls(t, rec, 2)

	for i, want := range []string{"SELECT id FROM orders", "UPDATE orders SET total = 1"} {
		other := "UPDATE orders SET total = 1"
		if i == 1 {
			other = "SELECT id FROM orders"
		}
		if !bytes.Contains(calls[i].Request.Body, []byte(want)) {
			t.Errorf("capture %d does not hold %q", i, want)
		}
		if bytes.Contains(calls[i].Request.Body, []byte(other)) {
			t.Errorf("capture %d also holds %q: the statement boundary was ignored", i, other)
		}
		// The database is on every row, not only on the one that happens to
		// carry the startup message.
		if calls[i].Path != "orders" {
			t.Errorf("capture %d path = %q, want the database", i, calls[i].Path)
		}
		if calls[i].Method != postgresStatementMethod {
			t.Errorf("capture %d method = %q", i, calls[i].Method)
		}
	}

	// The second statement is timed from when it was sent, not from when the
	// connection opened. Everything about hanging SQL under a request depends
	// on that.
	first, second := calls[0], calls[1]
	if !second.StartedAt.After(first.StartedAt.Add(first.Duration).Add(-time.Millisecond)) {
		t.Errorf("the second statement starts at %v, before the first ended at %v",
			second.StartedAt, first.StartedAt.Add(first.Duration))
	}
}

// The connection's own facts — the startup parameters, the mechanism that was
// demanded, the server's settings — happen once and belong somewhere. They ride
// in the first statement of the connection rather than in a row of their own.
func TestTheConnectionsOpeningRidesTheFirstStatement(t *testing.T) {
	rec := &collector{}
	addr := servePostgresOnce(t, startScriptedPostgres(t, pgBackend), rec, 64<<10)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(pgStartup("user", "app", "database", "orders"))
	readUntilReady(conn)

	// Idle first, the way a pooled connection is. If the capture were timed
	// from the connection opening this is the gap that would show up in it —
	// and it is exactly the gap that would push the statement outside the
	// request that ran it.
	idle := 30 * time.Millisecond
	time.Sleep(idle)

	asked := time.Now()
	conn.Write(pgMsg('Q', pgText("SELECT 1")))
	readUntilReady(conn)
	conn.Write(pgMsg('X'))
	conn.Close()

	// One capture, not two: the handshake does not get a row of its own.
	calls := settledCalls(t, rec, 1)
	if !bytes.Contains(calls[0].Request.Body, []byte("user")) {
		t.Error("the startup parameters were dropped instead of riding the first statement")
	}
	if calls[0].StartedAt.Before(asked) {
		t.Errorf("the capture starts at %v, %v before the statement was sent: it is timed from the connection",
			calls[0].StartedAt, asked.Sub(calls[0].StartedAt))
	}
	if calls[0].Duration >= idle {
		t.Errorf("the statement took %v, which is the idle time before it, not the statement", calls[0].Duration)
	}
}

// A connection that only ever authenticated still happened, and a debugger does
// not get to silently drop the fact that an authentication took place.
func TestAConnectionThatRanNothingIsStillRecorded(t *testing.T) {
	rec := &collector{}
	addr := servePostgresOnce(t, startScriptedPostgres(t, pgBackend), rec, 64<<10)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(pgStartup("user", "app", "database", "orders"))
	readUntilReady(conn)
	conn.Close()

	call := waitForCall(t, rec)
	if call.Method != postgresMethod {
		t.Errorf("method = %q, want the connection-level one: nothing ran", call.Method)
	}
	if call.Error != "" {
		t.Errorf("error = %q, want none: nothing failed", call.Error)
	}
	if !bytes.Contains(call.Response.Body, []byte{'R'}) {
		t.Error("the authentication is not in the capture")
	}
}

// A statement that never finished must be recorded and said to be incomplete.
// Reporting it as a success would be a lie, and dropping it would lose exactly
// the statement someone came looking for.
func TestAStatementCutShortSaysSo(t *testing.T) {
	silent := func(typ byte, body []byte) []byte {
		if typ == 0 {
			return pgBackend(0, body)
		}
		return nil // the query is taken and never answered
	}
	rec := &collector{}
	addr := servePostgresOnce(t, startScriptedPostgres(t, silent), rec, 64<<10)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(pgStartup("user", "app", "database", "orders"))
	readUntilReady(conn)
	conn.Write(pgMsg('Q', pgText("SELECT pg_sleep(600)")))
	conn.Close()

	call := waitForCall(t, rec)
	if call.Error != statementCutShort {
		t.Errorf("error = %q, want the statement reported as incomplete", call.Error)
	}
	if call.Method != postgresStatementMethod {
		t.Errorf("method = %q, want a statement", call.Method)
	}
	if !bytes.Contains(call.Request.Body, []byte("pg_sleep")) {
		t.Error("the statement that never finished was lost")
	}
}

// A COPY answers one statement with a conversation of its own. The rows the
// client streams belong to the statement that opened it, not to a capture of
// their own.
func TestACopyStaysWithTheStatementThatOpenedIt(t *testing.T) {
	backend := func(typ byte, body []byte) []byte {
		switch typ {
		case 0:
			return pgBackend(0, body)
		case 'Q':
			return pgMsg('G', []byte{0}, pgU16(0)) // CopyInResponse, text format
		case 'c':
			return pgCat(pgMsg('C', pgText("COPY 2")), pgMsg('Z', []byte{'I'}))
		}
		return nil
	}
	rec := &collector{}
	addr := servePostgresOnce(t, startScriptedPostgres(t, backend), rec, 64<<10)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(pgStartup("user", "app", "database", "orders"))
	readUntilReady(conn)
	conn.Write(pgMsg('Q', pgText("COPY orders FROM STDIN")))
	// The CopyInResponse, read so the copy really is under way before the rows
	// go out.
	head := make([]byte, 5)
	io.ReadFull(conn, head)
	io.ReadFull(conn, make([]byte, int(head[4])-4))
	conn.Write(pgMsg('d', []byte("1,10\n2,20\n")))
	conn.Write(pgMsg('c'))
	readUntilReady(conn)
	conn.Write(pgMsg('X'))
	conn.Close()

	// A copy is one statement, however many messages it takes.
	calls := settledCalls(t, rec, 1)
	for _, want := range []string{"COPY orders FROM STDIN", "1,10"} {
		if !bytes.Contains(calls[0].Request.Body, []byte(want)) {
			t.Errorf("the capture does not hold %q", want)
		}
	}
}

// The reason per-statement captures exist, proved end to end rather than
// asserted in a comment: the SQL a request ran hangs under that request.
//
// Nothing new correlates them. A statement that begins no earlier and ends no
// later than the HTTP call is contained in it, and internal/trace already hangs
// a contained call off its container — which only works because each capture
// now carries the timing of one statement instead of one connection.
func TestTheStatementHangsUnderTheRequestThatRanIt(t *testing.T) {
	// A query that takes a measurable moment. Two statements that begin and end
	// on the same clock tick contain each other as far as any timing rule can
	// tell, and the tree would stack them instead of setting them side by side.
	slow := func(typ byte, body []byte) []byte {
		if typ == 'Q' {
			time.Sleep(5 * time.Millisecond)
		}
		return pgBackend(typ, body)
	}

	rec := &collector{}
	db := servePostgresOnce(t, startScriptedPostgres(t, slow), rec, 64<<10)

	// The service under observation: it answers a request by running its
	// queries through Sonda's database listener.
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := net.Dial("tcp", db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer conn.Close()
		conn.Write(pgStartup("user", "app", "database", "orders"))
		readUntilReady(conn)
		// Two of them, which is the shape of the problem: an N+1 is worth
		// nothing as one row saying the connection ran some SQL.
		for _, sql := range []string{"SELECT id FROM orders", "SELECT name FROM customers WHERE id = 1"} {
			conn.Write(pgMsg('Q', pgText(sql)))
			if err := readUntilReady(conn); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		conn.Write(pgMsg('X'))
		w.Write([]byte(`{"orders":[]}`))
	}))
	defer service.Close()

	front := newProxy(t, service, 64<<10, rec)
	resp, err := http.Get(front.URL + "/orders")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	calls := waitForCalls(t, rec, 3)

	// The ids are handed out here: these captures never reached the database,
	// and three zeroes would make the tree hang a call off itself.
	var forTree []trace.Call
	var statements []int64
	var httpID int64
	for i, c := range calls {
		id := int64(i + 1)
		forTree = append(forTree, trace.Call{
			ID: id, Target: c.Target, Method: c.Method, Path: c.Path,
			Started: c.StartedAt, Duration: c.Duration,
		})
		if c.Protocol == config.ProtocolPostgres {
			statements = append(statements, id)
		} else {
			httpID = id
		}
	}
	if len(statements) != 2 {
		t.Fatalf("%d statements captured, want 2", len(statements))
	}

	trees := trace.Build(forTree)
	if len(trees) != 1 {
		t.Fatalf("%d trees, want one request with its SQL under it", len(trees))
	}
	root := trees[0].Root
	if root.Call.ID != httpID {
		t.Fatalf("the root is call %d, want the HTTP request %d", root.Call.ID, httpID)
	}
	if len(root.Children) != 2 {
		t.Fatalf("the request has %d children, want both statements under it: %+v",
			len(root.Children), root.Children)
	}
	under := map[int64]bool{}
	for _, child := range root.Children {
		under[child.Call.ID] = true
		// Grouped by timing, not by a trace id, and the tree has to say so.
		if !child.Inferred {
			t.Error("an inferred link was presented as a fact")
		}
	}
	for _, id := range statements {
		if !under[id] {
			t.Errorf("statement %d is not under the request that ran it", id)
		}
	}
}

func waitForCall(t *testing.T, rec *collector) *store.Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.calls)
		rec.mu.Unlock()
		if n > 0 {
			return rec.only(t)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no call was recorded")
	return nil
}

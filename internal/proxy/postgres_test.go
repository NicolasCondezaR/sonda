package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
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

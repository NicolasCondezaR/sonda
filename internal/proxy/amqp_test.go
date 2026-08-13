package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/amqpwire"
	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

const amqpPassword = "rabbit-password-must-never-be-stored"

func wireAMQPFrame(typ byte, channel uint16, parts ...[]byte) []byte {
	payload := bytes.Join(parts, nil)
	out := []byte{typ}
	out = binary.BigEndian.AppendUint16(out, channel)
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	return append(out, amqpFrameEnd)
}

func wireAMQPMethod(channel, class, method uint16, args ...[]byte) []byte {
	parts := [][]byte{binary.BigEndian.AppendUint16(nil, class), binary.BigEndian.AppendUint16(nil, method)}
	return wireAMQPFrame(1, channel, append(parts, args...)...)
}

func wireAMQPShort(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func wireAMQPLong(s string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(s))), s...)
}

func wireAMQPContentHeader(channel uint16, size uint64) []byte {
	return wireAMQPFrame(2, channel,
		binary.BigEndian.AppendUint16(nil, 60),
		binary.BigEndian.AppendUint16(nil, 0),
		binary.BigEndian.AppendUint64(nil, size),
		binary.BigEndian.AppendUint16(nil, 0),
	)
}

type fakeAMQPBroker struct {
	addr     string
	received chan []byte
	release  chan struct{}
}

func startFakeAMQPBroker(t *testing.T, want int, reply []byte) *fakeAMQPBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	b := &fakeAMQPBroker{addr: ln.Addr().String(), received: make(chan []byte, 1), release: make(chan struct{})}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		got := make([]byte, want)
		if _, err := io.ReadFull(conn, got); err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
		b.received <- got
		<-b.release
	}()
	return b
}

func serveAMQPOnce(t *testing.T, upstream string, rec Recorder, maxBody int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := New(config.Target{
		Name: "rabbit", Upstream: "amqp://" + upstream, Protocol: config.ProtocolAMQP,
	}, maxBody, rec, nil, nil)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			p.ServeAMQP(conn)
		}
	}()
	return ln.Addr().String()
}

func TestAMQPForwardsExactBytesAndCapturesUsefulUnitsWhileOpen(t *testing.T) {
	protocolHeader := []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}
	startOK := wireAMQPMethod(0, 10, 11,
		[]byte{0, 0, 0, 0},
		wireAMQPShort("PLAIN"),
		wireAMQPLong("\x00guest\x00"+amqpPassword),
		wireAMQPShort("en_US"),
	)
	payload := []byte(`{"order_id":417}`)
	publish := bytes.Join([][]byte{
		wireAMQPMethod(1, 60, 40,
			[]byte{0, 0}, wireAMQPShort(""), wireAMQPShort("orders.created"), []byte{0}),
		wireAMQPContentHeader(1, uint64(len(payload))),
		wireAMQPFrame(3, 1, payload),
	}, nil)
	sent := bytes.Join([][]byte{protocolHeader, startOK, publish}, nil)
	serverClose := wireAMQPMethod(1, 20, 40,
		binary.BigEndian.AppendUint16(nil, 406),
		wireAMQPShort("PRECONDITION_FAILED"),
		binary.BigEndian.AppendUint16(nil, 50),
		binary.BigEndian.AppendUint16(nil, 10),
	)

	broker := startFakeAMQPBroker(t, len(sent), serverClose)
	defer close(broker.release)
	rec := &collector{}
	addr := serveAMQPOnce(t, broker.addr, rec, 1<<20)

	client, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Split inside the protocol header, SASL response and content body. A tap
	// that assumes one network read is one frame will fail this conversation.
	for _, part := range [][]byte{sent[:3], sent[3:19], sent[19 : len(sent)-5], sent[len(sent)-5:]} {
		if _, err := client.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	reply := make([]byte, len(serverClose))
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reply, serverClose) {
		t.Fatalf("client received %x, want broker reply %x", reply, serverClose)
	}

	select {
	case got := <-broker.received:
		if !bytes.Equal(got, sent) {
			t.Fatalf("broker received changed bytes\n got %x\nwant %x", got, sent)
		}
		if !bytes.Contains(got, []byte(amqpPassword)) {
			t.Fatal("the forwarding assertion did not include the live SASL credential")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the broker never received the client stream")
	}

	// The broker and client are still connected. Captures must already be
	// visible; waiting for a pooled RabbitMQ connection to close can mean hours.
	calls := waitForCalls(t, rec, 4)
	for _, call := range calls {
		if call.Protocol != config.ProtocolAMQP {
			t.Errorf("protocol = %q", call.Protocol)
		}
		if bytes.Contains(call.Request.Body, []byte(amqpPassword)) || bytes.Contains(call.Response.Body, []byte(amqpPassword)) {
			t.Errorf("capture %s stored the broker password", call.Method)
		}
	}

	var start, message, failure bool
	for _, call := range calls {
		switch call.Method {
		case "connection.start-ok":
			start = true
			frames, rest := amqpwire.Deframe(call.Request.Body, true)
			if rest != 0 || len(frames) != 1 || frames[0].Mechanism != "PLAIN" {
				t.Errorf("the safe handshake no longer names its mechanism: frames=%+v rest=%d", frames, rest)
			}
		case "basic.publish":
			message = true
			frames, rest := amqpwire.Deframe(call.Request.Body, true)
			if rest != 0 || len(frames) != 3 {
				t.Fatalf("publish capture = %d frames, %d remainder", len(frames), rest)
			}
			if call.Path != "(default) -> orders.created" || frames[2].Text != string(payload) {
				t.Errorf("publish path=%q body=%q", call.Path, frames[2].Text)
			}
		case "channel.close":
			failure = true
			if call.Error == "" {
				t.Error("a 406 channel.close was recorded as a success")
			}
		}
	}
	if !start || !message || !failure {
		t.Fatalf("captures did not expose handshake=%v publish=%v failure=%v", start, message, failure)
	}

	dbPath := filepath.Join(t.TempDir(), "sonda.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if _, err := db.Insert(context.Background(), call); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	found, err := db.List(context.Background(), store.Filter{Protocol: config.ProtocolAMQP, Search: "order_id"})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Method != "basic.publish" {
		db.Close()
		t.Fatalf("AMQP text search = %+v", found)
	}
	byMethod, err := db.List(context.Background(), store.Filter{Protocol: config.ProtocolAMQP, Method: "BASIC.PUBLISH"})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if len(byMethod) != 1 || byMethod[0].Path != "(default) -> orders.created" {
		db.Close()
		t.Fatalf("AMQP method filter = %+v", byMethod)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte(amqpPassword)) {
		t.Fatal("the SQLite capture file contains the AMQP password")
	}

}

func TestAnIncompleteAMQPCredentialFrameStoresNoPartialSecret(t *testing.T) {
	frame := wireAMQPMethod(0, 10, 11,
		[]byte{0, 0, 0, 0}, wireAMQPShort("PLAIN"),
		wireAMQPLong("\x00guest\x00"+amqpPassword), wireAMQPShort("en_US"),
	)
	cut := frame[:len(frame)-1] // no frame-end marker, but the credential is present
	rec := &collector{}
	p := New(config.Target{Name: "rabbit", Upstream: "amqp://127.0.0.1:5672", Protocol: config.ProtocolAMQP}, 1<<20, rec, nil, nil)
	session := newAMQPSession(p, "127.0.0.1:1", false)
	tap := newAMQPTap(session, true)
	if _, err := tap.Write(cut); err != nil {
		t.Fatal(err)
	}
	tap.flush(time.Now())

	call := rec.only(t)
	if call.Error == "" || call.Request.Size != int64(len(cut)) {
		t.Fatalf("incomplete capture = error %q, size %d", call.Error, call.Request.Size)
	}
	if len(call.Request.Body) != 0 || bytes.Contains(call.Request.Body, []byte(amqpPassword)) {
		t.Fatalf("partial credential bytes were stored: %x", call.Request.Body)
	}
}

func TestMalformedAMQPCredentialLengthsFailClosedBeforePersistence(t *testing.T) {
	secret := []byte("malformed-length-secret-marker")
	malformedLong := func(value []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(value)+64))
		return append(out, value...)
	}
	tests := []struct {
		name     string
		methodID uint16
		args     [][]byte
	}{
		{
			name:     "start-ok response",
			methodID: 11,
			args:     [][]byte{{0, 0, 0, 0}, wireAMQPShort("PLAIN"), malformedLong(secret), wireAMQPShort("en_US")},
		},
		{name: "secure challenge", methodID: 20, args: [][]byte{malformedLong(secret)}},
		{name: "secure-ok response", methodID: 21, args: [][]byte{malformedLong(secret)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := wireAMQPMethod(0, 10, tt.methodID, tt.args...)
			forwarded := append([]byte(nil), frame...)
			rec := &collector{}
			p := New(config.Target{Name: "rabbit", Upstream: "amqp://127.0.0.1:5672", Protocol: config.ProtocolAMQP}, 1<<20, rec, nil, nil)
			tap := newAMQPTap(newAMQPSession(p, "127.0.0.1:1", false), true)
			if _, err := tap.Write(frame); err != nil {
				t.Fatal(err)
			}

			call := rec.only(t)
			if bytes.Contains(call.Request.Body, secret) {
				t.Fatalf("raw capture leaked the marker: %x", call.Request.Body)
			}
			if !bytes.Equal(frame, forwarded) {
				t.Fatal("capture sanitization mutated the bytes reserved for forwarding")
			}

			dbPath := filepath.Join(t.TempDir(), "sonda.db")
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.Insert(context.Background(), call)
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			persisted, err := db.Get(context.Background(), id)
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if bytes.Contains(persisted.Request.Body, secret) {
				db.Close()
				t.Fatalf("SQLite raw capture leaked the marker: %x", persisted.Request.Body)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			onDisk, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(onDisk, secret) {
				t.Fatal("SQLite file contains the malformed SASL marker")
			}
		})
	}
}

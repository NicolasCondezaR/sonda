package proxy

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/wsframe"
)

// The handshake and the frames are written by hand here rather than pulled from
// a library. A test that speaks the protocol the way a browser does is the only
// one that proves Sonda relays it untouched — and it keeps the dependency count
// where it is.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func accept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// echoSocket is an upstream that completes the handshake and echoes every text
// frame back, then closes when the client closes.
func echoSocket(t *testing.T) string {
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
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if !isWebSocketUpgrade(req.Header) {
					io.WriteString(conn, "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\n\r\n")
					return
				}
				io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
					"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
					"Sec-WebSocket-Accept: "+accept(req.Header.Get("Sec-WebSocket-Key"))+"\r\n\r\n")

				// Echo whole frames back, unmasked, as a server must.
				buf := make([]byte, 4096)
				var pending []byte
				for {
					n, err := br.Read(buf)
					if n > 0 {
						pending = append(pending, buf[:n]...)
						frames, rest := wsframe.Deframe(pending)
						pending = pending[len(pending)-rest:]
						for _, f := range frames {
							if f.Opcode == wsframe.OpClose {
								conn.Write(serverFrame(wsframe.OpClose, f.Payload))
								return
							}
							conn.Write(serverFrame(f.Opcode, f.Payload))
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func serverFrame(opcode int, payload []byte) []byte {
	out := []byte{byte(0x80 | opcode)}
	switch n := len(payload); {
	case n < 126:
		out = append(out, byte(n))
	default:
		out = append(out, 126)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	}
	return append(out, payload...)
}

func clientFrame(opcode int, payload []byte) []byte {
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	out := []byte{byte(0x80 | opcode), byte(0x80 | len(payload))}
	out = append(out, key[:]...)
	for i, b := range payload {
		out = append(out, b^key[i%4])
	}
	return out
}

// dialThrough completes a handshake against Sonda and hands back the raw
// connection, which is what a browser holds once the socket is open.
func dialThrough(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: sonda\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake got %d, want 101", resp.StatusCode)
	}
	// The upstream's own accept token has to arrive untouched, or the client is
	// negotiating with Sonda instead of with the service.
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != accept("dGhlIHNhbXBsZSBub25jZQ==") {
		t.Errorf("Sec-WebSocket-Accept = %q, want the upstream's own", got)
	}
	return conn, br
}

func socketProxy(t *testing.T, upstream string) (*httptest.Server, *captured) {
	t.Helper()
	rec := &captured{}
	target := config.Target{
		Name: "realtime", Listen: "127.0.0.1:0",
		Upstream: "http://" + upstream, Protocol: config.ProtocolHTTP,
	}
	front := httptest.NewServer(New(target, 1<<20, rec, nil, nil))
	t.Cleanup(front.Close)
	return front, rec
}

// waitForCapture gives the recording goroutine a moment. The call is written
// when the socket closes, not while it is open.
func waitForCapture(t *testing.T, rec *captured) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if rec.last() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no capture arrived after the socket closed")
}

func TestAWholeSocketConversationIsCaptured(t *testing.T) {
	front, rec := socketProxy(t, echoSocket(t))
	conn, br := dialThrough(t, strings.TrimPrefix(front.URL, "http://"))

	conn.Write(clientFrame(wsframe.OpText, []byte(`{"subscribe":"rates"}`)))
	conn.Write(clientFrame(wsframe.OpText, []byte(`{"subscribe":"orders"}`)))

	// Drain the echoes so the conversation really happened before it is closed.
	got := make([]byte, 0, 256)
	for len(got) < 2 {
		buf := make([]byte, 256)
		n, err := br.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			if frames, _ := wsframe.Deframe(got); len(frames) >= 2 {
				break
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	conn.Write(clientFrame(wsframe.OpClose, nil))
	conn.Close()
	waitForCapture(t, rec)

	call := rec.last()
	if call.Protocol != config.ProtocolWebSocket {
		t.Errorf("protocol = %q, want websocket", call.Protocol)
	}
	if call.Status != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want 101", call.Status)
	}
	if call.Path != "/socket" {
		t.Errorf("path = %q", call.Path)
	}

	// Both directions, read back out of the stored stream exactly the way the
	// inspector will read them.
	sent, _ := wsframe.Deframe(call.Request.Body)
	if len(sent) < 3 {
		t.Fatalf("%d frames captured from the client, want the two messages and the close", len(sent))
	}
	if sent[0].Text != `{"subscribe":"rates"}` {
		t.Errorf("first client frame = %q", sent[0].Text)
	}
	if !sent[0].Masked {
		t.Error("a client frame was recorded as unmasked")
	}

	received, _ := wsframe.Deframe(call.Response.Body)
	if len(received) < 2 {
		t.Fatalf("%d frames captured from the upstream", len(received))
	}
	if received[0].Text != `{"subscribe":"rates"}` {
		t.Errorf("first server frame = %q", received[0].Text)
	}
	if received[0].Masked {
		t.Error("a server frame must never be masked")
	}
}

// The handshake carries the trace id like any other request, so a socket
// belongs to the request that opened it.
func TestASocketKeepsItsTraceID(t *testing.T) {
	front, rec := socketProxy(t, echoSocket(t))
	addr := strings.TrimPrefix(front.URL, "http://")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: sonda\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"X-Request-Id: req-77\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitForCapture(t, rec)

	if got := rec.last().TraceID; got != "req-77" {
		t.Errorf("TraceID = %q, want req-77", got)
	}
}

// An upgrade the service refuses is an ordinary response, and relaying it as
// one is what lets the client read why.
func TestARefusedUpgradeIsRelayedAsAResponse(t *testing.T) {
	refuser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sockets are off", http.StatusForbidden)
	}))
	defer refuser.Close()

	front, rec := socketProxy(t, strings.TrimPrefix(refuser.URL, "http://"))
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: sonda\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want the upstream's 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "sockets are off") {
		t.Errorf("the reason did not reach the client: %q", body)
	}
	waitForCapture(t, rec)
	if got := rec.last().Status; got != http.StatusForbidden {
		t.Errorf("the capture recorded status %d", got)
	}
}

// Ordinary traffic must not go anywhere near this path.
func TestAPlainRequestIsNotTreatedAsASocket(t *testing.T) {
	for _, h := range []http.Header{
		{},
		{"Upgrade": []string{"h2c"}, "Connection": []string{"Upgrade"}},
		{"Connection": []string{"keep-alive"}},
		{"Upgrade": []string{"websocket"}}, // no Connection token
	} {
		if isWebSocketUpgrade(h) {
			t.Errorf("%v was read as a socket upgrade", h)
		}
	}

	// And the spellings that really do arrive from clients.
	for _, h := range []http.Header{
		{"Upgrade": []string{"websocket"}, "Connection": []string{"Upgrade"}},
		{"Upgrade": []string{"WebSocket"}, "Connection": []string{"keep-alive, Upgrade"}},
		{"Upgrade": []string{"websocket"}, "Connection": []string{"upgrade"}},
	} {
		if !isWebSocketUpgrade(h) {
			t.Errorf("%v was not read as a socket upgrade", h)
		}
	}
}

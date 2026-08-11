package ws

import (
	"bufio"
	"net"
	"testing"
)

// The frame codec is the only thing in the test bed that could be subtly wrong
// without anything failing loudly: a bad mask or a bad length produces frames
// that a browser would reject and that Sonda would faithfully record as
// garbage. One round trip in each direction is enough to catch that.
func TestFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	client := &Conn{rw: rw(a), c: a, client: true}
	server := &Conn{rw: rw(b), c: b}

	go func() {
		if err := client.Text("hello"); err != nil {
			t.Error(err)
		}
		if err := client.frame(OpClose, closePayload(CloseInternal, "shelf gone")); err != nil {
			t.Error(err)
		}
	}()

	op, payload, err := server.Read()
	if err != nil {
		t.Fatal(err)
	}
	if op != OpText || string(payload) != "hello" {
		t.Fatalf("got op %d %q, want a text frame saying hello", op, payload)
	}

	op, payload, err = server.Read()
	if err != nil {
		t.Fatal(err)
	}
	if op != OpClose {
		t.Fatalf("got op %d, want a close frame", op)
	}
	code, reason := CloseCode(payload)
	if code != CloseInternal || reason != "shelf gone" {
		t.Fatalf("got close %d %q, want 1011 \"shelf gone\"", code, reason)
	}
}

func closePayload(code int, reason string) []byte {
	out := make([]byte, 2+len(reason))
	out[0] = byte(code >> 8)
	out[1] = byte(code)
	copy(out[2:], reason)
	return out
}

func rw(c net.Conn) *bufio.ReadWriter {
	return bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
}

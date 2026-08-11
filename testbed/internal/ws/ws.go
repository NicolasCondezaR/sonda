// Package ws is a WebSocket just large enough to hold a short conversation.
//
// It is hand-rolled rather than taken from a library for one reason: the test
// bed has to close a socket with a specific code *and* a reason, because that
// pair is what Sonda shows you when a socket stops working and it is the whole
// point of the WebSocket exercise. The convenience wrappers hide the reason.
//
// Everything a real implementation needs and this does not have — continuation
// frames, fragmentation, permessage-deflate, ping timers — is missing on
// purpose. Both ends of every conversation here are in this repository.
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// The constant from RFC 6455, appended to the client key before hashing.
const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes. Only the ones a text conversation uses.
const (
	OpText  = 0x1
	OpClose = 0x8
	OpPing  = 0x9
	OpPong  = 0xA
)

// Conn is one side of an open socket.
type Conn struct {
	rw     *bufio.ReadWriter
	c      net.Conn
	client bool // clients must mask what they send; servers must not
}

// Close codes used by the test bed. 1000 is a clean goodbye; 1011 is the server
// admitting it failed, which is the one worth looking at in a capture.
const (
	CloseNormal   = 1000
	CloseInternal = 1011
)

// Accept completes the handshake and takes the connection off net/http.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("not a websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("no Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("connection cannot be hijacked")
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept(key) + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		c.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		c.Close()
		return nil, err
	}
	return &Conn{rw: rw, c: c}, nil
}

// Dial opens a socket. url is http:// — the scheme a WebSocket handshake is
// actually made over, which is also the address Sonda is proxying.
func Dial(url string) (*Conn, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(raw[:])
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")

	host := req.URL.Host
	if req.URL.Port() == "" {
		host += ":80"
	}
	c, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
	resp, err := http.ReadResponse(rw.Reader, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("upgrade refused: %s", resp.Status)
	}
	return &Conn{rw: rw, c: c, client: true}, nil
}

func accept(key string) string {
	sum := sha1.Sum([]byte(key + magic))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Text sends one text frame.
func (w *Conn) Text(s string) error { return w.frame(OpText, []byte(s)) }

// CloseWith sends a close frame carrying a code and a reason, then hangs up.
func (w *Conn) CloseWith(code int, reason string) error {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	err := w.frame(OpClose, payload)
	w.c.Close()
	return err
}

// Read returns the next frame's opcode and payload.
func (w *Conn) Read() (op int, payload []byte, err error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(w.rw, h); err != nil {
		return 0, nil, err
	}
	op = int(h[0] & 0x0F)
	masked := h[1]&0x80 != 0

	length := uint64(h[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.rw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.rw, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	// A toy that trusts a length field is a toy that can be made to allocate a
	// gigabyte by one bad frame.
	if length > 1<<20 {
		return 0, nil, errors.New("frame too large")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.rw, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(w.rw, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

// CloseCode reads a close frame's payload back as its code and reason.
func CloseCode(payload []byte) (int, string) {
	if len(payload) < 2 {
		return 0, ""
	}
	return int(binary.BigEndian.Uint16(payload[:2])), string(payload[2:])
}

func (w *Conn) frame(op int, payload []byte) error {
	head := []byte{byte(0x80 | op)} // FIN set: every frame here is whole

	n := len(payload)
	maskBit := byte(0)
	if w.client {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		head = append(head, maskBit|byte(n))
	case n < 1<<16:
		head = append(head, maskBit|126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(head, maskBit|127)
		head = append(head, ext[:]...)
	}

	body := payload
	if w.client {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		head = append(head, mask[:]...)
		body = make([]byte, n)
		for i := range payload {
			body[i] = payload[i] ^ mask[i%4]
		}
	}
	if _, err := w.rw.Write(head); err != nil {
		return err
	}
	if _, err := w.rw.Write(body); err != nil {
		return err
	}
	return w.rw.Flush()
}

// Hangup drops the connection without a close frame.
func (w *Conn) Hangup() { w.c.Close() }

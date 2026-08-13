package proxy

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/amqpwire"
	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

const (
	// RabbitMQ defaults frame-max to 128 KiB. A frame above this limit is still
	// forwarded, but the tap refuses to retain it while waiting for its end: a
	// corrupt size field must not turn the debugger into an unbounded buffer.
	maxAMQPCaptureFrame = 16 << 20

	amqpConnectionMethod = "CONNECTION"
	amqpFrameMethod      = "FRAME"
)

const (
	amqpFrameHeaderSize    = 7
	amqpProtocolHeaderSize = 8
	amqpFrameEnd           = 0xCE
)

// ServeAMQP forwards one AMQP 0-9-1 connection byte-for-byte and records
// useful units while the connection remains open.
//
// One RabbitMQ connection commonly lives for hours and multiplexes many
// channels. Waiting for it to close would make a publish invisible while it is
// being debugged, and one row per frame would split a method from its content.
// The taps therefore emit standalone methods immediately and group each
// content-bearing method with its following header and body frames per channel.
func (p *Proxy) ServeAMQP(client net.Conn) {
	defer client.Close()
	started := time.Now()

	upstream, err := p.dialUpstream()
	if err != nil {
		p.recorder.Record(&store.Call{
			Target:           p.target.Name,
			Protocol:         config.ProtocolAMQP,
			Method:           amqpConnectionMethod,
			Path:             "connection",
			ClientAddr:       remoteAddr(client),
			StartedAt:        started,
			Duration:         time.Since(started),
			Error:            fmt.Sprintf("could not reach %s: %v", p.target.Upstream, err),
			TLS:              isTLSConn(client),
			UpstreamTLS:      p.upstreamTLS,
			UpstreamInsecure: p.upstreamTLS && p.target.InsecureSkipVerify,
		})
		return
	}
	defer upstream.Close()

	session := newAMQPSession(p, remoteAddr(client), isTLSConn(client))
	sent := newAMQPTap(session, true)
	received := newAMQPTap(session, false)

	// The tap runs before the destination and copies every frame it keeps.
	// Credential blanking can therefore never touch the buffer forwarded to the
	// broker, even when the two writes happen from the same io.Copy read.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(sent, upstream), client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(received, client), upstream)
		closeWrite(client)
	}()
	wg.Wait()
	ended := time.Now()
	sent.flush(ended)
	received.flush(ended)
	session.flush(ended)
}

func remoteAddr(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return ""
	}
	return c.RemoteAddr().String()
}

func isTLSConn(c net.Conn) bool {
	_, ok := c.(*tls.Conn)
	return ok
}

type amqpSegment struct {
	head []byte
	size int64
}

func (s *amqpSegment) add(b []byte, limit int64) {
	s.size += int64(len(b))
	if room := limit - int64(len(s.head)); room > 0 {
		if int64(len(b)) > room {
			b = b[:room]
		}
		s.head = append(s.head, b...)
	}
}

func (s amqpSegment) message() store.Message {
	return store.Message{Body: s.head, Size: s.size, Truncated: s.size > int64(len(s.head))}
}

type amqpUnit struct {
	started time.Time
	primary amqpwire.Frame
	frames  []amqpwire.Frame
	raw     amqpSegment

	expectedBody int64
	bodyBytes    int64
}

func (u *amqpUnit) add(raw []byte, frame amqpwire.Frame, limit int64) {
	u.raw.add(raw, limit)
	u.frames = append(u.frames, frame)
}

type amqpSession struct {
	proxy      *Proxy
	client     string
	clientTLS  bool
	mu         sync.Mutex
	pendingOut map[uint16]*amqpUnit
	pendingIn  map[uint16]*amqpUnit
}

func newAMQPSession(p *Proxy, client string, clientTLS bool) *amqpSession {
	return &amqpSession{
		proxy: p, client: client, clientTLS: clientTLS,
		pendingOut: map[uint16]*amqpUnit{}, pendingIn: map[uint16]*amqpUnit{},
	}
}

func (s *amqpSession) accept(raw []byte, frame amqpwire.Frame, fromClient bool, now time.Time) {
	if frame.Kind == "heartbeat" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending(fromClient)

	switch {
	case contentMethod(frame.Kind):
		if old := pending[frame.Channel]; old != nil {
			s.emit(old, fromClient, now, "another method arrived before the content body finished")
		}
		u := &amqpUnit{started: now, primary: frame}
		u.add(raw, frame, s.proxy.maxBody)
		pending[frame.Channel] = u

	case frame.Kind == "content_header":
		u := pending[frame.Channel]
		if u == nil {
			s.emit(singleAMQPUnit(raw, frame, now, s.proxy.maxBody), fromClient, now, "content header arrived without a content-bearing method")
			return
		}
		u.add(raw, frame, s.proxy.maxBody)
		u.expectedBody = frame.BodySize
		if frame.BodySize == 0 {
			delete(pending, frame.Channel)
			s.emit(u, fromClient, now, "")
		}

	case frame.Kind == "content_body":
		u := pending[frame.Channel]
		if u == nil {
			s.emit(singleAMQPUnit(raw, frame, now, s.proxy.maxBody), fromClient, now, "content body arrived without a content-bearing method")
			return
		}
		u.add(raw, frame, s.proxy.maxBody)
		u.bodyBytes += frame.Size
		if u.expectedBody > 0 && u.bodyBytes >= u.expectedBody {
			delete(pending, frame.Channel)
			s.emit(u, fromClient, now, "")
		}

	default:
		if old := pending[frame.Channel]; old != nil {
			delete(pending, frame.Channel)
			s.emit(old, fromClient, now, "another method arrived before the content body finished")
		}
		s.emit(singleAMQPUnit(raw, frame, now, s.proxy.maxBody), fromClient, now, "")
	}
}

func singleAMQPUnit(raw []byte, frame amqpwire.Frame, now time.Time, limit int64) *amqpUnit {
	u := &amqpUnit{started: now, primary: frame}
	u.add(raw, frame, limit)
	return u
}

func contentMethod(kind string) bool {
	switch kind {
	case "basic.publish", "basic.return", "basic.deliver", "basic.get-ok":
		return true
	}
	return false
}

func (s *amqpSession) pending(fromClient bool) map[uint16]*amqpUnit {
	if fromClient {
		return s.pendingOut
	}
	return s.pendingIn
}

func (s *amqpSession) flush(end time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, direction := range []struct {
		pending    map[uint16]*amqpUnit
		fromClient bool
	}{{s.pendingOut, true}, {s.pendingIn, false}} {
		for channel, unit := range direction.pending {
			delete(direction.pending, channel)
			s.emit(unit, direction.fromClient, end, "the connection ended before the content body finished")
		}
	}
}

func (s *amqpSession) oversized(fromClient bool, size int64, at time.Time) {
	s.unreadable(fromClient, size, at,
		fmt.Sprintf("AMQP frame is %d bytes, above the %d-byte capture parse limit", size, maxAMQPCaptureFrame))
}

func (s *amqpSession) unreadable(fromClient bool, size int64, at time.Time, failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &amqpUnit{
		started: at,
		primary: amqpwire.Frame{Type: "unknown", Kind: amqpFrameMethod, Size: size, FromClient: fromClient},
		raw:     amqpSegment{size: size},
	}
	s.emit(u, fromClient, at, failure)
}

// emit is called with s.mu held so captures from the two relay goroutines keep
// a deterministic order at the recorder boundary.
func (s *amqpSession) emit(unit *amqpUnit, fromClient bool, end time.Time, failure string) {
	if unit == nil {
		return
	}
	if end.Before(unit.started) {
		end = unit.started
	}
	if failure == "" {
		failure = amqpFailure(unit.frames)
	}
	call := &store.Call{
		Target:           s.proxy.target.Name,
		Protocol:         config.ProtocolAMQP,
		Method:           unit.primary.Kind,
		Path:             amqpPath(unit.primary),
		ClientAddr:       s.client,
		StartedAt:        unit.started,
		Duration:         end.Sub(unit.started),
		Error:            failure,
		TLS:              s.clientTLS,
		UpstreamTLS:      s.proxy.upstreamTLS,
		UpstreamInsecure: s.proxy.upstreamTLS && s.proxy.target.InsecureSkipVerify,
	}
	if fromClient {
		call.Request = unit.raw.message()
	} else {
		call.Response = unit.raw.message()
	}
	s.proxy.recorder.Record(call)
}

func amqpFailure(frames []amqpwire.Frame) string {
	for _, frame := range frames {
		if frame.ReplyCode < 300 {
			continue
		}
		out := fmt.Sprintf("%s %d", frame.Kind, frame.ReplyCode)
		if frame.ReplyText != "" {
			out += ": " + frame.ReplyText
		}
		if frame.Cause != "" {
			out += " (on " + frame.Cause + ")"
		}
		return out
	}
	return ""
}

func amqpPath(frame amqpwire.Frame) string {
	if frame.Exchange != "" || frame.RoutingKey != "" {
		exchange := frame.Exchange
		if exchange == "" {
			exchange = "(default)"
		}
		if frame.RoutingKey != "" {
			return exchange + " -> " + frame.RoutingKey
		}
		return exchange
	}
	for _, value := range []string{frame.Queue, frame.VirtualHost, frame.ConsumerTag} {
		if value != "" {
			return value
		}
	}
	if frame.Channel == 0 {
		return "connection"
	}
	return fmt.Sprintf("channel/%d", frame.Channel)
}

type amqpTap struct {
	session    *amqpSession
	fromClient bool
	buf        []byte
	discard    int64
}

func newAMQPTap(session *amqpSession, fromClient bool) *amqpTap {
	return &amqpTap{session: session, fromClient: fromClient}
}

func (t *amqpTap) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		if t.discard > 0 {
			n := int64(len(p))
			if n > t.discard {
				n = t.discard
			}
			p = p[n:]
			t.discard -= n
			continue
		}
		t.buf = append(t.buf, p...)
		p = nil
		t.consume()
	}
	return written, nil
}

func (t *amqpTap) consume() {
	for len(t.buf) > 0 {
		now := time.Now()
		if len(t.buf) >= 4 && string(t.buf[:4]) == "AMQP" {
			if len(t.buf) < amqpProtocolHeaderSize {
				return
			}
			t.capture(t.buf[:amqpProtocolHeaderSize], now)
			t.buf = t.buf[amqpProtocolHeaderSize:]
			continue
		}
		if len(t.buf) < amqpFrameHeaderSize+1 {
			return
		}
		total := int64(amqpFrameHeaderSize) + int64(binary.BigEndian.Uint32(t.buf[3:7])) + 1
		if total > maxAMQPCaptureFrame {
			available := int64(len(t.buf))
			if available > total {
				available = total
			}
			t.buf = t.buf[available:]
			t.discard = total - available
			t.session.oversized(t.fromClient, total, now)
			continue
		}
		if int64(len(t.buf)) < total {
			return
		}
		end := int(total)
		if t.buf[end-1] != amqpFrameEnd {
			t.session.unreadable(t.fromClient, total, now, "AMQP frame did not end with the required 0xCE marker")
			t.buf = t.buf[end:]
			continue
		}
		t.capture(t.buf[:end], now)
		t.buf = t.buf[end:]
	}
}

func (t *amqpTap) capture(raw []byte, now time.Time) {
	blanked := amqpwire.BlankCredentials(raw)
	frames, rest := amqpwire.Deframe(blanked, t.fromClient)
	if rest != 0 || len(frames) != 1 {
		t.session.unreadable(t.fromClient, int64(len(raw)), now, "AMQP frame could not be decoded")
		return
	}
	t.session.accept(blanked, frames[0], t.fromClient, now)
}

func (t *amqpTap) flush(at time.Time) {
	if t.discard > 0 {
		// The complete declared size was already reported when the limit was
		// crossed. Do not create a second row merely because the peer closed
		// before the tap finished discarding that one frame.
		t.buf = nil
		t.discard = 0
		return
	}
	if len(t.buf) == 0 && t.discard == 0 {
		return
	}
	size := int64(len(t.buf)) + t.discard
	t.buf = nil
	t.discard = 0
	// An incomplete method cannot be sanitized safely: its length fields may
	// themselves be the bytes that are missing. Recording only the size and the
	// failure preserves the evidence without risking a partial SASL response in
	// the capture file.
	t.session.unreadable(t.fromClient, size, at,
		"the connection ended in the middle of an AMQP frame; incomplete bytes were not stored because they could include credentials")
}

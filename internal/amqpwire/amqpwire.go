// Package amqpwire reads the AMQP 0-9-1 protocol, the one RabbitMQ speaks.
//
// It stands next to pgwire, wsframe and grpcwire and exists for the same
// reason: what crossed the wire is a stream of framed messages, Sonda stores
// the stream exactly as it arrived, and turning it back into messages is a view
// computed when someone looks. Nothing here re-serializes, so nothing here can
// lose the parts the parser did not understand.
//
// Every frame has the same shape:
//
//	+--------+-----------+-----------+-------------+-----------+
//	|  type  |  channel  |   size    |   payload   | frame-end |
//	| 1 byte |  uint16   |  uint32   |  size bytes |   0xCE    |
//	+--------+-----------+-----------+-------------+-----------+
//
// Two things make this different from the others. A connection opens with an
// eight-byte protocol header that is not a frame at all, and every frame after
// it carries a channel number: one TCP connection multiplexes many independent
// conversations, so a reader who ignores the channel is reading several
// unrelated exchanges interleaved as though they were one.
//
// Only the method table is partial. AMQP defines around sixty methods and most
// of them are handshake bookkeeping nobody debugs; the ones that carry a
// routing decision, a delivery, an acknowledgement or a failure are decoded,
// and the rest are named by class and method id with their size — a lesser view
// rather than a blank one.
package amqpwire

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	frameMethod    = 1
	frameHeader    = 2
	frameBody      = 3
	frameHeartbeat = 8

	// frameEnd terminates every frame. It is the protocol's own resynchronisation
	// check: if it is not there, the size field was not believable and the reader
	// is no longer at a frame boundary.
	frameEnd = 0xCE

	// frameHeaderSize is the type byte, the channel and the size.
	frameHeaderSize = 7

	// protocolHeaderSize is the literal "AMQP" plus four version bytes.
	protocolHeaderSize = 8

	// maxFrame refuses a size no real connection produces. The negotiated
	// frame-max is a uint32 and RabbitMQ's default is 128 KiB, so anything near
	// the top of the range means the stream is corrupt, mis-framed, or not this
	// protocol at all.
	//
	// The bounds check below would catch most of those anyway. What it would not
	// catch is the size near 2^32 that a corrupt stream produces: int is 32 bits
	// on a 32-bit build, the conversion wraps negative, and the "is the frame all
	// here" comparison then passes on a negative end and the slice panics.
	// Capping first means the conversion is always in range.
	maxFrame = 1 << 30

	// classBasic is the only class in 0-9-1 that carries content, and therefore
	// the only one whose content-header properties have a defined layout.
	classBasic = 60
)

// Frame is one frame as it crossed.
//
// The fields are flat and almost all optional rather than one type per method.
// This value is serialised straight into a view, and a single shape with absent
// fields is easier to consume than a union every client has to switch on. Only
// what a reader would act on is decoded; the rest of the payload stays in
// Payload.
type Frame struct {
	// Type is the frame type: method, header, body, heartbeat, or the protocol
	// header that opens a connection and is not a frame at all.
	Type string `json:"type"`

	// Kind is the human name for what this frame is — "basic.publish",
	// "content_header", "heartbeat". A method outside the table below is still
	// named, by its class and method id, because reporting "unknown(class 85,
	// method 10), 1 byte" is a lesser view and dropping the frame is a blank one.
	Kind string `json:"kind"`

	// Channel is the conversation this frame belongs to. Channel 0 is the
	// connection itself; everything else is one of several independent exchanges
	// sharing the socket.
	Channel uint16 `json:"channel"`

	// Size is the payload, excluding the seven-byte header and the end marker. It
	// is the one thing that can always be reported, whatever the payload turns
	// out to be.
	Size       int64 `json:"size"`
	FromClient bool  `json:"from_client"`

	// Payload aliases the captured bytes rather than copying them: decoding only
	// ever reads, so there is nothing to protect the caller from.
	Payload []byte `json:"-"`

	// Class and Method identify a method frame even when it is not in the table.
	Class  uint16 `json:"class,omitempty"`
	Method uint16 `json:"method,omitempty"`

	// Exchange and RoutingKey are the routing decision — where a message was sent
	// and where it came from. An absent Exchange on a publish or a delivery is
	// the default exchange, which is a real exchange and the most common one:
	// publishing to "" with the routing key set to a queue name is how almost
	// every tutorial sends its first message.
	Exchange   string `json:"exchange,omitempty"`
	RoutingKey string `json:"routing_key,omitempty"`
	Queue      string `json:"queue,omitempty"`

	// ConsumerTag tells apart the deliveries of several consumers on one channel.
	ConsumerTag string `json:"consumer_tag,omitempty"`

	// DeliveryTag is what an ack, a nack or a reject refers back to. It is scoped
	// to the channel, which is the other reason Channel is on every frame.
	DeliveryTag uint64 `json:"delivery_tag,omitempty"`

	// Redelivered marks a message the broker has handed out before — the first
	// thing to look at when a consumer keeps seeing the same message.
	Redelivered bool `json:"redelivered,omitempty"`

	// Multiple and Requeue qualify an acknowledgement: whether it settles every
	// delivery up to the tag, and whether a rejected message goes back on the
	// queue or is dropped.
	Multiple bool `json:"multiple,omitempty"`
	Requeue  bool `json:"requeue,omitempty"`

	// MessageCount is the queue depth the broker reported, on a declare, a purge,
	// a delete or a get.
	MessageCount uint32 `json:"message_count,omitempty"`

	// ReplyCode and ReplyText are why a channel or a connection was closed, and
	// Cause names the method that caused it. This is the reason most captures get
	// opened at all: a 406 PRECONDITION_FAILED on a queue.declare says the queue
	// already exists with different arguments, and nothing else in the capture
	// says that.
	ReplyCode uint16 `json:"reply_code,omitempty"`
	ReplyText string `json:"reply_text,omitempty"`
	Cause     string `json:"cause,omitempty"`

	// Mechanisms is the list the server offered; Mechanism is the one the client
	// picked. The choice is worth showing. What it proves possession of is not —
	// see the note on connection.start-ok below.
	Mechanisms string `json:"mechanisms,omitempty"`
	Mechanism  string `json:"mechanism,omitempty"`

	VirtualHost string `json:"virtual_host,omitempty"`

	// Protocol is the version announced by a protocol header, as major-minor-
	// revision.
	Protocol string `json:"protocol,omitempty"`

	// BodySize is the total content length a content header announces, which the
	// body frames after it then deliver in chunks. A capture cut short shows
	// fewer bytes than this, and the difference is the evidence of the cut.
	BodySize int64 `json:"body_size,omitempty"`

	// The content properties worth naming. Delivery mode 2 is persistent and 1 is
	// transient, which is the difference between a message that survives a broker
	// restart and one that does not; reply-to and correlation-id are the whole of
	// the RPC-over-AMQP pattern.
	ContentType   string `json:"content_type,omitempty"`
	DeliveryMode  uint8  `json:"delivery_mode,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	ReplyTo       string `json:"reply_to,omitempty"`

	// Text is a content body when it is valid UTF-8, which is what almost every
	// application publishes. A body that is not text is not claimed as text; the
	// bytes stay in Payload either way.
	Text string `json:"text,omitempty"`

	// Note explains what was deliberately not decoded, or what could not be. A
	// gap the reader knows about is survivable; a silent one is not.
	Note string `json:"note,omitempty"`
}

func methodKey(class, method uint16) uint32 { return uint32(class)<<16 | uint32(method) }

// methodNames covers the methods a person debugging can act on, plus the
// handshake ones whose presence or absence is itself the answer. Anything
// outside it is reported by number rather than dropped.
var methodNames = map[uint32]string{
	methodKey(10, 10): "connection.start",
	methodKey(10, 11): "connection.start-ok",
	methodKey(10, 20): "connection.secure",
	methodKey(10, 21): "connection.secure-ok",
	methodKey(10, 30): "connection.tune",
	methodKey(10, 31): "connection.tune-ok",
	methodKey(10, 40): "connection.open",
	methodKey(10, 41): "connection.open-ok",
	methodKey(10, 50): "connection.close",
	methodKey(10, 51): "connection.close-ok",
	methodKey(10, 60): "connection.blocked",
	methodKey(10, 61): "connection.unblocked",

	methodKey(20, 10): "channel.open",
	methodKey(20, 11): "channel.open-ok",
	methodKey(20, 20): "channel.flow",
	methodKey(20, 21): "channel.flow-ok",
	methodKey(20, 40): "channel.close",
	methodKey(20, 41): "channel.close-ok",

	methodKey(40, 10): "exchange.declare",
	methodKey(40, 11): "exchange.declare-ok",
	methodKey(40, 20): "exchange.delete",
	methodKey(40, 21): "exchange.delete-ok",

	methodKey(50, 10): "queue.declare",
	methodKey(50, 11): "queue.declare-ok",
	methodKey(50, 20): "queue.bind",
	methodKey(50, 21): "queue.bind-ok",
	methodKey(50, 30): "queue.purge",
	methodKey(50, 31): "queue.purge-ok",
	methodKey(50, 40): "queue.delete",
	methodKey(50, 41): "queue.delete-ok",
	methodKey(50, 50): "queue.unbind",
	methodKey(50, 51): "queue.unbind-ok",

	methodKey(60, 10):  "basic.qos",
	methodKey(60, 11):  "basic.qos-ok",
	methodKey(60, 20):  "basic.consume",
	methodKey(60, 21):  "basic.consume-ok",
	methodKey(60, 30):  "basic.cancel",
	methodKey(60, 31):  "basic.cancel-ok",
	methodKey(60, 40):  "basic.publish",
	methodKey(60, 50):  "basic.return",
	methodKey(60, 60):  "basic.deliver",
	methodKey(60, 70):  "basic.get",
	methodKey(60, 71):  "basic.get-ok",
	methodKey(60, 72):  "basic.get-empty",
	methodKey(60, 80):  "basic.ack",
	methodKey(60, 90):  "basic.reject",
	methodKey(60, 120): "basic.nack",
}

func methodName(class, method uint16) string {
	if name, ok := methodNames[methodKey(class, method)]; ok {
		return name
	}
	return fmt.Sprintf("unknown(class %d, method %d)", class, method)
}

// Deframe reads as many whole frames as the buffer holds and reports how many
// bytes were left over.
//
// A remainder is not an error. A capture is a prefix of a conversation that may
// still be running, and cutting one mid-frame is the normal case — the same
// contract pgwire.Deframe and wsframe.Deframe have, for the same reason. It is
// also where a refused size and a missing end marker land: a desynchronised
// stream and an unfinished frame are indistinguishable from inside, and both
// mean "read no further".
func Deframe(stream []byte, fromClient bool) (frames []Frame, remainder int) {
	rest := stream

	for len(rest) > 0 {
		// The protocol header is not a frame: no channel, no size, no end marker.
		// It is checked at every boundary rather than only at the start, because
		// a server that refuses the version replies with a header of its own and
		// a capture can begin anywhere. A frame type byte of 'A' does not exist,
		// so the literal cannot be mistaken for one.
		if len(rest) >= 4 && string(rest[:4]) == "AMQP" {
			if len(rest) < protocolHeaderSize {
				return frames, len(rest)
			}
			frames = append(frames, protocolHeader(rest[:protocolHeaderSize], fromClient))
			rest = rest[protocolHeaderSize:]
			continue
		}

		if len(rest) < frameHeaderSize+1 {
			return frames, len(rest)
		}
		size := binary.BigEndian.Uint32(rest[3:7])
		if size > maxFrame {
			return frames, len(rest)
		}
		end := frameHeaderSize + int(size) + 1
		if len(rest) < end {
			return frames, len(rest)
		}
		if rest[end-1] != frameEnd {
			// The size said the frame ended here and the protocol's own marker
			// says it did not. Reading on would frame the next "frame" from the
			// middle of this one's payload and report methods nobody sent.
			return frames, len(rest)
		}

		frames = append(frames, decode(rest[0], binary.BigEndian.Uint16(rest[1:3]), rest[frameHeaderSize:end-1], fromClient))
		rest = rest[end:]
	}

	return frames, len(rest)
}

func protocolHeader(b []byte, fromClient bool) Frame {
	f := Frame{
		Type:       "protocol_header",
		Kind:       "protocol_header",
		Size:       protocolHeaderSize,
		FromClient: fromClient,
		Payload:    b,
		Protocol:   fmt.Sprintf("%d-%d-%d", b[5], b[6], b[7]),
	}
	// 0-9-1 is the only version this package can read. AMQP 1.0 is a different
	// protocol that happens to share the four magic letters, and framing its
	// bytes as 0-9-1 would produce confident nonsense.
	if f.Protocol != "0-9-1" {
		f.Note = "this peer announced a version other than AMQP 0-9-1; the bytes after this header are a different protocol and are not decoded here"
	}
	return f
}

func decode(typ byte, channel uint16, payload []byte, fromClient bool) Frame {
	f := Frame{
		Channel:    channel,
		Size:       int64(len(payload)),
		FromClient: fromClient,
		Payload:    payload,
	}
	cur := &cursor{b: payload}

	switch typ {
	case frameMethod:
		f.Type = "method"
		f.Class = cur.uint16()
		f.Method = cur.uint16()
		f.Kind = methodName(f.Class, f.Method)
		decodeMethod(&f, cur)

	case frameHeader:
		f.Type = "header"
		f.Kind = "content_header"
		f.Class = cur.uint16()
		cur.uint16() // weight, reserved and always zero
		f.BodySize = int64(cur.uint64())
		decodeProperties(&f, cur)

	case frameBody:
		f.Type = "body"
		f.Kind = "content_body"
		// Only when it really is valid UTF-8. A body is whatever the application
		// published — protobuf, gzip, an image — and claiming bytes are text they
		// are not is the kind of confident nonsense a debugger must never print.
		if utf8.Valid(payload) {
			f.Text = string(payload)
		}

	case frameHeartbeat:
		f.Type = "heartbeat"
		f.Kind = "heartbeat"

	default:
		f.Type = fmt.Sprintf("unknown(0x%02X)", typ)
		f.Kind = f.Type
	}

	noteIfPartial(&f, cur)
	return f
}

// decodeMethod reads the arguments of the methods worth reading.
//
// Every bit field below is read as a single octet. AMQP packs consecutive bit
// arguments into one octet, least significant bit first, and starts a fresh
// octet at the next non-bit argument — and no method here has more than eight
// consecutive bits, so one octet and a mask is the whole of it.
func decodeMethod(f *Frame, cur *cursor) {
	switch f.Kind {
	case "connection.start":
		cur.uint8() // version major
		cur.uint8() // version minor
		cur.table() // server properties: product, version, capabilities
		f.Mechanisms = cur.longstr()

	case "connection.start-ok":
		cur.table() // client properties
		f.Mechanism = cur.shortstr()
		// What follows is the SASL response, and for the PLAIN mechanism it is
		// literally "\0user\0password". Decoding it would put the broker's
		// credentials into a view a browser renders and an agent can read. The
		// mechanism is worth showing; the exchange is not.
		f.Note = "not decoded: this method carries the SASL response, which is the password"

	case "connection.secure":
		f.Note = "not decoded: this method carries a SASL challenge"

	case "connection.secure-ok":
		f.Note = "not decoded: this method carries a SASL response, which is the password"

	case "connection.open":
		f.VirtualHost = cur.shortstr()

	case "connection.close", "channel.close":
		f.ReplyCode = cur.uint16()
		f.ReplyText = cur.shortstr()
		// The class and method that provoked the close. A 406 on its own says
		// something failed; a 406 naming queue.declare says which thing.
		if class, method := cur.uint16(), cur.uint16(); class != 0 {
			f.Cause = methodName(class, method)
		}

	case "connection.blocked":
		// RabbitMQ stops accepting publishes under memory or disk pressure and
		// says so here. A producer that has silently stalled is usually this.
		f.ReplyText = cur.shortstr()

	case "exchange.declare", "exchange.delete":
		cur.uint16() // reserved
		f.Exchange = cur.shortstr()

	case "queue.declare", "queue.purge", "queue.delete":
		cur.uint16() // reserved
		f.Queue = cur.shortstr()

	case "queue.declare-ok":
		f.Queue = cur.shortstr()
		f.MessageCount = cur.uint32()

	case "queue.purge-ok", "queue.delete-ok":
		f.MessageCount = cur.uint32()

	case "queue.bind", "queue.unbind":
		cur.uint16() // reserved
		f.Queue = cur.shortstr()
		f.Exchange = cur.shortstr()
		f.RoutingKey = cur.shortstr()

	case "basic.consume":
		cur.uint16() // reserved
		f.Queue = cur.shortstr()
		f.ConsumerTag = cur.shortstr()

	case "basic.consume-ok", "basic.cancel", "basic.cancel-ok":
		f.ConsumerTag = cur.shortstr()

	case "basic.publish":
		cur.uint16() // reserved
		f.Exchange = cur.shortstr()
		f.RoutingKey = cur.shortstr()

	case "basic.return":
		// The broker handing a published message back because it could not be
		// routed. Silent message loss looks exactly like this in a capture.
		f.ReplyCode = cur.uint16()
		f.ReplyText = cur.shortstr()
		f.Exchange = cur.shortstr()
		f.RoutingKey = cur.shortstr()

	case "basic.deliver":
		f.ConsumerTag = cur.shortstr()
		f.DeliveryTag = cur.uint64()
		f.Redelivered = cur.uint8()&0x01 != 0
		f.Exchange = cur.shortstr()
		f.RoutingKey = cur.shortstr()

	case "basic.get":
		cur.uint16() // reserved
		f.Queue = cur.shortstr()

	case "basic.get-ok":
		f.DeliveryTag = cur.uint64()
		f.Redelivered = cur.uint8()&0x01 != 0
		f.Exchange = cur.shortstr()
		f.RoutingKey = cur.shortstr()
		f.MessageCount = cur.uint32()

	case "basic.ack":
		f.DeliveryTag = cur.uint64()
		f.Multiple = cur.uint8()&0x01 != 0

	case "basic.reject":
		f.DeliveryTag = cur.uint64()
		f.Requeue = cur.uint8()&0x01 != 0

	case "basic.nack":
		f.DeliveryTag = cur.uint64()
		bits := cur.uint8()
		f.Multiple = bits&0x01 != 0
		f.Requeue = bits&0x02 != 0
	}
}

// decodeProperties reads a content header's property list.
//
// The list is a bitmap followed by the values of exactly the properties whose
// bits are set, in bit order from 15 downwards. There are no lengths and no
// tags: a property misread is every property after it misread, which is why
// each one below is consumed whether or not it is reported.
func decodeProperties(f *Frame, cur *cursor) {
	if f.Class != classBasic {
		// basic is the only class in 0-9-1 that carries content. A header for any
		// other class has no defined property layout, so walking it would be
		// guessing.
		f.Note = fmt.Sprintf("content header for class %d, whose properties have no defined layout in AMQP 0-9-1", f.Class)
		return
	}

	flags := cur.uint16()
	// Bit 0 says the bitmap continues into another word. Nothing in 0-9-1 defines
	// a sixteenth property, but the framing allows it and skipping the extra
	// words keeps the values that follow aligned.
	for more := flags; more&0x0001 != 0 && !cur.bad; {
		more = cur.uint16()
	}

	if flags&0x8000 != 0 {
		f.ContentType = cur.shortstr()
	}
	if flags&0x4000 != 0 {
		cur.shortstr() // content encoding
	}
	if flags&0x2000 != 0 {
		// Application headers. They are arbitrary and application-defined, and
		// rendering them would mean decoding the whole field-table type system
		// for something that is rarely the answer.
		cur.table()
	}
	if flags&0x1000 != 0 {
		f.DeliveryMode = cur.uint8()
	}
	if flags&0x0800 != 0 {
		cur.uint8() // priority
	}
	if flags&0x0400 != 0 {
		f.CorrelationID = cur.shortstr()
	}
	if flags&0x0200 != 0 {
		f.ReplyTo = cur.shortstr()
	}
	if flags&0x0100 != 0 {
		cur.shortstr() // expiration
	}
	if flags&0x0080 != 0 {
		cur.shortstr() // message id
	}
	if flags&0x0040 != 0 {
		cur.uint64() // timestamp
	}
	if flags&0x0020 != 0 {
		cur.shortstr() // type
	}
	if flags&0x0010 != 0 {
		cur.shortstr() // user id
	}
	if flags&0x0008 != 0 {
		cur.shortstr() // app id
	}
	if flags&0x0004 != 0 {
		cur.shortstr() // reserved
	}
}

// noteIfPartial records that the reading is incomplete, so nobody mistakes a
// half-decoded frame for the whole of what was sent.
func noteIfPartial(f *Frame, cur *cursor) {
	if f.Note != "" {
		return
	}
	switch {
	case cur.bad:
		f.Note = "the frame payload ended sooner than its own fields claimed; this reading is partial"
	case cur.nonUTF8:
		f.Note = "one or more text fields were not valid UTF-8 and are shown empty; the bytes are in the capture"
	}
}

// cursor walks a frame payload.
//
// Every read is bounds checked and a read past the end sets bad and yields a
// zero, which keeps the length fields — all of them chosen by the sender — from
// turning a malformed capture into a panic or a huge allocation.
type cursor struct {
	b       []byte
	bad     bool
	nonUTF8 bool
}

func (c *cursor) take(n int) []byte {
	if n < 0 || len(c.b) < n {
		c.bad = true
		c.b = nil
		return nil
	}
	out := c.b[:n]
	c.b = c.b[n:]
	return out
}

func (c *cursor) uint8() byte {
	v := c.take(1)
	if v == nil {
		return 0
	}
	return v[0]
}

func (c *cursor) uint16() uint16 {
	v := c.take(2)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint16(v)
}

func (c *cursor) uint32() uint32 {
	v := c.take(4)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (c *cursor) uint64() uint64 {
	v := c.take(8)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// shortstr reads a string with a one-byte length, which is how AMQP carries
// every name: a queue, an exchange, a routing key, a consumer tag.
//
// The length is read in its own statement rather than nested in the call: the
// order the two reads happen in is the difference between a name and garbage,
// and it should not depend on how a reader remembers Go's argument evaluation.
func (c *cursor) shortstr() string {
	n := int(c.uint8())
	return c.text(c.take(n))
}

// longstr reads a string with a four-byte length.
func (c *cursor) longstr() string {
	n := int(c.uint32())
	return c.text(c.take(n))
}

// table skips a field table. Its contents are application-defined — queue
// arguments, client capabilities, message headers — and decoding the whole AMQP
// type system for something that is rarely the answer would be a lot of code to
// maintain. The length is still consumed, because everything after it depends
// on landing in the right place.
func (c *cursor) table() {
	n := int(c.uint32())
	c.take(n)
}

func (c *cursor) text(raw []byte) string {
	if raw == nil {
		return ""
	}
	if !utf8.Valid(raw) {
		// AMQP says these are UTF-8, but a broken client is exactly what someone
		// opens a capture to find. Rendering the bytes as a Go string would put
		// replacement characters in a view people copy queue names out of.
		c.nonUTF8 = true
		return ""
	}
	return string(raw)
}

// Summarise gives a one-line reading of a conversation, for a listing that has
// no room for the frames themselves.
//
// It is not a plain count by kind the way wsframe's is. An AMQP connection is
// mostly handshake and heartbeats, and "1 connection.start, 1
// connection.start-ok, 40 heartbeat" tells a reader nothing they came for: they
// came for the failure, or for what was published where. Counts remain the
// fallback for a connection that did neither.
func Summarise(frames []Frame) string {
	if len(frames) == 0 {
		return "no frames"
	}

	var failures []Frame
	var route string
	published, delivered, heartbeats := 0, 0, 0
	channels := map[uint16]bool{}

	for _, f := range frames {
		if f.Channel != 0 {
			channels[f.Channel] = true
		}
		switch {
		case f.ReplyCode >= 300:
			// 200 is a normal goodbye. Everything from 300 up is a channel or a
			// connection that died, or a message the broker could not route.
			failures = append(failures, f)
		case f.Kind == "heartbeat":
			heartbeats++
		case f.Kind == "basic.publish":
			published++
			if route == "" {
				route = routeOf(f)
			}
		case f.Kind == "basic.deliver" || f.Kind == "basic.get-ok":
			delivered++
			if route == "" {
				route = routeOf(f)
			}
		}
	}

	// The failure is why the capture was opened. Nothing outranks it — and if
	// more than one arrived, the count says so rather than the line pretending
	// there was one.
	if len(failures) > 0 {
		line := failureLine(failures[0])
		if len(failures) > 1 {
			line += fmt.Sprintf(" (+%d more)", len(failures)-1)
		}
		return line + channelSuffix(channels)
	}

	var parts []string
	if published > 0 {
		parts = append(parts, fmt.Sprintf("%d published", published))
	}
	if delivered > 0 {
		parts = append(parts, fmt.Sprintf("%d delivered", delivered))
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", ") + " (" + route + ")" + channelSuffix(channels)
	}

	// Nothing moved. Fall back to counts, with the heartbeats folded into one
	// entry: a connection idling for an hour is hundreds of them, and listing
	// them one by one would bury the handshake that is the only thing to see.
	counts := map[string]int{}
	order := []string{}
	for _, f := range frames {
		if f.Kind == "heartbeat" {
			continue
		}
		if counts[f.Kind] == 0 {
			order = append(order, f.Kind)
		}
		counts[f.Kind]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })

	for _, kind := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	if heartbeats > 0 {
		parts = append(parts, fmt.Sprintf("%d heartbeats", heartbeats))
	}
	return strings.Join(parts, ", ") + channelSuffix(channels)
}

// routeOf renders where a message went. The default exchange is named rather
// than shown as an empty string: it is a real exchange with real behaviour —
// the routing key is a queue name — and a blank there reads like missing data.
func routeOf(f Frame) string {
	exchange := f.Exchange
	if exchange == "" {
		exchange = "(default)"
	}
	if f.RoutingKey == "" {
		return exchange
	}
	return exchange + " -> " + f.RoutingKey
}

func failureLine(f Frame) string {
	line := fmt.Sprintf("%s %d", f.Kind, f.ReplyCode)
	if f.ReplyText != "" {
		line += ": " + f.ReplyText
	}
	if f.Cause != "" {
		line += " (on " + f.Cause + ")"
	}
	return line
}

// channelSuffix says when several conversations shared the connection, because
// every line above describes them as though they were one.
func channelSuffix(channels map[uint16]bool) string {
	if len(channels) > 1 {
		return fmt.Sprintf(" on %d channels", len(channels))
	}
	return ""
}

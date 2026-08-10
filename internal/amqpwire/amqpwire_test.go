package amqpwire

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// The helpers below put bytes on the wire the way a client library does, so the
// tests prove the decoder against the format rather than against itself.
// Nothing here imports an AMQP library: a test that borrows the encoder from
// the library it is checking proves only that the two agree.

// frame wraps a payload in the seven-byte header and the 0xCE end marker.
func frame(typ byte, channel uint16, parts ...[]byte) []byte {
	payload := cat(parts...)
	out := []byte{typ}
	out = binary.BigEndian.AppendUint16(out, channel)
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	return append(out, frameEnd)
}

// method frames a method: the class and method ids, then the arguments.
func method(channel, class, id uint16, args ...[]byte) []byte {
	return frame(frameMethod, channel, u16(class), u16(id), cat(args...))
}

// content frames a basic content header: the class, the reserved weight, the
// announced body size, the property bitmap and the properties it flagged.
func content(channel uint16, bodySize uint64, flags uint16, props ...[]byte) []byte {
	return frame(frameHeader, channel, u16(classBasic), u16(0), u64(bodySize), u16(flags), cat(props...))
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func u8(v byte) []byte    { return []byte{v} }
func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func shortstr(s string) []byte {
	return append([]byte{byte(len(s))}, s...)
}
func longstr(s string) []byte { return append(u32(uint32(len(s))), s...) }

// emptyTable is a field table with no entries: a length of zero and nothing
// after it.
func emptyTable() []byte { return u32(0) }

// A publish is three frames, not one: the method says where the message went,
// the content header says what it is, and the body carries it. Losing the join
// between them loses the message.
func TestAPublishIsAMethodAHeaderAndABody(t *testing.T) {
	body := `{"order":417}`
	stream := cat(
		method(1, 60, 40, u16(0), shortstr("orders"), shortstr("order.created"), u8(0)),
		content(1, uint64(len(body)), 0x9000, shortstr("application/json"), u8(2)),
		frame(frameBody, 1, []byte(body)),
	)

	frames, rest := Deframe(stream, true)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	if len(frames) != 3 {
		t.Fatalf("%d frames, want 3", len(frames))
	}

	publish := frames[0]
	if publish.Kind != "basic.publish" || publish.Type != "method" {
		t.Fatalf("kind=%q type=%q", publish.Kind, publish.Type)
	}
	if publish.Exchange != "orders" || publish.RoutingKey != "order.created" {
		t.Errorf("route = %q -> %q", publish.Exchange, publish.RoutingKey)
	}
	if publish.Channel != 1 {
		t.Errorf("channel = %d", publish.Channel)
	}

	head := frames[1]
	if head.Kind != "content_header" {
		t.Fatalf("kind = %q", head.Kind)
	}
	if head.ContentType != "application/json" {
		t.Errorf("content type = %q", head.ContentType)
	}
	if head.DeliveryMode != 2 {
		t.Errorf("delivery mode = %d, want 2 (persistent)", head.DeliveryMode)
	}
	if head.BodySize != int64(len(body)) {
		t.Errorf("body size = %d, want %d", head.BodySize, len(body))
	}

	if frames[2].Kind != "content_body" || frames[2].Text != body {
		t.Errorf("body frame = %+v", frames[2])
	}

	if got := Summarise(frames); !strings.Contains(got, "orders -> order.created") {
		t.Errorf("summary = %q, want the route in it", got)
	}
}

// The property list has no tags and no lengths: a property misread is every
// property after it misread. The headers table in the middle is the trap —
// skipping it by the wrong number of bytes turns reply-to into noise.
func TestThePropertyListIsWalkedInBitOrder(t *testing.T) {
	// content-type, headers, delivery-mode, priority, correlation-id, reply-to.
	const flags = 0x8000 | 0x2000 | 0x1000 | 0x0800 | 0x0400 | 0x0200
	stream := content(3, 12, flags,
		shortstr("text/plain"),
		cat(u32(9), []byte("ignorable")), // an application headers table
		u8(1),
		u8(5),
		shortstr("rpc-88"),
		shortstr("amq.rabbitmq.reply-to"),
	)

	frames, rest := Deframe(stream, false)
	if rest != 0 || len(frames) != 1 {
		t.Fatalf("%d frames, %d left over", len(frames), rest)
	}
	f := frames[0]
	if f.ContentType != "text/plain" {
		t.Errorf("content type = %q", f.ContentType)
	}
	if f.DeliveryMode != 1 {
		t.Errorf("delivery mode = %d, want 1 (transient)", f.DeliveryMode)
	}
	if f.CorrelationID != "rpc-88" || f.ReplyTo != "amq.rabbitmq.reply-to" {
		t.Errorf("the walk lost its place: correlation=%q reply-to=%q", f.CorrelationID, f.ReplyTo)
	}
	if f.Note != "" {
		t.Errorf("note = %q, want a clean reading", f.Note)
	}
}

// The SASL response is the broker password in plaintext: PLAIN is literally
// "\0user\0password". The mechanism is worth showing, the exchange is not, and
// nothing that reaches a view may carry it.
func TestTheSASLResponseIsNamedButNotDecoded(t *testing.T) {
	stream := method(0, 10, 11,
		emptyTable(),
		shortstr("PLAIN"),
		longstr("\x00guest\x00hunter2"),
		shortstr("en_US"),
	)

	frames, _ := Deframe(stream, true)
	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	f := frames[0]
	if f.Kind != "connection.start-ok" {
		t.Fatalf("kind = %q", f.Kind)
	}
	if f.Mechanism != "PLAIN" {
		t.Errorf("mechanism = %q, want the choice to be visible", f.Mechanism)
	}
	if f.Note == "" {
		t.Error("nothing tells the reader why the rest of this method is missing")
	}

	// Every reported field at once, not a hand-picked few: the point is that no
	// route to a view carries the secret, whichever field a later change adds.
	view, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "guest"} {
		if strings.Contains(string(view), secret) {
			t.Errorf("the decoded frame leaked %q: %s", secret, view)
		}
	}
}

// A connection.secure-ok is the second half of a challenge-response and is just
// as much the password as the first.
func TestTheSASLChallengeAndItsAnswerAreBothWithheld(t *testing.T) {
	stream := cat(
		method(0, 10, 20, longstr("challenge-bytes")),
		method(0, 10, 21, longstr("response-bytes")),
	)
	frames, _ := Deframe(stream, true)
	if len(frames) != 2 {
		t.Fatalf("%d frames", len(frames))
	}
	for _, f := range frames {
		if f.Note == "" {
			t.Errorf("%s was decoded with no note saying what was withheld", f.Kind)
		}
		view, _ := json.Marshal(f)
		if strings.Contains(string(view), "bytes") {
			t.Errorf("%s leaked its body: %s", f.Kind, view)
		}
	}
}

// One connection carries many independent conversations. A reader who cannot
// tell them apart is reading several unrelated exchanges as though they were
// one, and the delivery tags below collide across channels on purpose.
func TestChannelsAreKeptApart(t *testing.T) {
	stream := cat(
		method(1, 60, 60, shortstr("ctag-a"), u64(1), u8(0), shortstr("orders"), shortstr("a")),
		method(2, 60, 60, shortstr("ctag-b"), u64(1), u8(1), shortstr("audit"), shortstr("b")),
		method(1, 60, 80, u64(1), u8(0)),
	)

	frames, rest := Deframe(stream, false)
	if rest != 0 || len(frames) != 3 {
		t.Fatalf("%d frames, %d left over", len(frames), rest)
	}
	if frames[0].Channel != 1 || frames[1].Channel != 2 || frames[2].Channel != 1 {
		t.Fatalf("channels = %d, %d, %d", frames[0].Channel, frames[1].Channel, frames[2].Channel)
	}
	if frames[0].ConsumerTag != "ctag-a" || frames[1].ConsumerTag != "ctag-b" {
		t.Errorf("consumer tags = %q, %q", frames[0].ConsumerTag, frames[1].ConsumerTag)
	}
	if frames[0].Redelivered {
		t.Error("a first delivery was reported as redelivered")
	}
	if !frames[1].Redelivered {
		t.Error("a redelivery was not reported as one, which is how a poison message hides")
	}
	if got := Summarise(frames); !strings.Contains(got, "2 channels") {
		t.Errorf("summary = %q, want it to say the connection was multiplexed", got)
	}
}

// Whether a delivery was settled, and how, is the difference between a working
// consumer and a queue that grows for ever.
func TestAcknowledgementsAreReadBackWithTheirTags(t *testing.T) {
	frames, rest := Deframe(cat(
		method(1, 60, 80, u64(7), u8(0x01)),  // ack, multiple
		method(1, 60, 90, u64(8), u8(0x01)),  // reject, requeue
		method(1, 60, 120, u64(9), u8(0x02)), // nack, requeue but not multiple
		method(1, 60, 71, u64(10), u8(0x01), shortstr(""), shortstr("jobs"), u32(4)),
	), true)
	if rest != 0 || len(frames) != 4 {
		t.Fatalf("%d frames, %d left over", len(frames), rest)
	}

	ack := frames[0]
	if ack.Kind != "basic.ack" || ack.DeliveryTag != 7 || !ack.Multiple {
		t.Errorf("ack = %+v", ack)
	}
	reject := frames[1]
	if reject.Kind != "basic.reject" || reject.DeliveryTag != 8 || !reject.Requeue {
		t.Errorf("reject = %+v", reject)
	}
	nack := frames[2]
	if nack.Kind != "basic.nack" || !nack.Requeue {
		t.Errorf("nack = %+v", nack)
	}
	if nack.Multiple {
		t.Error("the nack bits were read in the wrong order: requeue was taken as multiple")
	}

	get := frames[3]
	if get.Kind != "basic.get-ok" || get.DeliveryTag != 10 || get.MessageCount != 4 {
		t.Errorf("get-ok = %+v", get)
	}
	if get.RoutingKey != "jobs" || get.Exchange != "" {
		t.Errorf("get-ok route = %q -> %q, want the default exchange", get.Exchange, get.RoutingKey)
	}
	if got := Summarise(frames[3:]); !strings.Contains(got, "(default)") {
		t.Errorf("summary = %q; the default exchange is a real one and a blank there reads as missing data", got)
	}
}

// This is the frame people open a capture to find. A 406 on its own says
// something failed; a 406 naming queue.declare says which thing.
func TestAChannelCloseCarriesWhyAndWhat(t *testing.T) {
	text := "PRECONDITION_FAILED - inequivalent arg 'durable' for queue 'jobs'"
	frames, rest := Deframe(method(1, 20, 40, u16(406), shortstr(text), u16(50), u16(10)), false)
	if rest != 0 || len(frames) != 1 {
		t.Fatalf("%d frames, %d left over", len(frames), rest)
	}
	f := frames[0]
	if f.Kind != "channel.close" || f.ReplyCode != 406 {
		t.Fatalf("kind=%q code=%d", f.Kind, f.ReplyCode)
	}
	if f.ReplyText != text {
		t.Errorf("reply text = %q", f.ReplyText)
	}
	if f.Cause != "queue.declare" {
		t.Errorf("cause = %q, want the method that provoked the close", f.Cause)
	}

	got := Summarise(frames)
	if !strings.Contains(got, "406") || !strings.Contains(got, "queue.declare") {
		t.Errorf("summary = %q, want the failure to lead", got)
	}
}

// A normal goodbye is reply code 200. Reporting it as a failure would put a
// red line on every clean disconnection.
func TestANormalCloseIsNotAFailure(t *testing.T) {
	frames, _ := Deframe(cat(
		method(1, 60, 40, u16(0), shortstr("orders"), shortstr("k"), u8(0)),
		method(0, 10, 50, u16(200), shortstr("OK"), u16(0), u16(0)),
	), true)

	if got := Summarise(frames); !strings.Contains(got, "1 published") {
		t.Errorf("summary = %q, want the publish rather than a fake failure", got)
	}
}

// The protocol header is not a frame: no channel, no size, no end marker.
// Framing it as one would read "AMQP" as a type byte and a channel and desync
// the connection from byte zero.
func TestTheProtocolHeaderIsNotAFrame(t *testing.T) {
	header := []byte{'A', 'M', 'Q', 'P', 0, 0, 9, 1}
	frames, rest := Deframe(cat(header, method(0, 10, 11, emptyTable(), shortstr("PLAIN"), longstr("x"), shortstr("en_US"))), true)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	if len(frames) != 2 {
		t.Fatalf("%d frames, want the header and the method after it", len(frames))
	}
	if frames[0].Kind != "protocol_header" || frames[0].Protocol != "0-9-1" {
		t.Errorf("header = %+v", frames[0])
	}
	if frames[0].Note != "" {
		t.Errorf("note = %q, want none for the version this package reads", frames[0].Note)
	}
	if frames[1].Kind != "connection.start-ok" {
		t.Errorf("the frame after the header read as %q", frames[1].Kind)
	}
}

// AMQP 1.0 is a different protocol that happens to share the four magic
// letters. Framing its bytes as 0-9-1 would produce confident nonsense.
func TestAnotherVersionIsReportedRatherThanDecodedAsGarbage(t *testing.T) {
	frames, _ := Deframe([]byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, true)
	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	if frames[0].Protocol != "1-0-0" {
		t.Errorf("protocol = %q", frames[0].Protocol)
	}
	if frames[0].Note == "" {
		t.Error("nothing tells the reader this is not the protocol being decoded")
	}
}

// A capture is a prefix of a conversation that may still be running, so a
// half-frame at the end is normal and must not lose the whole ones before it.
func TestATruncatedTailKeepsWhatCameBefore(t *testing.T) {
	whole := method(1, 60, 40, u16(0), shortstr("orders"), shortstr("first"), u8(0))
	partial := method(1, 60, 40, u16(0), shortstr("orders"), shortstr("second"), u8(0))

	for cut := 1; cut < len(partial); cut++ {
		frames, rest := Deframe(cat(whole, partial[:cut]), true)
		if len(frames) != 1 {
			t.Fatalf("cut at %d: %d frames, want the one complete frame", cut, len(frames))
		}
		if frames[0].RoutingKey != "first" {
			t.Errorf("cut at %d: routing key = %q", cut, frames[0].RoutingKey)
		}
		if rest != cut {
			t.Errorf("cut at %d: remainder = %d, want %d", cut, rest, cut)
		}
	}
}

// The end marker is the protocol's own resynchronisation check. Without it, a
// size field one byte out frames the next "frame" from the middle of this one's
// payload and reports methods nobody sent.
func TestAMissingEndMarkerStopsTheReader(t *testing.T) {
	good := method(1, 60, 40, u16(0), shortstr("orders"), shortstr("k"), u8(0))
	bad := method(1, 60, 40, u16(0), shortstr("orders"), shortstr("k"), u8(0))
	bad[len(bad)-1] = 0x00

	frames, rest := Deframe(cat(good, bad), true)
	if len(frames) != 1 {
		t.Fatalf("%d frames, want only the well-formed one", len(frames))
	}
	if rest != len(bad) {
		t.Errorf("remainder = %d, want the %d unreadable bytes", rest, len(bad))
	}
}

// A size that cannot be real is a corrupt stream, a mis-framed one, or not this
// protocol at all. Reading on would mean trusting it to size an allocation.
func TestAnAbsurdSizeIsRefusedRatherThanAllocated(t *testing.T) {
	for _, size := range []uint32{0xFFFFFFFF, 1 << 31, (1 << 30) + 1} {
		header := cat([]byte{frameMethod}, u16(1), u32(size))
		frames, rest := Deframe(header, true)
		if len(frames) != 0 {
			t.Errorf("size %d: %d frames from a corrupt header", size, len(frames))
		}
		if rest != len(header) {
			t.Errorf("size %d: remainder = %d, want the whole buffer back", size, rest)
		}
	}

	// The same applies to a length inside a payload: a shortstr claiming 200
	// bytes in a six-byte method must not invent them.
	frames, _ := Deframe(frame(frameMethod, 1, u16(50), u16(10), u16(0), u8(200), []byte("jobs")), true)
	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	if frames[0].Queue != "" || frames[0].Note == "" {
		t.Errorf("a truncated name was reported as whole: %+v", frames[0])
	}
}

// The product principle is to degrade to a lesser view, never to a blank one.
// An unrecognised method still has a name and a size.
func TestAnUnknownMethodIsReportedByClassAndMethod(t *testing.T) {
	frames, rest := Deframe(frame(frameMethod, 1, u16(85), u16(10), []byte("...")), true)
	if rest != 0 || len(frames) != 1 {
		t.Fatalf("%d frames, %d left over", len(frames), rest)
	}
	f := frames[0]
	if !strings.Contains(f.Kind, "85") || !strings.Contains(f.Kind, "10") {
		t.Errorf("kind = %q, want something naming the class and method", f.Kind)
	}
	if f.Class != 85 || f.Method != 10 {
		t.Errorf("class=%d method=%d", f.Class, f.Method)
	}
	if f.Size != 7 {
		t.Errorf("size = %d, want 7", f.Size)
	}

	// And a frame type nobody has heard of, which is the same principle one
	// layer down.
	frames, _ = Deframe(frame(0x05, 1, []byte("....")), true)
	if len(frames) != 1 || !strings.Contains(frames[0].Type, "0x05") {
		t.Errorf("unknown frame type read as %+v", frames)
	}
}

// A connection idling for an hour is hundreds of heartbeats. Listing them one
// by one would bury the handshake that is the only thing to see.
func TestHeartbeatsAreFoldedRatherThanListed(t *testing.T) {
	stream := method(0, 10, 41)
	for range 40 {
		stream = append(stream, frame(frameHeartbeat, 0)...)
	}

	frames, rest := Deframe(stream, false)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	// Every heartbeat is still a frame that crossed, and the capture says so.
	if len(frames) != 41 {
		t.Fatalf("%d frames, want all 41 kept", len(frames))
	}

	got := Summarise(frames)
	if !strings.Contains(got, "40 heartbeats") {
		t.Errorf("summary = %q, want the heartbeats counted once", got)
	}
	if !strings.Contains(got, "connection.open-ok") {
		t.Errorf("summary = %q, want the one interesting frame still visible", got)
	}
	if Summarise(nil) != "no frames" {
		t.Errorf("empty summary = %q", Summarise(nil))
	}
}

// Bytes that are not UTF-8 are what someone opens a capture to find. Claiming
// they are text is the kind of confident nonsense the product rules out.
func TestANonUTF8NameIsNotClaimedAsText(t *testing.T) {
	raw := []byte{0x48, 0xC3, 0x28, 0xFF}
	frames, _ := Deframe(frame(frameMethod, 1, u16(60), u16(40), u16(0), shortstr("orders"), append(u8(byte(len(raw))), raw...), u8(0)), true)
	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	f := frames[0]
	if f.RoutingKey != "" {
		t.Errorf("invalid UTF-8 was reported as the routing key %q", f.RoutingKey)
	}
	if f.Note == "" {
		t.Error("the reader was not told a field had been dropped")
	}
	if f.Exchange != "orders" {
		t.Errorf("the fields before it were lost too: exchange = %q", f.Exchange)
	}
}

// A payload cut inside its own fields is worse than a cut between frames,
// because a naive parser reads past the end of it. What can be read is kept and
// the gap is stated.
func TestAPayloadCutInsideItsFieldsIsReportedAsPartial(t *testing.T) {
	// A deliver that stops after the delivery tag.
	frames, rest := Deframe(frame(frameMethod, 1, u16(60), u16(60), shortstr("ctag"), u64(3)), false)
	if rest != 0 || len(frames) != 1 {
		t.Fatalf("%d frames, %d left over", len(frames), rest)
	}
	f := frames[0]
	if f.ConsumerTag != "ctag" || f.DeliveryTag != 3 {
		t.Errorf("what was really there was lost: %+v", f)
	}
	if f.Note == "" {
		t.Error("a partial reading was presented as a complete one")
	}
}

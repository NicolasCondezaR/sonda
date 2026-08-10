package wsframe

import (
	"encoding/binary"
	"strings"
	"testing"
)

// frame builds a frame the way a real endpoint would, so the tests read the
// same bytes a browser or a Go client actually sends.
func frame(final bool, opcode int, payload []byte, mask *[4]byte) []byte {
	var out []byte
	first := byte(opcode)
	if final {
		first |= 0x80
	}
	out = append(out, first)

	second := byte(0)
	if mask != nil {
		second |= 0x80
	}
	switch n := len(payload); {
	case n < 126:
		out = append(out, second|byte(n))
	case n < 1<<16:
		out = append(out, second|126)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	default:
		out = append(out, second|127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}

	if mask != nil {
		out = append(out, mask[:]...)
		masked := make([]byte, len(payload))
		for i := range payload {
			masked[i] = payload[i] ^ mask[i%4]
		}
		return append(out, masked...)
	}
	return append(out, payload...)
}

func TestATextFrameFromTheServer(t *testing.T) {
	frames, rest := Deframe(frame(true, OpText, []byte(`{"sku":"ABC-9"}`), nil))

	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	if len(frames) != 1 {
		t.Fatalf("%d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.Kind != "text" || !f.Final {
		t.Errorf("kind=%q final=%v", f.Kind, f.Final)
	}
	if f.Text != `{"sku":"ABC-9"}` {
		t.Errorf("text = %q", f.Text)
	}
	if f.Masked {
		t.Error("a server frame must not be masked")
	}
}

// Every frame a client sends is masked. Reporting the masked bytes as the
// payload would show gibberish for exactly half the conversation.
func TestAClientFrameIsUnmasked(t *testing.T) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	frames, _ := Deframe(frame(true, OpText, []byte("Hello"), &key))

	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	f := frames[0]
	if f.Text != "Hello" {
		t.Errorf("text = %q, want the unmasked payload", f.Text)
	}
	if !f.Masked {
		t.Error("the frame does not record that it arrived masked")
	}
	if f.MaskKey != binary.BigEndian.Uint32(key[:]) {
		t.Error("the masking key was not kept, so the frame cannot be reproduced")
	}
}

// One capture holds a whole conversation, so reading them back in order is the
// ordinary case rather than the interesting one.
func TestAWholeConversationInOneBuffer(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	var stream []byte
	stream = append(stream, frame(true, OpText, []byte("subscribe"), &key)...)
	stream = append(stream, frame(true, OpText, []byte("ack"), nil)...)
	stream = append(stream, frame(true, OpPing, nil, nil)...)
	stream = append(stream, frame(true, OpPong, nil, &key)...)

	frames, rest := Deframe(stream)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	want := []string{"text", "text", "ping", "pong"}
	if len(frames) != len(want) {
		t.Fatalf("%d frames, want %d", len(frames), len(want))
	}
	for i, kind := range want {
		if frames[i].Kind != kind {
			t.Errorf("frame %d is %q, want %q", i, frames[i].Kind, kind)
		}
	}
}

// A capture is a prefix of a conversation that may still be running, so a
// half-frame at the end is normal and must not lose the whole frames before it.
func TestATruncatedTailKeepsWhatCameBefore(t *testing.T) {
	whole := frame(true, OpText, []byte("first"), nil)
	partial := frame(true, OpText, []byte("second"), nil)

	for cut := 1; cut < len(partial); cut++ {
		frames, rest := Deframe(append(append([]byte{}, whole...), partial[:cut]...))
		if len(frames) != 1 {
			t.Fatalf("cut at %d: %d frames, want the one complete frame", cut, len(frames))
		}
		if frames[0].Text != "first" {
			t.Errorf("cut at %d: got %q", cut, frames[0].Text)
		}
		if rest != cut {
			t.Errorf("cut at %d: remainder = %d, want %d", cut, rest, cut)
		}
	}
}

// The two extended length forms are where a hand-written parser goes wrong, and
// they are the ordinary case for any real payload.
func TestTheExtendedLengthForms(t *testing.T) {
	for _, size := range []int{125, 126, 1000, 1 << 16, (1 << 16) + 1} {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = 'x'
		}
		frames, rest := Deframe(frame(true, OpBinary, payload, nil))
		if rest != 0 {
			t.Errorf("size %d: %d bytes left over", size, rest)
			continue
		}
		if len(frames) != 1 || frames[0].Size != int64(size) {
			t.Errorf("size %d: got %d frames, size %v", size, len(frames), frames)
		}
	}
}

// A close frame carries why, which is usually the answer to why the socket
// stopped working.
func TestACloseFrameReportsItsCodeAndReason(t *testing.T) {
	payload := binary.BigEndian.AppendUint16(nil, 1011)
	payload = append(payload, []byte("upstream is gone")...)

	frames, _ := Deframe(frame(true, OpClose, payload, nil))
	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	if frames[0].CloseCode != 1011 {
		t.Errorf("close code = %d, want 1011", frames[0].CloseCode)
	}
	if frames[0].CloseReason != "upstream is gone" {
		t.Errorf("reason = %q", frames[0].CloseReason)
	}

	// An empty close frame is legal and must not be read as code zero with a
	// reason nobody sent.
	frames, _ = Deframe(frame(true, OpClose, nil, nil))
	if frames[0].CloseCode != 0 || frames[0].CloseReason != "" {
		t.Errorf("an empty close was read as %+v", frames[0])
	}
}

// Payloads are routinely not text, and claiming otherwise is the kind of
// confident nonsense the product principles rule out.
func TestABinaryPayloadIsNotClaimedAsText(t *testing.T) {
	frames, _ := Deframe(frame(true, OpBinary, []byte{0xff, 0xfe, 0x00, 0x01}, nil))
	if frames[0].Text != "" {
		t.Errorf("binary bytes were reported as text: %q", frames[0].Text)
	}
	if frames[0].Size != 4 {
		t.Errorf("size = %d, want 4", frames[0].Size)
	}
}

// Invalid UTF-8 in a text frame happens with badly encoded payloads, which the
// product notes are routine here.
func TestInvalidUTF8IsNotReportedAsText(t *testing.T) {
	frames, _ := Deframe(frame(true, OpText, []byte{0x48, 0xC3, 0x28}, nil))
	if frames[0].Text != "" {
		t.Errorf("invalid UTF-8 was reported as text: %q", frames[0].Text)
	}
}

// A length that cannot be real is a corrupt or hostile stream, and reading on
// would mean trusting it to size an allocation.
func TestAnAbsurdLengthIsRefusedRatherThanAllocated(t *testing.T) {
	header := []byte{0x82, 127}
	header = binary.BigEndian.AppendUint64(header, 1<<63)

	frames, rest := Deframe(header)
	if len(frames) != 0 {
		t.Errorf("%d frames from a corrupt header", len(frames))
	}
	if rest != len(header) {
		t.Errorf("remainder = %d, want the whole buffer back", rest)
	}
}

// A fragmented message arrives as a first frame plus continuations. Sonda
// reports the frames as they crossed rather than gluing them: the fragmentation
// is part of what happened.
func TestFragmentsAreReportedAsTheyCrossed(t *testing.T) {
	var stream []byte
	stream = append(stream, frame(false, OpText, []byte("hel"), nil)...)
	stream = append(stream, frame(true, OpContinuation, []byte("lo"), nil)...)

	frames, _ := Deframe(stream)
	if len(frames) != 2 {
		t.Fatalf("%d frames, want 2", len(frames))
	}
	if frames[0].Final || frames[0].Kind != "text" {
		t.Errorf("first frame: final=%v kind=%q", frames[0].Final, frames[0].Kind)
	}
	if !frames[1].Final || frames[1].Kind != "continuation" {
		t.Errorf("second frame: final=%v kind=%q", frames[1].Final, frames[1].Kind)
	}
}

func TestSummariseCountsByKind(t *testing.T) {
	var stream []byte
	stream = append(stream, frame(true, OpText, []byte("a"), nil)...)
	stream = append(stream, frame(true, OpText, []byte("b"), nil)...)
	stream = append(stream, frame(true, OpPing, nil, nil)...)

	frames, _ := Deframe(stream)
	got := Summarise(frames)
	if !strings.Contains(got, "2 text") || !strings.Contains(got, "1 ping") {
		t.Errorf("summary = %q", got)
	}
	if Summarise(nil) != "no frames" {
		t.Errorf("empty summary = %q", Summarise(nil))
	}
}

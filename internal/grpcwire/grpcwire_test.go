package grpcwire

import (
	"encoding/binary"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func frame(data []byte, compressed bool) []byte {
	out := make([]byte, headerSize+len(data))
	if compressed {
		out[0] = 1
	}
	binary.BigEndian.PutUint32(out[1:headerSize], uint32(len(data)))
	copy(out[headerSize:], data)
	return out
}

func TestDeframe(t *testing.T) {
	a, b := []byte("first"), []byte("second message")

	t.Run("several messages in one body", func(t *testing.T) {
		body := append(frame(a, false), frame(b, false)...)
		frames, remainder := Deframe(body)
		if len(frames) != 2 || remainder != 0 {
			t.Fatalf("got %d frames, %d bytes left over", len(frames), remainder)
		}
		if string(frames[0].Data) != "first" || string(frames[1].Data) != "second message" {
			t.Errorf("frames = %q %q", frames[0].Data, frames[1].Data)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		frames, remainder := Deframe(nil)
		if len(frames) != 0 || remainder != 0 {
			t.Errorf("got %d frames, %d left over", len(frames), remainder)
		}
	})

	t.Run("zero-length message is still a message", func(t *testing.T) {
		frames, remainder := Deframe(frame(nil, false))
		if len(frames) != 1 || remainder != 0 {
			t.Fatalf("got %d frames, %d left over", len(frames), remainder)
		}
		if len(frames[0].Data) != 0 {
			t.Errorf("data = %q, want empty", frames[0].Data)
		}
	})

	t.Run("the compression flag is reported", func(t *testing.T) {
		frames, _ := Deframe(frame(a, true))
		if len(frames) != 1 || !frames[0].Compressed {
			t.Error("the compression flag was lost")
		}
	})

	// A capture cut short by the body cap, or a stream that was still running,
	// ends mid-frame. That is normal and must not lose the whole body.
	t.Run("a body cut off mid-payload keeps the whole messages", func(t *testing.T) {
		body := append(frame(a, false), frame(b, false)...)
		truncated := body[:len(body)-4]

		frames, remainder := Deframe(truncated)
		if len(frames) != 1 {
			t.Fatalf("got %d frames, want the one complete message", len(frames))
		}
		if string(frames[0].Data) != "first" {
			t.Errorf("frame = %q", frames[0].Data)
		}
		if remainder == 0 {
			t.Error("the dangling bytes should be reported as a remainder")
		}
	})

	t.Run("a body cut off inside the header", func(t *testing.T) {
		body := append(frame(a, false), 0x00, 0x00)
		frames, remainder := Deframe(body)
		if len(frames) != 1 || remainder != 2 {
			t.Errorf("got %d frames, %d left over, want 1 and 2", len(frames), remainder)
		}
	})

	// A length field larger than what was captured must not panic or allocate
	// wildly; it just means the frame is incomplete.
	t.Run("a length that overruns the body", func(t *testing.T) {
		body := []byte{0, 0xff, 0xff, 0xff, 0xff, 'x'}
		frames, remainder := Deframe(body)
		if len(frames) != 0 {
			t.Errorf("got %d frames, want none", len(frames))
		}
		if remainder != len(body) {
			t.Errorf("remainder = %d, want %d", remainder, len(body))
		}
	})
}

func TestMethodParts(t *testing.T) {
	cases := []struct {
		path            string
		service, method string
		ok              bool
	}{
		{"/demo.v1.Orders/GetOrder", "demo.v1.Orders", "GetOrder", true},
		{"/pkg.sub.Service/Method?x=1", "pkg.sub.Service", "Method", true},
		{"/NoPackageService/Do", "NoPackageService", "Do", true},
		{"/incomplete", "", "", false},
		{"/trailing/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		service, method, ok := MethodParts(tc.path)
		if service != tc.service || method != tc.method || ok != tc.ok {
			t.Errorf("MethodParts(%q) = %q, %q, %v; want %q, %q, %v",
				tc.path, service, method, ok, tc.service, tc.method, tc.ok)
		}
	}
}

func TestExplain(t *testing.T) {
	// Field 1: a string. Field 2: a varint. Field 3: a nested message holding
	// its own string. This is the shape Explain has to make readable with no
	// schema at all.
	nested := protowire.AppendTag(nil, 1, protowire.BytesType)
	nested = protowire.AppendBytes(nested, []byte("CLP"))

	msg := protowire.AppendTag(nil, 1, protowire.BytesType)
	msg = protowire.AppendBytes(msg, []byte("Comercial Andes SpA"))
	msg = protowire.AppendTag(msg, 2, protowire.VarintType)
	msg = protowire.AppendVarint(msg, 42)
	msg = protowire.AppendTag(msg, 3, protowire.BytesType)
	msg = protowire.AppendBytes(msg, nested)

	fields, err := Explain(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(fields))
	}

	// Readable text is shown as text. Rendering a customer name as a tree of
	// meaningless field numbers would help nobody.
	if fields[0].Type != "string" || fields[0].Value != "Comercial Andes SpA" {
		t.Errorf("field 1 = %+v, want the string", fields[0])
	}
	if fields[1].Type != "varint" || fields[1].Value != uint64(42) {
		t.Errorf("field 2 = %+v, want varint 42", fields[1])
	}
	if fields[2].Type != "message" {
		t.Fatalf("field 3 = %+v, want a nested message", fields[2])
	}
	inner, ok := fields[2].Value.([]Field)
	if !ok || len(inner) != 1 || inner[0].Value != "CLP" {
		t.Errorf("nested fields = %+v", fields[2].Value)
	}
}

func TestExplainMarksGuesses(t *testing.T) {
	msg := protowire.AppendTag(nil, 1, protowire.VarintType)
	msg = protowire.AppendVarint(msg, 1)

	fields, err := Explain(msg)
	if err != nil {
		t.Fatal(err)
	}
	// On the wire a varint of 1 could be the number one, true, or the first
	// value of an enum. Saying so is the difference between a useful view and
	// a misleading one.
	if fields[0].Note == "" {
		t.Error("an ambiguous varint should carry a note saying so")
	}
}

func TestExplainRejectsGarbage(t *testing.T) {
	// A tag announcing a length-delimited field with no length after it.
	garbage := []byte{0x0a}
	if _, err := Explain(garbage); err == nil {
		t.Error("expected an error for a truncated field")
	}
}

func TestExplainFallsBackToBase64(t *testing.T) {
	// Bytes that are neither readable text nor a parseable message.
	blob := []byte{0xff, 0xfe, 0xfd, 0xfc}
	msg := protowire.AppendTag(nil, 1, protowire.BytesType)
	msg = protowire.AppendBytes(msg, blob)

	fields, err := Explain(msg)
	if err != nil {
		t.Fatal(err)
	}
	if fields[0].Type != "bytes" {
		t.Errorf("type = %q, want bytes", fields[0].Type)
	}
	if fields[0].Value != "//79/A==" {
		t.Errorf("value = %v, want base64", fields[0].Value)
	}
}

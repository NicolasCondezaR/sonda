// Package grpcwire understands the two layers of framing under a gRPC call,
// without needing to know what the messages mean.
//
// A gRPC body is not one message. It is a sequence of length-prefixed frames,
// and that framing does not line up with HTTP/2 frames — one HTTP/2 DATA frame
// can hold several messages, or a fraction of one. Deframe recovers the message
// boundaries. Explain then reads a single message straight off the wire format,
// which is what lets Sonda show something useful even when no schema is
// available.
package grpcwire

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

// headerSize is the gRPC length prefix: one compression flag plus a four-byte
// big-endian length.
const headerSize = 5

type Frame struct {
	// Compressed reports the frame's compression flag. Sonda does not
	// decompress: the encoding lives in the grpc-encoding header and is
	// negotiated per call, so a compressed payload is reported honestly rather
	// than guessed at.
	Compressed bool
	Data       []byte
}

// Deframe splits a captured body into messages.
//
// A capture can be cut short by the body cap or by a stream that was still
// running, so a trailing partial frame is normal rather than an error:
// remainder reports how many bytes were left dangling.
func Deframe(body []byte) (frames []Frame, remainder int) {
	rest := body
	for len(rest) >= headerSize {
		length := binary.BigEndian.Uint32(rest[1:headerSize])
		end := headerSize + int(length)
		// A length that overruns what was captured means the frame is cut off,
		// not that the stream is corrupt.
		if end < headerSize || end > len(rest) {
			break
		}
		frames = append(frames, Frame{
			Compressed: rest[0] != 0,
			Data:       rest[headerSize:end],
		})
		rest = rest[end:]
	}
	return frames, len(rest)
}

// MethodParts splits an HTTP/2 :path of the form /package.Service/Method.
func MethodParts(path string) (service, method string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if idx := strings.IndexByte(trimmed, '?'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	slash := strings.LastIndexByte(trimmed, '/')
	if slash <= 0 || slash == len(trimmed)-1 {
		return "", "", false
	}
	return trimmed[:slash], trimmed[slash+1:], true
}

// Field is one entry of the schema-free view of a message: what the wire format
// itself can tell us, which is a field number, a wire type, and a value whose
// interpretation is a guess.
type Field struct {
	Number int32  `json:"number"`
	Type   string `json:"type"`
	Value  any    `json:"value"`
	// Note explains a guess that could be wrong, so the reader knows which
	// parts of the view to distrust.
	Note string `json:"note,omitempty"`
}

// Explain decodes a message using only the wire format.
//
// Without a schema there are no field names and no types: on the wire, a
// varint could be an int32, a bool or an enum, and a length-delimited field
// could be a string, a nested message or raw bytes. The values below are
// labelled guesses, not decoded truth — but a numbered field with a plausible
// value is far more useful than a base64 blob.
func Explain(message []byte) ([]Field, error) {
	var fields []Field
	rest := message

	for len(rest) > 0 {
		number, wireType, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return fields, fmt.Errorf("field %d: %w", len(fields)+1, protowire.ParseError(n))
		}
		rest = rest[n:]

		field := Field{Number: int32(number)}
		switch wireType {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(rest)
			if n < 0 {
				return fields, protowire.ParseError(n)
			}
			rest = rest[n:]
			field.Type = "varint"
			field.Value = v
			field.Note = "could be an integer, a bool or an enum"

		case protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(rest)
			if n < 0 {
				return fields, protowire.ParseError(n)
			}
			rest = rest[n:]
			field.Type = "fixed32"
			field.Value = v

		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(rest)
			if n < 0 {
				return fields, protowire.ParseError(n)
			}
			rest = rest[n:]
			field.Type = "fixed64"
			field.Value = v

		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(rest)
			if n < 0 {
				return fields, protowire.ParseError(n)
			}
			rest = rest[n:]
			field.Type, field.Value, field.Note = interpretBytes(v)

		case protowire.StartGroupType:
			v, n := protowire.ConsumeGroup(number, rest)
			if n < 0 {
				return fields, protowire.ParseError(n)
			}
			rest = rest[n:]
			field.Type = "group"
			if nested, err := Explain(v); err == nil {
				field.Value = nested
			} else {
				field.Value = base64.StdEncoding.EncodeToString(v)
			}

		default:
			return fields, fmt.Errorf("field %d: unknown wire type %d", number, wireType)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// interpretBytes guesses what a length-delimited field holds.
//
// Text is tried before a nested message on purpose. Both are plausible for the
// same bytes, but a nested message carries field tags, which are control
// characters, so readable text is rarely a valid message by accident — whereas
// short strings often do parse as one, and rendering "Comercial Andes SpA" as
// a tree of meaningless field numbers helps nobody.
func interpretBytes(v []byte) (kind string, value any, note string) {
	if len(v) == 0 {
		return "bytes", "", "empty"
	}
	if isReadableText(v) {
		return "string", string(v), ""
	}
	if nested, err := Explain(v); err == nil && len(nested) > 0 {
		return "message", nested, "guessed to be a nested message"
	}
	return "bytes", base64.StdEncoding.EncodeToString(v), "not readable as text or as a message"
}

func isReadableText(v []byte) bool {
	if !utf8.Valid(v) {
		return false
	}
	for _, r := range string(v) {
		// Tab, newline and carriage return are fine; other control characters
		// mean this is structure, not prose.
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
		if r == 0x7f {
			return false
		}
	}
	return true
}

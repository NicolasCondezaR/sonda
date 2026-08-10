// Package wsframe reads the WebSocket wire format.
//
// It is the counterpart of grpcwire, and it exists for the same reason: what
// crossed the wire is a stream of framed messages, Sonda stores the stream
// exactly as it arrived, and turning it back into messages is a view computed
// when someone looks. Re-serializing on the way in would lose whatever the
// parser did not understand, which is the one thing a capture must not do.
//
// The format is RFC 6455 section 5. A frame is a two-byte header, an optional
// extended length, an optional masking key, and a payload.
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-------+-+-------------+-------------------------------+
//	|F|R|R|R| opcode|M| Payload len |    Extended payload length    |
//	|I|S|S|S|  (4)  |A|     (7)     |             (16/64)           |
//	|N|V|V|V|       |S|             |                               |
//	| |1|2|3|       |K|             |                               |
//	+-+-+-+-+-------+-+-------------+-------------------------------+
package wsframe

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Opcode values that matter. The rest are reserved and reported by number.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// Frame is one frame as it crossed, already unmasked.
//
// Unmasking is not a modification of the record: the mask is transport
// scaffolding the receiving end removes before anything reads the payload, and
// keeping it would mean storing bytes no participant ever saw as data. The key
// is kept so the frame can be reproduced exactly.
type Frame struct {
	Final   bool   `json:"final"`
	Opcode  int    `json:"opcode"`
	Kind    string `json:"kind"`
	Payload []byte `json:"-"`
	Size    int64  `json:"size"`

	// Masked and MaskKey record what the transport did, for a reader who needs
	// to know the frame really came from a client.
	Masked  bool   `json:"masked,omitempty"`
	MaskKey uint32 `json:"mask_key,omitempty"`

	// Text is the payload when it is valid UTF-8, which is what a text frame
	// promises and what almost every application sends.
	Text string `json:"text,omitempty"`

	// Close carries the two fields a close frame may hold.
	CloseCode   int    `json:"close_code,omitempty"`
	CloseReason string `json:"close_reason,omitempty"`
}

func kindOf(opcode int) string {
	switch opcode {
	case OpContinuation:
		return "continuation"
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	default:
		return fmt.Sprintf("reserved(0x%X)", opcode)
	}
}

// Deframe reads as many whole frames as the buffer holds and reports how many
// bytes were left over.
//
// A remainder is not an error. A capture is a prefix of a conversation that may
// still be running, and truncating one mid-frame is the normal case — the same
// contract grpcwire.Deframe has, for the same reason.
func Deframe(stream []byte) (frames []Frame, remainder int) {
	rest := stream

	for len(rest) >= 2 {
		first, second := rest[0], rest[1]
		masked := second&0x80 != 0
		length := int64(second & 0x7F)

		offset := 2
		switch length {
		case 126:
			if len(rest) < offset+2 {
				return frames, len(rest)
			}
			length = int64(binary.BigEndian.Uint16(rest[offset:]))
			offset += 2
		case 127:
			if len(rest) < offset+8 {
				return frames, len(rest)
			}
			// The high bit must be zero per the specification. A length that
			// large is a corrupt or hostile stream, and reading on would mean
			// trusting it to size an allocation.
			raw := binary.BigEndian.Uint64(rest[offset:])
			if raw > 1<<62 {
				return frames, len(rest)
			}
			length = int64(raw)
			offset += 8
		}

		var key [4]byte
		if masked {
			if len(rest) < offset+4 {
				return frames, len(rest)
			}
			copy(key[:], rest[offset:offset+4])
			offset += 4
		}

		end := offset + int(length)
		if length < 0 || end < offset || len(rest) < end {
			// The frame is not all here yet.
			return frames, len(rest)
		}

		payload := make([]byte, length)
		copy(payload, rest[offset:end])
		if masked {
			for i := range payload {
				payload[i] ^= key[i%4]
			}
		}

		f := Frame{
			Final:   first&0x80 != 0,
			Opcode:  int(first & 0x0F),
			Payload: payload,
			Size:    length,
			Masked:  masked,
		}
		f.Kind = kindOf(f.Opcode)
		if masked {
			f.MaskKey = binary.BigEndian.Uint32(key[:])
		}
		describe(&f)

		frames = append(frames, f)
		rest = rest[end:]
	}

	return frames, len(rest)
}

// describe fills in whatever can be read from the payload without guessing.
func describe(f *Frame) {
	switch f.Opcode {
	case OpClose:
		// A close frame is empty, or a two-byte code and an optional reason.
		if len(f.Payload) >= 2 {
			f.CloseCode = int(binary.BigEndian.Uint16(f.Payload))
			if reason := f.Payload[2:]; utf8.Valid(reason) {
				f.CloseReason = string(reason)
			}
		}
	case OpText, OpContinuation, OpPing, OpPong:
		// Only when it really is valid UTF-8. Sonda never claims a payload is
		// text it could not decode; the bytes stay available either way.
		if utf8.Valid(f.Payload) {
			f.Text = string(f.Payload)
		}
	}
}

// Summarise gives a one-line reading of a conversation, for a listing that has
// no room for the frames themselves.
func Summarise(frames []Frame) string {
	if len(frames) == 0 {
		return "no frames"
	}
	counts := map[string]int{}
	order := []string{}
	for _, f := range frames {
		if counts[f.Kind] == 0 {
			order = append(order, f.Kind)
		}
		counts[f.Kind]++
	}

	parts := make([]string, 0, len(order))
	for _, kind := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	return strings.Join(parts, ", ")
}

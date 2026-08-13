package amqpwire

import "encoding/binary"

// BlankCredentials returns a copy of a complete AMQP frame stream with SASL
// challenge and response values replaced by zero bytes.
//
// The decoder deliberately never exposes these values, but a capture stores
// the wire bytes too. PLAIN carries "\x00user\x00password" as the response, so
// excluding Payload from JSON is not enough: the stored byte stream itself
// must be safe before it reaches SQLite or any raw-body surface.
//
// Only the copy is changed. Callers may pass a relay buffer that is still being
// forwarded to the broker; mutating it would break authentication and violate
// the proxy's byte-exact forwarding guarantee.
func BlankCredentials(stream []byte) []byte {
	out := append([]byte(nil), stream...)
	rest := out

	for len(rest) > 0 {
		if len(rest) >= 4 && string(rest[:4]) == "AMQP" {
			if len(rest) < protocolHeaderSize {
				break
			}
			rest = rest[protocolHeaderSize:]
			continue
		}
		if len(rest) < frameHeaderSize+1 {
			break
		}
		size := binary.BigEndian.Uint32(rest[3:7])
		if size > maxFrame {
			break
		}
		end := frameHeaderSize + int(size) + 1
		if len(rest) < end || rest[end-1] != frameEnd {
			break
		}
		if rest[0] == frameMethod {
			blankMethodCredentials(rest[frameHeaderSize : end-1])
		}
		rest = rest[end:]
	}
	return out
}

func blankMethodCredentials(payload []byte) {
	if len(payload) < 4 || binary.BigEndian.Uint16(payload[:2]) != 10 {
		return
	}
	method := binary.BigEndian.Uint16(payload[2:4])
	off := 4

	// Parse the complete argument shape before preserving any of it. These are
	// credential-bearing methods, so a malformed inner length cannot be treated
	// as an ordinary decoder error: if a boundary is not trustworthy, retain the
	// class and method for diagnosis and blank every argument after them.
	switch method {
	case 11: // connection.start-ok: table, mechanism, response, locale
		var ok bool
		if off, ok = skipLong(payload, off); !ok {
			blank(payload[4:])
			return
		}
		if off, ok = skipShort(payload, off); !ok {
			blank(payload[4:])
			return
		}
		responseStart := off
		if off, ok = skipLong(payload, off); !ok {
			blank(payload[4:])
			return
		}
		responseEnd := off
		if off, ok = skipShort(payload, off); !ok || off != len(payload) {
			blank(payload[4:])
			return
		}
		blank(payload[responseStart+4 : responseEnd])
	case 20, 21: // connection.secure / connection.secure-ok
		end, ok := skipLong(payload, off)
		if !ok || end != len(payload) {
			blank(payload[4:])
			return
		}
		blank(payload[off+4 : end])
	}
}

func skipShort(b []byte, off int) (int, bool) {
	if off >= len(b) {
		return off, false
	}
	end := off + 1 + int(b[off])
	return end, end <= len(b)
}

func skipLong(b []byte, off int) (int, bool) {
	if off+4 > len(b) {
		return off, false
	}
	end64 := int64(off) + 4 + int64(binary.BigEndian.Uint32(b[off:off+4]))
	if end64 > int64(len(b)) {
		return off, false
	}
	return int(end64), true
}

func blank(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

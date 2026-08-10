package pgwire

import "encoding/binary"

// A Postgres handshake carries the password. Deframe already refuses to decode
// it and Message.Payload never reaches JSON, but a decoder that declines to
// look is not protection: the stored capture is the raw stream, and from there
// the secret sits in sonda.db in plaintext and can reach an agent over MCP —
// whose redaction walks headers and JSON keys, which a TCP stream is neither.
//
// So the credential-bearing bodies are overwritten as they go past, before
// anything is kept. What is never written cannot leak, and there is no display
// path left to forget about.
//
// The framing survives untouched: same type byte, same length, same number of
// bytes. The capture still deframes, and a reader still sees that an
// authentication happened and by which mechanism — only the secret is gone.
//
// This is a rewrite of the *copy*, never of what is forwarded. Blank returns
// the original slice unchanged when there was nothing to blank, and a fresh
// copy when there was; the bytes the upstream receives are never touched.

// fill replaces a secret byte for byte, so every length field in the stream
// stays true. A run of asterisks in a hexdump reads as a deliberate blank
// rather than as data, which zeros would not.
const fill = '*'

// Blanker rewrites one direction of a Postgres stream as it flows past.
//
// It is a state machine over frames rather than a search for a pattern: a
// password is not recognisable by its bytes, only by its position, and TCP
// delivers that position split across arbitrary chunks. The machine therefore
// carries across calls how much of the current message is still to come and
// whether those bytes are the secret.
type Blanker struct {
	fromClient bool

	// startup is set while the next message is the untyped first one. It goes
	// back to true after an SSLRequest, because a server that refuses
	// encryption gets a second, ordinary startup message on the same
	// connection — and that is the negotiation psql and pgx do by default, so
	// losing the state there would lose the password that follows.
	startup bool

	// sslReply covers the one byte that is not a message at all: the server's
	// answer to an SSLRequest is a bare 'S' or 'N', with no length and no body.
	// Framing it would swallow four bytes of whatever came next and desync the
	// direction that carries the cancellation key.
	//
	// It is decided without being told what the client asked, because it can
	// be: a backend's first message is Authentication, ErrorResponse or
	// NegotiateProtocolVersion, never 'S', 'N' or 'G'.
	sslReply bool

	header    [8]byte
	headerLen int

	// copyLeft then blankLeft describe the rest of the current body: a run to
	// pass through, then a run to overwrite. Two runs rather than a flag
	// because an Authentication message keeps its mechanism code and loses
	// everything after it.
	copyLeft  int
	blankLeft int

	// stopped means the stream is no longer protocol messages. Reading TLS
	// records as frames would blank bytes at meaningless offsets, and there is
	// no plaintext secret left to protect once the connection is encrypted.
	stopped bool

	// msgType is the type byte of the message currently being drained, kept for
	// OnMessage. A startup message has none and reports 0, which no typed
	// message can be.
	msgType byte

	// OnMessage, when set, is called once per whole message with the offset one
	// past its last byte in the chunk that completed it. Nothing about blanking
	// depends on it and a nil one changes nothing.
	//
	// It lives here rather than in a framer of its own because this is the only
	// state machine in the process that already frames a live stream message by
	// message, and the parts of that which are easy to get wrong — the untyped
	// startup message, the bare byte that answers an SSLRequest, the point
	// where the stream turns into TLS and framing has to stop — are exactly the
	// parts a second copy would get wrong differently. The capture is split
	// into one call per statement on these boundaries, so a framer that
	// disagreed with the blanker would put the split in a different place than
	// the blanking.
	OnMessage func(typ byte, end int)
}

// NewBlanker returns a blanker for one direction. The direction matters for the
// same reason it matters to Deframe: the same type byte means different
// messages on each side, and 'p' from the client is the password while 'p' from
// the server is nothing at all.
func NewBlanker(fromClient bool) *Blanker {
	return &Blanker{fromClient: fromClient, startup: fromClient, sslReply: !fromClient}
}

// Encrypted reports that the stream stopped being readable protocol messages.
func (b *Blanker) Encrypted() bool { return b.stopped }

// Blank returns chunk with any credential bytes in it overwritten.
//
// The result aliases chunk when nothing changed, and is a fresh slice when
// something did; either way the caller must copy it before the next call. The
// header bytes of a message go out verbatim as they arrive — they are the
// framing, never the secret — so nothing is ever held back and a stream that
// stops mid-message leaves no secret unwritten.
func (b *Blanker) Blank(chunk []byte) []byte {
	if b.stopped || len(chunk) == 0 {
		return chunk
	}

	out := chunk
	copied := false

	for i := 0; i < len(chunk); {
		switch {
		case b.sslReply:
			b.sslReply = false
			switch chunk[i] {
			case 'S', 'G':
				// Encryption was agreed. Everything after this byte is a TLS or
				// GSSAPI record, and there is no plaintext secret left to find.
				b.stopped = true
				return out
			case 'N':
				i++ // one bare byte, not a frame
			}
			// Anything else is a real type byte, left for the header below.

		case b.copyLeft > 0:
			n := min(b.copyLeft, len(chunk)-i)
			b.copyLeft -= n
			i += n
			if b.copyLeft == 0 && b.blankLeft == 0 {
				b.report(i)
			}

		case b.blankLeft > 0:
			n := min(b.blankLeft, len(chunk)-i)
			if !copied {
				// Copied only now, so the ordinary chunk — which is nearly
				// every chunk — costs nothing.
				out = append([]byte(nil), chunk...)
				copied = true
			}
			for j := i; j < i+n; j++ {
				out[j] = fill
			}
			b.blankLeft -= n
			i += n
			if b.blankLeft == 0 {
				b.report(i)
			}

		default:
			need := 5
			if b.startup {
				need = 8 // a length and a protocol code, with no type byte
			}
			n := min(need-b.headerLen, len(chunk)-i)
			copy(b.header[b.headerLen:], chunk[i:i+n])
			b.headerLen += n
			i += n
			if b.headerLen < need {
				return out // the rest of the header is in the next chunk
			}
			b.headerLen = 0
			startup := b.startup
			if b.readHeader(); b.stopped {
				return out
			}
			b.msgType = 0
			if !startup {
				b.msgType = b.header[0]
			}
			// A message with no body at all — Sync, Terminate, an SSLRequest —
			// is whole the moment its header is, and would otherwise never be
			// reported: there is nothing left to drain.
			if b.copyLeft == 0 && b.blankLeft == 0 {
				b.report(i)
			}
		}
	}
	return out
}

func (b *Blanker) report(end int) {
	if b.OnMessage != nil {
		b.OnMessage(b.msgType, end)
	}
}

// readHeader works out what the message just framed is and how much of its body
// has to disappear.
func (b *Blanker) readHeader() {
	if b.startup {
		b.readStartupHeader()
		return
	}

	length := binary.BigEndian.Uint32(b.header[1:5])
	if length < 4 || length > maxMessage {
		// The same refusal Deframe makes, and for the same reason: a length no
		// sender produces means this is not the protocol any more.
		b.stopped = true
		return
	}
	body := int(length) - 4

	switch typ := b.header[0]; {
	case b.fromClient && typ == 'p':
		// PasswordMessage, SASLInitialResponse, SASLResponse and GSSResponse all
		// share this type byte, and every one of them is the secret itself.
		b.blankLeft = body

	case !b.fromClient && typ == 'R':
		// Authentication. The first four bytes are the sub-code — which
		// mechanism was demanded, and the only part of this message pgwire
		// decodes — so they stay. Everything after is the SASL exchange or the
		// MD5 salt, and goes.
		b.copyLeft = min(4, body)
		b.blankLeft = body - b.copyLeft

	case !b.fromClient && typ == 'K':
		// BackendKeyData is the cancellation key: a credential whose only use is
		// cancelling someone else's query.
		b.blankLeft = body

	default:
		b.copyLeft = body
	}
}

func (b *Blanker) readStartupHeader() {
	if !looksLikeStartup(b.header[:]) {
		// Not a startup message and not a typed one either: the connection went
		// to TLS after an SSLRequest, so there is nothing further to read.
		b.stopped = true
		return
	}
	length := int(binary.BigEndian.Uint32(b.header[0:4]))
	code := binary.BigEndian.Uint32(b.header[4:8])
	body := length - 8

	switch code {
	case codeSSLRequest, codeGSSENCRequest:
		b.copyLeft = body
		// Either TLS follows, or the server refused and an ordinary startup
		// message does. Staying in startup state lets the next header decide.
		return
	case codeCancelRequest:
		// The process id and the secret key. Neither is decoded and the pair is
		// a credential.
		b.blankLeft = body
	default:
		b.copyLeft = body
	}
	b.startup = false
}

package pgwire

import (
	"bytes"
	"testing"
)

// password is deliberately a string that would be trivially greppable if it
// survived anywhere.
const password = "hunter2-do-not-store-me"

// blankAll runs a whole stream through in one chunk.
func blankAll(fromClient bool, stream []byte) []byte {
	return append([]byte(nil), NewBlanker(fromClient).Blank(stream)...)
}

// blankInChunks feeds the stream a few bytes at a time, which is what TCP
// actually does and the only way to prove the state machine survives a message
// split across writes.
func blankInChunks(fromClient bool, stream []byte, size int) []byte {
	b := NewBlanker(fromClient)
	var out []byte
	for i := 0; i < len(stream); i += size {
		out = append(out, b.Blank(stream[i:min(i+size, len(stream))])...)
	}
	return out
}

func TestACleartextPasswordNeverReachesTheCapture(t *testing.T) {
	stream := cat(
		startup(196608, "user", "app", "database", "orders"),
		msg('p', cstr(password)),
		msg('Q', cstr("SELECT 1")),
	)

	for _, size := range []int{len(stream), 1, 3, 7} {
		got := blankInChunks(true, stream, size)

		if bytes.Contains(got, []byte(password)) {
			t.Fatalf("chunks of %d: the password is in the stored bytes", size)
		}
		if len(got) != len(stream) {
			t.Fatalf("chunks of %d: %d bytes out of %d in, the framing moved", size, len(got), len(stream))
		}
		// The parts that are not the secret have to be untouched, or the
		// capture stops being what crossed the wire.
		if !bytes.Contains(got, []byte("SELECT 1")) {
			t.Errorf("chunks of %d: the query was blanked too", size)
		}
		if !bytes.Contains(got, []byte("orders")) {
			t.Errorf("chunks of %d: the startup parameters were blanked too", size)
		}
	}
}

// The point of keeping the framing is that the capture still reads back as a
// conversation. A blanked stream that no longer deframes would have traded one
// gap for a bigger one.
func TestABlankedStreamStillDeframes(t *testing.T) {
	stream := cat(
		startup(196608, "user", "app", "database", "orders"),
		msg('p', cstr(password)),
		msg('Q', cstr("SELECT 1")),
	)

	msgs, rest := Deframe(blankAll(true, stream), true)
	if rest != 0 {
		t.Fatalf("%d bytes left over: the blanked stream no longer frames", rest)
	}
	if len(msgs) != 3 {
		t.Fatalf("%d messages, want 3", len(msgs))
	}
	if msgs[0].Parameters["database"] != "orders" {
		t.Errorf("startup parameters = %v", msgs[0].Parameters)
	}
	// Still visibly an authentication step, which is the fact a reader needs.
	if msgs[1].Kind != "authentication_response" {
		t.Errorf("second message = %q, want the password message still framed", msgs[1].Kind)
	}
	if msgs[2].SQL != "SELECT 1" {
		t.Errorf("sql = %q", msgs[2].SQL)
	}
}

// SCRAM is what any current server negotiates, and both halves of it are
// secrets: the client proof going up and the server signature coming back.
func TestTheSASLExchangeIsBlankedInBothDirections(t *testing.T) {
	const clientProof = "n,,n=,r=clientnonce-and-proof"
	const serverFinal = "v=server-signature-value"

	fromClient := cat(
		startup(196608, "user", "app"),
		msg('p', cstr("SCRAM-SHA-256"), i32(int32(len(clientProof))), []byte(clientProof)),
	)
	got := blankAll(true, fromClient)
	if bytes.Contains(got, []byte(clientProof)) {
		t.Error("the SASL client proof is in the stored bytes")
	}
	if bytes.Contains(got, []byte("SCRAM-SHA-256")) {
		t.Error("the whole SASLInitialResponse body is a credential exchange and none of it is kept")
	}

	fromServer := cat(
		msg('R', u32(10), cstr("SCRAM-SHA-256"), []byte{0}), // AuthenticationSASL
		msg('R', u32(12), []byte(serverFinal)),              // AuthenticationSASLFinal
		msg('R', u32(0)),                                    // AuthenticationOk
	)
	got = blankAll(false, fromServer)
	if bytes.Contains(got, []byte(serverFinal)) {
		t.Error("the SASL server signature is in the stored bytes")
	}

	// The mechanism has to survive, because "an authentication happened, by
	// SASL" is exactly the fact a reader is entitled to.
	msgs, rest := Deframe(got, false)
	if rest != 0 {
		t.Fatalf("%d bytes left over", rest)
	}
	want := []string{"sasl", "sasl_final", "ok"}
	for i, kind := range want {
		if msgs[i].Auth != kind {
			t.Errorf("message %d auth = %q, want %q", i, msgs[i].Auth, kind)
		}
	}
}

func TestTheCancellationKeyIsBlankedOnBothSides(t *testing.T) {
	const secret = 0x7ABBCCDD

	// The server hands the key out in BackendKeyData.
	fromServer := cat(msg('K', i32(4242), i32(secret)), msg('Z', []byte{'I'}))
	got := blankAll(false, fromServer)
	if bytes.Contains(got, i32(secret)) {
		t.Error("the cancellation key is in the stored bytes")
	}
	msgs, _ := Deframe(got, false)
	if len(msgs) != 2 || msgs[0].Kind != "backend_key_data" || msgs[1].TxStatus != "idle" {
		t.Errorf("blanking BackendKeyData broke the frames after it: %+v", msgs)
	}

	// A client presents it back on a second connection to cancel a query.
	fromClient := append(u32(16), append(u32(codeCancelRequest), append(i32(4242), i32(secret)...)...)...)
	got = blankAll(true, fromClient)
	if bytes.Contains(got, i32(secret)) {
		t.Error("the cancellation key is in the stored bytes of a CancelRequest")
	}
	if len(got) != len(fromClient) {
		t.Errorf("%d bytes out of %d in", len(got), len(fromClient))
	}
}

// The MD5 salt is not the password, but the pair of them is what an offline
// attack needs, and there is nothing in the salt a reader would act on.
func TestTheMD5SaltGoesAndTheMechanismStays(t *testing.T) {
	stream := msg('R', u32(5), []byte{0xDE, 0xAD, 0xBE, 0xEF})
	got := blankAll(false, stream)

	if bytes.Contains(got, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
		t.Error("the salt is in the stored bytes")
	}
	msgs, _ := Deframe(got, false)
	if len(msgs) != 1 || msgs[0].Auth != "md5_password" {
		t.Fatalf("the mechanism did not survive: %+v", msgs)
	}
}

// sslmode=prefer is the default in psql and pgx: the client asks for TLS, and a
// server without it answers 'N' and the whole exchange carries on in the clear.
// Treating the refusal as the end of the readable stream would leave the
// password that follows it untouched.
func TestAnSSLRequestRefusedStillBlanksThePasswordThatFollows(t *testing.T) {
	fromClient := cat(
		append(u32(8), u32(codeSSLRequest)...),
		startup(196608, "user", "app"),
		msg('p', cstr(password)),
	)
	got := blankAll(true, fromClient)
	if bytes.Contains(got, []byte(password)) {
		t.Fatal("the password after a refused SSLRequest is in the stored bytes")
	}

	// The server's refusal is a bare byte, not a frame. Reading it as one would
	// swallow the four bytes after it and desync everything downstream.
	fromServer := cat([]byte{'N'}, msg('R', u32(3)), msg('K', i32(1), i32(0x51515151)))
	got = blankAll(false, fromServer)
	if bytes.Contains(got, i32(0x51515151)) {
		t.Error("the cancellation key after a refused SSLRequest is in the stored bytes")
	}
	if got[0] != 'N' {
		t.Errorf("the refusal byte itself was rewritten to %q", got[0])
	}
}

// Once the connection is encrypted there is nothing left to read, and guessing
// at frames inside TLS records would blank bytes at meaningless offsets.
func TestAnAgreedSSLRequestStopsTheBlanker(t *testing.T) {
	b := NewBlanker(false)
	b.Blank([]byte{'S'})
	if !b.Encrypted() {
		t.Fatal("the blanker kept parsing after the server agreed to encrypt")
	}

	record := []byte{0x16, 0x03, 0x03, 0x00, 0x2A, 'R', 0, 0, 0, 8, 0, 0, 0, 5}
	if got := b.Blank(record); !bytes.Equal(got, record) {
		t.Errorf("a TLS record was rewritten: % x", got)
	}
}

// Nearly every chunk carries no secret, and the ordinary path must not pay for
// the rare one — nor hand the caller a slice it might mutate later.
func TestAnUntouchedChunkIsReturnedAsItIs(t *testing.T) {
	b := NewBlanker(true)
	b.Blank(startup(196608, "user", "app"))

	query := msg('Q', cstr("SELECT 1"))
	if got := b.Blank(query); &got[0] != &query[0] {
		t.Error("a chunk with nothing to blank was copied")
	}

	// And a chunk that does carry one must not be rewritten in place: the same
	// bytes are on their way to the upstream.
	b = NewBlanker(true)
	b.Blank(startup(196608, "user", "app"))
	secret := msg('p', cstr(password))
	original := append([]byte(nil), secret...)
	if got := b.Blank(secret); bytes.Contains(got, []byte(password)) {
		t.Fatal("the password was not blanked at all, so this proves nothing")
	}
	if !bytes.Equal(secret, original) {
		t.Error("blanking rewrote the caller's buffer, which is what gets forwarded")
	}
}

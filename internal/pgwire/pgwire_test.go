package pgwire

import (
	"encoding/binary"
	"strings"
	"testing"
)

// The helpers below put bytes on the wire the way a driver does, so the tests
// prove the decoder against the format rather than against itself. Nothing here
// imports a Postgres driver: a test that borrows the encoder from the library
// it is checking proves only that the two agree.

// msg frames a typed message: the type byte, a big-endian length that counts
// itself but not the type byte, and the body.
func msg(typ byte, body ...[]byte) []byte {
	payload := cat(body...)
	out := []byte{typ}
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)+4))
	return append(out, payload...)
}

// startup frames the untyped first message: a length, a protocol code, and the
// key/value pairs, ended by a bare zero byte.
func startup(code uint32, pairs ...string) []byte {
	body := u32(code)
	for _, p := range pairs {
		body = append(body, cstr(p)...)
	}
	body = append(body, 0)
	return append(u32(uint32(len(body)+4)), body...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func cstr(s string) []byte { return append([]byte(s), 0) }
func u16(v int) []byte     { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
func u32(v uint32) []byte  { return binary.BigEndian.AppendUint32(nil, v) }
func i32(v int32) []byte   { return u32(uint32(v)) }
func field(c byte, s string) []byte {
	return append([]byte{c}, cstr(s)...)
}

func TestASimpleQueryIsReadBackAsItsSQL(t *testing.T) {
	msgs, rest := Deframe(msg('Q', cstr("SELECT id FROM orders WHERE total > 100")), true)

	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Kind != "query" || m.Type != "Q" {
		t.Errorf("kind=%q type=%q", m.Kind, m.Type)
	}
	if m.SQL != "SELECT id FROM orders WHERE total > 100" {
		t.Errorf("sql = %q", m.SQL)
	}
	if !m.FromClient {
		t.Error("a Query only ever comes from the client")
	}
}

// Every ORM and every pgx-based service uses the extended protocol, so this is
// the shape most captures will actually contain — the simple Query above is the
// rarer one.
func TestAParseBindExecuteSequenceIsReadInOrder(t *testing.T) {
	stream := cat(
		msg('P', cstr("s1"), cstr("SELECT name FROM users WHERE id = $1"), u16(1), u32(23)),
		msg('B',
			cstr(""), cstr("s1"),
			u16(0), // no format codes: every parameter is text
			u16(1), i32(3), []byte("417"),
			u16(0),
		),
		msg('E', cstr(""), i32(0)),
		msg('S'),
	)

	msgs, rest := Deframe(stream, true)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	want := []string{"parse", "bind", "execute", "sync"}
	if len(msgs) != len(want) {
		t.Fatalf("%d messages, want %d", len(msgs), len(want))
	}
	for i, kind := range want {
		if msgs[i].Kind != kind {
			t.Errorf("message %d is %q, want %q", i, msgs[i].Kind, kind)
		}
	}

	parse := msgs[0]
	if parse.Statement != "s1" || parse.SQL != "SELECT name FROM users WHERE id = $1" {
		t.Errorf("parse: statement=%q sql=%q", parse.Statement, parse.SQL)
	}
	if len(parse.ParamTypes) != 1 || parse.ParamTypes[0] != 23 {
		t.Errorf("parse: param types = %v, want the int4 OID", parse.ParamTypes)
	}

	bind := msgs[1]
	if bind.Statement != "s1" {
		t.Errorf("bind: statement = %q", bind.Statement)
	}
	if len(bind.Params) != 1 || bind.Params[0].Text != "417" {
		t.Fatalf("bind: params = %+v, want the value the query was run with", bind.Params)
	}
	if bind.Params[0].Binary {
		t.Error("bind: a parameter sent with no format codes is text")
	}
}

// The column names live in the RowDescription and nowhere else: a DataRow is a
// bare list of lengths and bytes. Reading rows without it gives values with no
// idea what they are.
func TestARowDescriptionNamesTheColumnsItsDataRowsCarry(t *testing.T) {
	col := func(name string, oid uint32, format int) []byte {
		return cat(cstr(name), u32(0), u16(0), u32(oid), u16(-1), u32(0xFFFFFFFF), u16(format))
	}
	stream := cat(
		msg('T', u16(2), col("id", 23, 0), col("email", 25, 0)),
		msg('D', u16(2), i32(3), []byte("417"), i32(14), []byte("ana@example.cl")),
		msg('C', cstr("SELECT 1")),
		msg('Z', []byte{'I'}),
	)

	msgs, rest := Deframe(stream, false)
	if rest != 0 {
		t.Fatalf("%d bytes left over", rest)
	}
	if len(msgs) != 4 {
		t.Fatalf("%d messages, want 4", len(msgs))
	}

	cols := msgs[0].Columns
	if len(cols) != 2 || cols[0].Name != "id" || cols[1].Name != "email" {
		t.Fatalf("columns = %+v", cols)
	}
	if cols[1].TypeOID != 25 {
		t.Errorf("email type OID = %d, want the text OID", cols[1].TypeOID)
	}

	values := msgs[1].Values
	if len(values) != 2 || values[0].Text != "417" || values[1].Text != "ana@example.cl" {
		t.Errorf("values = %+v", values)
	}
	if msgs[2].Tag != "SELECT 1" {
		t.Errorf("tag = %q, want the row count the server reported", msgs[2].Tag)
	}
	if msgs[3].TxStatus != "idle" {
		t.Errorf("tx status = %q", msgs[3].TxStatus)
	}
}

// A DataRow states its lengths but never its formats, so a binary result set
// only reads correctly if the formats carry forward from the RowDescription.
// pgx asks for binary results by default, which makes this the common case, and
// an int4 of 1633771873 is the four bytes "aaaa" — perfectly valid UTF-8 and
// completely the wrong answer.
func TestBinaryResultsAreNotReportedAsText(t *testing.T) {
	desc := cat(cstr("n"), u32(0), u16(0), u32(23), u16(4), u32(0xFFFFFFFF), u16(1))
	stream := cat(
		msg('T', u16(1), desc),
		msg('D', u16(1), i32(4), []byte("aaaa")),
	)

	msgs, _ := Deframe(stream, false)
	if len(msgs) != 2 {
		t.Fatalf("%d messages", len(msgs))
	}
	v := msgs[1].Values[0]
	if !v.Binary {
		t.Error("the value was not marked binary, so the RowDescription formats did not carry forward")
	}
	if v.Text != "" {
		t.Errorf("a binary int4 was reported as the text %q", v.Text)
	}
	if v.Size != 4 {
		t.Errorf("size = %d, want 4", v.Size)
	}
}

// This is the message people open a capture to find. Losing any of the three
// fields makes the capture useless for the one job it had.
func TestAnErrorResponseCarriesSeverityCodeAndMessage(t *testing.T) {
	body := cat(
		field('S', "ERROR"),
		field('V', "ERROR"),
		field('C', "42P01"),
		field('M', `relation "orders" does not exist`),
		field('P', "15"),
		field('F', "parse_relation.c"),
		[]byte{0},
	)

	msgs, rest := Deframe(msg('E', body), false)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	m := msgs[0]
	if m.Kind != "error_response" {
		t.Fatalf("kind = %q; 'E' from the server is an error, from the client it is Execute", m.Kind)
	}
	if m.Severity != "ERROR" || m.Code != "42P01" {
		t.Errorf("severity=%q code=%q", m.Severity, m.Code)
	}
	if m.Message != `relation "orders" does not exist` {
		t.Errorf("message = %q", m.Message)
	}
	// The unpromoted fields are kept rather than dropped: position is how an
	// editor highlights the offending token.
	if m.Fields["position"] != "15" || m.Fields["file"] != "parse_relation.c" {
		t.Errorf("extra fields = %v", m.Fields)
	}
	if got := Summarise(msgs); !strings.Contains(got, "42P01") || !strings.Contains(got, "does not exist") {
		t.Errorf("summary = %q, want the error to lead", got)
	}
}

// The same byte means different messages in each direction, and getting it
// backwards would label a client's Describe as a row of data.
func TestTheSameTypeByteMeansDifferentThingsByDirection(t *testing.T) {
	describe := msg('D', []byte{'S'}, cstr("s1"))
	if got, _ := Deframe(describe, true); got[0].Kind != "describe" || got[0].Statement != "s1" {
		t.Errorf("from the client, 'D' read as %q %+v", got[0].Kind, got[0])
	}
	if got, _ := Deframe(msg('D', u16(0)), false); got[0].Kind != "data_row" {
		t.Errorf("from the server, 'D' read as %q", got[0].Kind)
	}
	if got, _ := Deframe(msg('S'), true); got[0].Kind != "sync" {
		t.Errorf("from the client, 'S' read as %q", got[0].Kind)
	}
	if got, _ := Deframe(msg('S', cstr("client_encoding"), cstr("UTF8")), false); got[0].Kind != "parameter_status" {
		t.Errorf("from the server, 'S' read as %q", got[0].Kind)
	}
}

// The startup message is the only one with no type byte. Reading its first
// length byte as a type would mis-frame the whole connection from byte zero.
func TestTheStartupMessageHasNoTypeByte(t *testing.T) {
	stream := cat(
		startup(196608, "user", "sonda", "database", "orders", "application_name", "psql"),
		msg('Q', cstr("SELECT 1")),
	)

	msgs, rest := Deframe(stream, true)
	if rest != 0 {
		t.Errorf("%d bytes left over", rest)
	}
	if len(msgs) != 2 {
		t.Fatalf("%d messages, want the startup and the query after it", len(msgs))
	}
	m := msgs[0]
	if m.Kind != "startup" || m.Type != "" {
		t.Errorf("kind=%q type=%q, want a startup with no type byte", m.Kind, m.Type)
	}
	if m.Protocol != "3.0" {
		t.Errorf("protocol = %q", m.Protocol)
	}
	if m.Parameters["user"] != "sonda" || m.Parameters["database"] != "orders" {
		t.Errorf("parameters = %v", m.Parameters)
	}
	if msgs[1].SQL != "SELECT 1" {
		t.Errorf("the message after the startup read as %+v", msgs[1])
	}
}

// A server stream never contains a startup message, and a client stream that
// was joined mid-conversation does not either. Guessing one where there is none
// would swallow a real message whole.
func TestAStartupIsOnlyExpectedFromAClientAtTheStart(t *testing.T) {
	stream := cat(msg('Q', cstr("SELECT 1")), msg('X'))
	msgs, rest := Deframe(stream, true)
	if rest != 0 || len(msgs) != 2 {
		t.Fatalf("%d messages, %d left over", len(msgs), rest)
	}
	if msgs[0].Kind != "query" || msgs[1].Kind != "terminate" {
		t.Errorf("kinds = %q, %q", msgs[0].Kind, msgs[1].Kind)
	}
}

// An SSLRequest means everything after it is TLS. Decoding ciphertext as
// protocol messages would produce a confident fiction, which the product rules
// out: say the conversation cannot be read instead.
func TestAnSSLRequestIsReportedRatherThanDecodedAsGarbage(t *testing.T) {
	ssl := append(u32(8), u32(80877103)...)
	// What follows on the wire is TLS, which is indistinguishable from random
	// bytes — and random bytes occasionally spell a valid message. These nine
	// are byte for byte a Query for "abc". Without the guard Sonda would report
	// a statement nobody ran, which is worse than reporting nothing.
	ciphertext := msg('Q', cstr("abc"))

	msgs, rest := Deframe(cat(ssl, ciphertext), true)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want only the SSLRequest", len(msgs))
	}
	m := msgs[0]
	if m.Kind != "ssl_request" || !m.Encrypted {
		t.Errorf("kind=%q encrypted=%v", m.Kind, m.Encrypted)
	}
	if m.Note == "" {
		t.Error("nothing tells the reader why the rest of the capture is missing")
	}
	if rest != len(ciphertext) {
		t.Errorf("remainder = %d, want the %d unreadable bytes", rest, len(ciphertext))
	}
	if got := Summarise(msgs); !strings.Contains(got, "encrypted") {
		t.Errorf("summary = %q, want it to say the connection is encrypted", got)
	}
}

// A capture is a prefix of a conversation that may still be running, so a
// half-message at the end is normal and must not lose the whole ones before it.
func TestATruncatedTailKeepsWhatCameBefore(t *testing.T) {
	whole := msg('Q', cstr("SELECT 1"))
	partial := msg('Q', cstr("SELECT 2"))

	for cut := 1; cut < len(partial); cut++ {
		msgs, rest := Deframe(cat(whole, partial[:cut]), true)
		if len(msgs) != 1 {
			t.Fatalf("cut at %d: %d messages, want the one complete message", cut, len(msgs))
		}
		if msgs[0].SQL != "SELECT 1" {
			t.Errorf("cut at %d: got %q", cut, msgs[0].SQL)
		}
		if rest != cut {
			t.Errorf("cut at %d: remainder = %d, want %d", cut, rest, cut)
		}
	}
}

// A body cut inside its own fields is worse than a cut between messages,
// because a naive parser reads past the end of it. What can be read is kept and
// the gap is stated.
func TestABodyCutInsideItsFieldsIsReportedAsPartial(t *testing.T) {
	// A RowDescription that promises three columns and delivers one.
	body := cat(u16(3), cstr("id"), u32(0), u16(0), u32(23), u16(4), u32(0xFFFFFFFF), u16(0))

	msgs, rest := Deframe(msg('T', body), false)
	if rest != 0 || len(msgs) != 1 {
		t.Fatalf("%d messages, %d left over", len(msgs), rest)
	}
	if len(msgs[0].Columns) != 1 || msgs[0].Columns[0].Name != "id" {
		t.Errorf("columns = %+v, want the one that was really there", msgs[0].Columns)
	}
	if msgs[0].Note == "" {
		t.Error("a partial reading was presented as a complete one")
	}
}

// A length that cannot be real is a corrupt stream, a mis-framed one, or not
// this protocol at all. Reading on would mean trusting it to size an
// allocation.
func TestAnAbsurdLengthIsRefusedRatherThanAllocated(t *testing.T) {
	for _, length := range []uint32{0xFFFFFFFF, 1 << 31, (1 << 30) + 1, 3} {
		header := append([]byte{'Q'}, u32(length)...)
		msgs, rest := Deframe(header, true)
		if len(msgs) != 0 {
			t.Errorf("length %d: %d messages from a corrupt header", length, len(msgs))
		}
		if rest != len(header) {
			t.Errorf("length %d: remainder = %d, want the whole buffer back", length, rest)
		}
	}

	// The same applies to a count inside a body: a Bind claiming 60000
	// parameters in a nine-byte message must not size anything from that.
	msgs, _ := Deframe(msg('B', cstr(""), cstr(""), u16(0), u16(60000)), true)
	if len(msgs[0].Params) != 0 {
		t.Errorf("%d parameters materialised out of a body that held none", len(msgs[0].Params))
	}
}

// Bytes that are not UTF-8 are routine — a bytea column, a LATIN1 database, a
// value that is really a serialised blob. Claiming they are text is the kind of
// confident nonsense the product principles rule out.
func TestANonUTF8ValueIsNotClaimedAsText(t *testing.T) {
	raw := []byte{0x48, 0xC3, 0x28, 0xFF}
	msgs, _ := Deframe(msg('B', cstr(""), cstr(""), u16(0), u16(1), i32(int32(len(raw))), raw, u16(0)), true)

	if len(msgs[0].Params) != 1 {
		t.Fatalf("params = %+v", msgs[0].Params)
	}
	v := msgs[0].Params[0]
	if v.Text != "" {
		t.Errorf("invalid UTF-8 was reported as the text %q", v.Text)
	}
	if v.Size != len(raw) || v.Null {
		t.Errorf("the value itself was lost: %+v", v)
	}
}

// NULL and the empty string are different on the wire and different in a WHERE
// clause. Collapsing them hides the bug someone is looking for.
func TestANullValueIsNotAnEmptyString(t *testing.T) {
	body := cat(u16(2), i32(-1), i32(0))

	msgs, _ := Deframe(msg('D', body), false)
	values := msgs[0].Values
	if len(values) != 2 {
		t.Fatalf("values = %+v", values)
	}
	if !values[0].Null {
		t.Error("a NULL column was not reported as NULL")
	}
	if values[1].Null {
		t.Error("an empty string was reported as NULL")
	}
}

// The product principle is to degrade to a lesser view, never to a blank one.
// An unrecognised type byte still has a name and a size.
func TestAnUnknownMessageIsReportedByTypeAndSize(t *testing.T) {
	msgs, rest := Deframe(msg('W', []byte("......")), true)
	if rest != 0 || len(msgs) != 1 {
		t.Fatalf("%d messages, %d left over", len(msgs), rest)
	}
	m := msgs[0]
	if m.Type != "W" {
		t.Errorf("type = %q", m.Type)
	}
	if !strings.Contains(m.Kind, "unknown") || !strings.Contains(m.Kind, "W") {
		t.Errorf("kind = %q, want something naming the byte", m.Kind)
	}
	if m.Size != 6 {
		t.Errorf("size = %d, want 6", m.Size)
	}
}

// A password and a cancellation key are credentials that would otherwise be
// rendered in a browser. They are named and measured, never decoded.
func TestCredentialsAreNamedButNotDecoded(t *testing.T) {
	msgs, _ := Deframe(msg('p', cstr("md5abc123")), true)
	if msgs[0].Kind != "authentication_response" || msgs[0].Note == "" {
		t.Errorf("password message read as %+v", msgs[0])
	}
	if strings.Contains(msgs[0].Note, "md5abc123") {
		t.Error("the note leaked the credential it exists to withhold")
	}

	msgs, _ = Deframe(msg('K', u32(4242), u32(0xDEADBEEF)), false)
	if msgs[0].Kind != "backend_key_data" || msgs[0].Note == "" {
		t.Errorf("backend key data read as %+v", msgs[0])
	}
}

func TestSummariseLeadsWithTheStatement(t *testing.T) {
	stream := cat(
		msg('Q', cstr("SELECT id,\n       email\n  FROM users")),
	)
	msgs, _ := Deframe(stream, true)
	if got := Summarise(msgs); got != "SELECT id, email FROM users" {
		t.Errorf("summary = %q, want the statement on one line", got)
	}

	server, _ := Deframe(cat(msg('2'), msg('C', cstr("UPDATE 3")), msg('Z', []byte{'I'})), false)
	if got := Summarise(server); !strings.Contains(got, "bind_complete") || !strings.Contains(got, "command_complete") {
		t.Errorf("summary = %q, want counts when there is no statement to show", got)
	}

	if Summarise(nil) != "no messages" {
		t.Errorf("empty summary = %q", Summarise(nil))
	}
}

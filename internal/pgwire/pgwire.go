// Package pgwire reads the PostgreSQL v3 frontend/backend protocol.
//
// It stands next to grpcwire and wsframe and exists for the same reason: what
// crossed the wire is a stream of framed messages, Sonda stores the stream
// exactly as it arrived, and turning it back into messages is a view computed
// when someone looks. Nothing here re-serializes, so nothing here can lose the
// parts the parser did not understand.
//
// The format is the PostgreSQL "Frontend/Backend Protocol", version 3.0. After
// the handshake every message has the same shape:
//
//	+--------+-----------------------+-------------------------+
//	|  type  |     length (int32)    |          body           |
//	| 1 byte | counts itself, not    |    length - 4 bytes     |
//	|        | the type byte         |                         |
//	+--------+-----------------------+-------------------------+
//
// Two things make this harder than it looks. The first message a client sends
// has no type byte at all — it is a bare length and a protocol code — and the
// same type byte means different messages in each direction ('D' is Describe
// from the client and DataRow from the server, 'C' is Close and
// CommandComplete, 'S' is Sync and ParameterStatus). Deframe therefore needs to
// be told which side of the conversation it is reading.
package pgwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// maxMessage refuses a length no real conversation produces. PostgreSQL's
	// own ceiling on a message body is 1 GB, so anything above that means the
	// stream is corrupt, mis-framed, or not this protocol at all.
	//
	// The bounds check below would catch most of those anyway. What it would
	// not catch is the length near 2^32 that a corrupt stream produces: int is
	// 32 bits on a 32-bit build, the conversion wraps negative, and the "is the
	// message all here" comparison then passes on a negative end and the slice
	// panics. Capping first means the conversion is always in range.
	maxMessage = 1 << 30

	// These codes sit in the protocol-version slot of a startup message, which
	// is how a client asks for something other than an ordinary session.
	codeCancelRequest = 80877102
	codeSSLRequest    = 80877103
	codeGSSENCRequest = 80877104
)

// Message is one protocol message as it crossed.
//
// The fields are flat and almost all optional rather than one type per message
// kind. This value is serialised straight into the HTTP API, and a single shape
// with absent fields is easier to consume than a union every client has to
// switch on. Only what a reader would act on is decoded; the rest of the body
// stays in Payload.
type Message struct {
	// Type is the type byte as it crossed, kept as a one-character string so
	// the JSON view reads "Q" rather than 81. A startup message has none.
	Type string `json:"type,omitempty"`

	// Kind is the human name for Type in this direction. An unrecognised type
	// byte is still named, by its byte, because reporting "unknown('W'), 14
	// bytes" is a lesser view and dropping the message is a blank one.
	Kind string `json:"kind"`

	// Size is the body, excluding the type byte and the length field. It is the
	// one thing that can always be reported, whatever the body turns out to be.
	Size       int64 `json:"size"`
	FromClient bool  `json:"from_client"`

	// Payload aliases the captured bytes rather than copying them: decoding
	// only ever reads, so there is nothing to protect the caller from.
	Payload []byte `json:"-"`

	// SQL is the statement text of a Query or a Parse — the reason most people
	// open a Postgres capture at all.
	SQL        string   `json:"sql,omitempty"`
	Statement  string   `json:"statement,omitempty"`
	Portal     string   `json:"portal,omitempty"`
	ParamTypes []uint32 `json:"param_types,omitempty"`
	Params     []Value  `json:"params,omitempty"`
	MaxRows    int32    `json:"max_rows,omitempty"`

	Columns []Column `json:"columns,omitempty"`
	Values  []Value  `json:"values,omitempty"`

	// Tag is the CommandComplete tag, e.g. "SELECT 12" or "UPDATE 1", which is
	// the only place the row count of a write is stated.
	Tag string `json:"tag,omitempty"`

	// The five error fields worth naming. ErrorResponse and NoticeResponse
	// share a shape, so both land here.
	Severity string `json:"severity,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	// Fields holds the remaining error fields — position, table, constraint,
	// source file — under readable names. They are rarely the answer but they
	// are occasionally the whole answer.
	Fields map[string]string `json:"fields,omitempty"`

	TxStatus string `json:"tx_status,omitempty"`
	Auth     string `json:"auth,omitempty"`

	// Protocol and Parameters come from the startup message; Parameters also
	// carries the single pair of a ParameterStatus, which is the same kind of
	// fact arriving later.
	Protocol   string            `json:"protocol,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`

	// Encrypted marks an SSLRequest or GSSENCRequest. If the server agreed,
	// everything after it is TLS and cannot be read at all.
	Encrypted bool `json:"encrypted,omitempty"`

	// Note explains what was deliberately not decoded, or what could not be.
	// A gap the reader knows about is survivable; a silent one is not.
	Note string `json:"note,omitempty"`
}

// Value is one bind parameter or one column of a row.
type Value struct {
	// Null distinguishes a SQL NULL (length -1) from an empty string
	// (length 0). They are different on the wire and different in a WHERE
	// clause, and collapsing them is how an afternoon disappears.
	Null bool `json:"null,omitempty"`
	Size int  `json:"size"`

	// Binary reports the format code the protocol gave this value. A binary
	// value is not claimed as text even when its bytes happen to be valid
	// UTF-8 — a four-byte int4 of 1633771873 is exactly "aaaa".
	Binary bool   `json:"binary,omitempty"`
	Text   string `json:"text,omitempty"`
	Bytes  []byte `json:"-"`
}

// Column is one entry of a RowDescription.
//
// The table OID, column number, type length and type modifier are read past but
// not reported: without a catalogue lookup they are numbers nobody can act on,
// and the name plus the type OID are what identify a column to a reader.
type Column struct {
	Name    string `json:"name"`
	TypeOID uint32 `json:"type_oid"`
	Binary  bool   `json:"binary,omitempty"`
}

var frontendKinds = map[byte]string{
	'Q': "query",
	'P': "parse",
	'B': "bind",
	'E': "execute",
	'S': "sync",
	'X': "terminate",
	'D': "describe",
	'C': "close",
	'H': "flush",
	'p': "authentication_response",
	'F': "function_call",
	'd': "copy_data",
	'c': "copy_done",
	'f': "copy_fail",
}

var backendKinds = map[byte]string{
	'R': "authentication",
	'K': "backend_key_data",
	'S': "parameter_status",
	'Z': "ready_for_query",
	'T': "row_description",
	'D': "data_row",
	'C': "command_complete",
	'E': "error_response",
	'N': "notice_response",
	'I': "empty_query",
	'n': "no_data",
	's': "portal_suspended",
	't': "parameter_description",
	'A': "notification",
	'v': "negotiate_protocol_version",
	'1': "parse_complete",
	'2': "bind_complete",
	'3': "close_complete",
	'G': "copy_in_response",
	'H': "copy_out_response",
	'W': "copy_both_response",
	'd': "copy_data",
	'c': "copy_done",
}

func kindOf(typ byte, fromClient bool) string {
	table := backendKinds
	if fromClient {
		table = frontendKinds
	}
	if kind, ok := table[typ]; ok {
		return kind
	}
	// A type byte outside ASCII is either a protocol extension or a stream that
	// is not Postgres. Printing it as a character would be a lie in the second
	// case, so it is shown as a byte.
	if typ >= 0x20 && typ < 0x7f {
		return fmt.Sprintf("unknown(%q)", string(rune(typ)))
	}
	return fmt.Sprintf("unknown(0x%02X)", typ)
}

// Deframe reads as many whole messages as the buffer holds and reports how many
// bytes were left over.
//
// A remainder is not an error. A capture is a prefix of a conversation that may
// still be running, and cutting one mid-message is the normal case — the same
// contract grpcwire.Deframe and wsframe.Deframe have, for the same reason. It
// is also where a refused length lands: an impossible length and an unfinished
// message are indistinguishable from inside, and both mean "read no further".
func Deframe(stream []byte, fromClient bool) (msgs []Message, remainder int) {
	rest := stream
	first := true

	// Result format codes carry forward from a RowDescription to the DataRows
	// that follow it: a DataRow states its lengths but never its formats. Every
	// pgx-based client asks for binary results, so without this the values
	// would be reported as text that happens to decode.
	var formats []bool

	for len(rest) > 0 {
		if first && fromClient && looksLikeStartup(rest) {
			length := int(binary.BigEndian.Uint32(rest))
			if len(rest) < length {
				return msgs, len(rest)
			}
			m := decodeStartup(rest[4:length])
			msgs = append(msgs, m)
			rest = rest[length:]
			first = false
			if m.Encrypted {
				// If the server agreed, every byte after this is a TLS record.
				// Decoding them as protocol messages would produce confident
				// nonsense; reporting them as bytes nobody can read is the
				// honest answer, and the message says why.
				return msgs, len(rest)
			}
			continue
		}
		first = false

		if len(rest) < 5 {
			return msgs, len(rest)
		}
		// The length counts itself but not the type byte, so a body of n bytes
		// arrives as n+4. Below 4 the message claims to be shorter than its own
		// header, which no sender produces.
		length := binary.BigEndian.Uint32(rest[1:5])
		if length < 4 || length > maxMessage {
			return msgs, len(rest)
		}
		end := 1 + int(length)
		if len(rest) < end {
			return msgs, len(rest)
		}

		m := decode(rest[0], rest[5:end], fromClient, formats)
		if m.Kind == "row_description" {
			formats = formats[:0]
			for _, c := range m.Columns {
				formats = append(formats, c.Binary)
			}
		}
		msgs = append(msgs, m)
		rest = rest[end:]
	}

	return msgs, len(rest)
}

// looksLikeStartup decides whether the buffer opens with the one message that
// has no type byte.
//
// Guessing wrong in either direction is bad: treating a startup message as
// typed reads its first length byte as a type, and treating a typed message as
// a startup swallows it whole. The protocol code makes the call safe — a real
// startup message always carries a version or one of the request codes, and a
// type byte followed by a plausible length practically never does.
func looksLikeStartup(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	if length := binary.BigEndian.Uint32(b); length < 8 || length > maxMessage {
		return false
	}
	code := binary.BigEndian.Uint32(b[4:])
	switch code {
	case codeCancelRequest, codeSSLRequest, codeGSSENCRequest:
		return true
	}
	// Major 3 is the only version that has ever existed; 3.2 arrived with
	// PostgreSQL 18. A future major would be reported as an unknown message
	// rather than mis-framed into garbage.
	return code>>16 == 3
}

func decodeStartup(body []byte) Message {
	m := Message{Kind: "startup", Size: int64(len(body)), FromClient: true, Payload: body}
	cur := &cursor{b: body}
	code := cur.uint32()

	switch code {
	case codeSSLRequest, codeGSSENCRequest:
		m.Kind = "ssl_request"
		m.Encrypted = true
		m.Note = "the client asked to encrypt the connection; if the server agreed, the rest of this capture is TLS and cannot be decoded"
		if code == codeGSSENCRequest {
			m.Kind = "gssenc_request"
		}
		return m
	case codeCancelRequest:
		m.Kind = "cancel_request"
		// The process id and the secret key are a credential pair whose only
		// use is cancelling someone else's query. Nothing is served by putting
		// them in a view a browser renders.
		m.Note = "carries the cancellation key, which is a credential and is not decoded"
		return m
	}

	m.Protocol = fmt.Sprintf("%d.%d", code>>16, code&0xffff)
	m.Parameters = map[string]string{}
	for !cur.bad && len(cur.b) > 0 {
		key := cur.text()
		if key == "" {
			// The list is terminated by an empty key, i.e. a bare zero byte.
			break
		}
		value := cur.text()
		if cur.bad {
			break
		}
		m.Parameters[key] = value
	}
	noteIfPartial(&m, cur)
	return m
}

func decode(typ byte, body []byte, fromClient bool, formats []bool) Message {
	m := Message{
		Type:       string(rune(typ)),
		Kind:       kindOf(typ, fromClient),
		Size:       int64(len(body)),
		FromClient: fromClient,
		Payload:    body,
	}
	cur := &cursor{b: body}

	switch m.Kind {
	case "query":
		m.SQL = cur.text()

	case "parse":
		m.Statement = cur.text()
		m.SQL = cur.text()
		m.ParamTypes = readOIDs(cur)

	case "bind":
		m.Portal = cur.text()
		m.Statement = cur.text()
		paramFormats := readFormats(cur)
		for i, n := 0, int(cur.uint16()); i < n; i++ {
			v := readValue(cur, formatAt(paramFormats, i))
			// The count came from the sender. Appending what the cursor
			// invented after running out would report parameters nobody sent.
			if cur.bad {
				break
			}
			m.Params = append(m.Params, v)
		}

	case "execute":
		m.Portal = cur.text()
		m.MaxRows = cur.int32()

	case "describe", "close":
		// Both are a target byte then a name: 'S' for a prepared statement,
		// 'P' for a portal. An empty name is the unnamed one, which is what
		// every driver uses for a one-shot query.
		target := cur.uint8()
		name := cur.text()
		if target == 'S' {
			m.Statement = name
		} else {
			m.Portal = name
		}

	case "row_description":
		for n := cur.uint16(); n > 0; n-- {
			c := Column{Name: cur.text()}
			cur.uint32() // table OID
			cur.uint16() // column number within that table
			c.TypeOID = cur.uint32()
			cur.uint16() // type length
			cur.uint32() // type modifier
			c.Binary = cur.uint16() == 1
			if cur.bad {
				break
			}
			m.Columns = append(m.Columns, c)
		}

	case "data_row":
		for i, n := 0, int(cur.uint16()); i < n; i++ {
			v := readValue(cur, formatAt(formats, i))
			if cur.bad {
				break
			}
			m.Values = append(m.Values, v)
		}

	case "command_complete":
		m.Tag = cur.text()

	case "error_response", "notice_response":
		readDiagnostic(&m, cur)

	case "ready_for_query":
		switch s := cur.uint8(); s {
		case 'I':
			m.TxStatus = "idle"
		case 'T':
			m.TxStatus = "in_transaction"
		case 'E':
			m.TxStatus = "failed_transaction"
		default:
			m.TxStatus = fmt.Sprintf("unknown(0x%02X)", s)
		}

	case "authentication":
		m.Auth = authKind(cur.uint32())
		// The SASL challenge and response bodies are part of a password
		// exchange. The mechanism is worth showing; the exchange is not.

	case "authentication_response":
		m.Note = "not decoded: this message carries a password or a SASL exchange"

	case "backend_key_data":
		m.Note = "carries the cancellation key, which is a credential and is not decoded"

	case "parameter_status":
		// Read in two statements rather than one map literal: the order the two
		// strings come off the wire matters and should not depend on how a
		// reader remembers composite-literal evaluation order.
		name := cur.text()
		m.Parameters = map[string]string{name: cur.text()}

	case "parameter_description":
		m.ParamTypes = readOIDs(cur)
	}

	noteIfPartial(&m, cur)
	return m
}

// readFormats reads a count of format codes. The count is allowed to be 0,
// meaning every value is text, or 1, meaning one code applies to all of them.
func readFormats(cur *cursor) []bool {
	var out []bool
	for n := cur.uint16(); n > 0; n-- {
		v := cur.uint16()
		if cur.bad {
			break
		}
		out = append(out, v == 1)
	}
	return out
}

// readOIDs reads a counted list of type OIDs, as Parse and
// ParameterDescription both carry.
func readOIDs(cur *cursor) []uint32 {
	var out []uint32
	for n := cur.uint16(); n > 0; n-- {
		v := cur.uint32()
		if cur.bad {
			break
		}
		out = append(out, v)
	}
	return out
}

func formatAt(formats []bool, i int) bool {
	switch {
	case len(formats) == 0:
		return false
	case len(formats) == 1:
		return formats[0]
	case i < len(formats):
		return formats[i]
	}
	return false
}

func readValue(cur *cursor, binaryFormat bool) Value {
	length := cur.int32()
	if length < 0 {
		// -1 is NULL. It is the only negative length the protocol defines, and
		// it is not a zero-length value.
		return Value{Null: true, Size: 0}
	}
	raw := cur.take(int(length))
	v := Value{Size: len(raw), Binary: binaryFormat, Bytes: raw}
	// Text only when it really is text: a binary value is a wire encoding, and
	// invalid UTF-8 is not a string however much a reader would like one.
	if !binaryFormat && utf8.Valid(raw) {
		v.Text = string(raw)
	}
	return v
}

// errorFieldNames covers the diagnostic fields that are not promoted to their
// own struct field. Anything outside the table is kept under its raw letter
// rather than dropped, because an unknown field is still evidence.
var errorFieldNames = map[byte]string{
	'V': "severity_unlocalized",
	'P': "position",
	'p': "internal_position",
	'q': "internal_query",
	'W': "where",
	's': "schema",
	't': "table",
	'c': "column",
	'd': "data_type",
	'n': "constraint",
	'F': "file",
	'L': "line",
	'R': "routine",
}

func readDiagnostic(m *Message, cur *cursor) {
	for !cur.bad {
		field := cur.uint8()
		if field == 0 {
			// A zero byte where a field code would be ends the list.
			break
		}
		value := cur.text()
		switch field {
		case 'S':
			m.Severity = value
		case 'C':
			m.Code = value
		case 'M':
			m.Message = value
		case 'D':
			m.Detail = value
		case 'H':
			m.Hint = value
		default:
			name, ok := errorFieldNames[field]
			if !ok {
				name = fmt.Sprintf("field(%q)", string(rune(field)))
			}
			if m.Fields == nil {
				m.Fields = map[string]string{}
			}
			m.Fields[name] = value
		}
	}
}

func authKind(sub uint32) string {
	switch sub {
	case 0:
		return "ok"
	case 2:
		return "kerberos_v5"
	case 3:
		return "cleartext_password"
	case 5:
		return "md5_password"
	case 6:
		return "scm_credential"
	case 7:
		return "gss"
	case 8:
		return "gss_continue"
	case 9:
		return "sspi"
	case 10:
		return "sasl"
	case 11:
		return "sasl_continue"
	case 12:
		return "sasl_final"
	default:
		return fmt.Sprintf("unknown(%d)", sub)
	}
}

// noteIfPartial records that the reading is incomplete, so nobody mistakes a
// half-decoded message for the whole of what was sent.
func noteIfPartial(m *Message, cur *cursor) {
	if m.Note != "" {
		return
	}
	switch {
	case cur.bad:
		m.Note = "the message body ended sooner than its own fields claimed; this reading is partial"
	case cur.nonUTF8:
		m.Note = "one or more text fields were not valid UTF-8 and are shown empty; the bytes are in the capture"
	}
}

// cursor walks a message body.
//
// Every read is bounds checked and a read past the end sets bad and yields a
// zero, which keeps the length fields — all of them chosen by the sender —
// from turning a malformed capture into a panic or a huge allocation. Nothing
// here is pre-sized from a declared count for the same reason: the slices grow
// from the bytes that are actually present.
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

func (c *cursor) int32() int32 { return int32(c.uint32()) }

// text reads a null-terminated string. A missing terminator is a truncated
// body, not an empty string.
func (c *cursor) text() string {
	end := bytes.IndexByte(c.b, 0)
	if end < 0 {
		c.bad = true
		c.b = nil
		return ""
	}
	s := c.b[:end]
	c.b = c.b[end+1:]
	if !utf8.Valid(s) {
		// Postgres strings are in the server encoding, which is nearly always
		// UTF-8 but is not required to be. Rendering LATIN1 bytes as a Go
		// string would put replacement characters in a view people copy SQL
		// out of, so the field is left empty and the message says so.
		c.nonUTF8 = true
		return ""
	}
	return string(s)
}

// Summarise gives a one-line reading of a conversation, for a listing that has
// no room for the messages themselves.
//
// It is not a count by kind the way wsframe's is. A Postgres exchange is mostly
// DataRows, and "1 query, 1 row_description, 40 data_row" tells a reader
// nothing they came for: they came for the statement, or for the error. Counts
// remain the fallback for an exchange that has neither.
func Summarise(msgs []Message) string {
	if len(msgs) == 0 {
		return "no messages"
	}

	var sql, tag string
	rows := 0
	for _, m := range msgs {
		switch {
		case m.Kind == "error_response":
			// The error is why the capture was opened. Nothing outranks it.
			return errorLine(m)
		case m.Encrypted:
			return "encrypted connection, not readable"
		case m.SQL != "" && sql == "":
			sql = m.SQL
		case m.Kind == "data_row":
			rows++
		case m.Kind == "command_complete" && tag == "":
			tag = m.Tag
		}
	}

	if sql != "" {
		line := oneLine(sql)
		switch {
		case tag != "":
			return line + " -> " + tag
		case rows > 0:
			return fmt.Sprintf("%s -> %d rows", line, rows)
		}
		return line
	}

	counts := map[string]int{}
	order := []string{}
	for _, m := range msgs {
		if counts[m.Kind] == 0 {
			order = append(order, m.Kind)
		}
		counts[m.Kind]++
	}
	parts := make([]string, 0, len(order))
	for _, kind := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
	}
	return strings.Join(parts, ", ")
}

// errorLine states an error with whatever of it arrived. A server that sent no
// SQLSTATE, or a body cut short before the message, still gets a line saying an
// error happened, which is more than "5 messages" says.
func errorLine(m Message) string {
	parts := []string{"error"}
	if m.Code != "" {
		parts = append(parts, m.Code)
	}
	if m.Message != "" {
		return strings.Join(parts, " ") + ": " + oneLine(m.Message)
	}
	return strings.Join(parts, " ")
}

// oneLine flattens SQL onto a single line. Formatted statements arrive with
// newlines and long runs of indentation, and a listing has one row per capture.
func oneLine(sql string) string {
	flat := strings.Join(strings.Fields(sql), " ")
	const limit = 90
	if len(flat) > limit {
		return flat[:limit] + "..."
	}
	return flat
}

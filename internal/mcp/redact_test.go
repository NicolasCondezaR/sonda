package mcp

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	descpb "google.golang.org/protobuf/types/descriptorpb"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/pgwire"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Sonda stores real bearer tokens and session cookies. Everything that leaves
// through MCP lands in whatever model the agent is driving, so these are the
// tests that matter most in this package: a regression here does not produce a
// wrong answer, it produces a leaked production credential.

func cleaned(t *testing.T, payload string, detail bool) string {
	t.Helper()
	return cleanedFrom(t, "/api/calls", payload, detail)
}

// cleanedFrom redacts a payload as the answer of one endpoint. Which fields are
// Sonda's own is a property of the endpoint, so a test that means "one capture
// in full" has to say so — the same way the tool that asks for it does.
func cleanedFrom(t *testing.T, endpoint, payload string, detail bool) string {
	t.Helper()
	v, err := cleanAnswer(endpoint, []byte(payload), detail)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// cleanedCall is one capture read in full, which is where the decoded views
// live.
func cleanedCall(t *testing.T, payload string, detail bool) string {
	t.Helper()
	return cleanedFrom(t, "/api/calls/88", payload, detail)
}

func TestAMQPDecodedFramesStayVisibleAndReplaceTheRawCopy(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("framed AMQP bytes with a secret-shaped payload"))
	out := cleanedCall(t, `{
	  "protocol":"amqp",
	  "request":{"base64":"`+raw+`","size":47,"stored":47},
	  "response":{"size":0,"stored":0},
	  "amqp":{"sent":[{
	    "type":"method","kind":"basic.publish","channel":1,
	    "exchange":"orders","routing_key":"order.created"
	  }],"received":[]}
	}`, true)

	for _, want := range []string{"basic.publish", "orders", "order.created"} {
		if !strings.Contains(out, want) {
			t.Errorf("the decoded AMQP view lost %q: %s", want, out)
		}
	}
	var call struct {
		Request rawMessage `json:"request"`
	}
	if err := json.Unmarshal([]byte(out), &call); err != nil {
		t.Fatal(err)
	}
	if call.Request.Base64 != redacted {
		t.Errorf("the raw AMQP stream remained beside its decoded view: %q", call.Request.Base64)
	}
}

func TestCredentialsNeverLeave(t *testing.T) {
	// Headers arrive from Go's http.Header as name to list of values.
	payload := `{
	  "request": {
	    "headers": {
	      "Authorization": ["Bearer eyJhbGciOiJIUzI1NiJ9.SECRET"],
	      "Cookie": ["session=abc123; theme=dark"],
	      "X-Api-Key": ["k-live-9f8e7d"],
	      "Content-Type": ["application/json"]
	    }
	  },
	  "response": {
	    "headers": { "Set-Cookie": ["session=renewed; HttpOnly"] }
	  }
	}`

	got := cleaned(t, payload, false)

	for _, secret := range []string{"eyJhbGciOiJIUzI1NiJ9", "SECRET", "abc123", "k-live-9f8e7d", "renewed"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived redaction:\n%s", secret, got)
		}
	}
	// And the harmless header is untouched, or the tool is useless.
	if !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type was redacted too:\n%s", got)
	}
}

// The same field turns up spelled four ways depending on whose service wrote
// it, and a leak is a leak regardless of the casing.
func TestRedactionCatchesEverySpelling(t *testing.T) {
	payload := `{
	  "accessToken": "one",
	  "access_token": "two",
	  "ACCESS-TOKEN": "three",
	  "refreshToken": "four",
	  "clientSecret": "five",
	  "x-company-auth-token": "six",
	  "password": "seven",
	  "credentials": "eight"
	}`

	got := cleaned(t, payload, false)
	for _, secret := range []string{`"one"`, `"two"`, `"three"`, `"four"`, `"five"`, `"six"`, `"seven"`, `"eight"`} {
		if strings.Contains(got, secret) {
			t.Errorf("%s survived redaction:\n%s", secret, got)
		}
	}
}

// A credential three levels down inside a decoded protobuf body is still a
// credential.
func TestRedactionReachesNestedBodies(t *testing.T) {
	payload := `{"response":{"json":{"user":{"profile":{"apiKey":"deep-secret","name":"Nicolas"}}}}}`

	got := cleaned(t, payload, false)
	if strings.Contains(got, "deep-secret") {
		t.Errorf("a nested credential survived:\n%s", got)
	}
	if !strings.Contains(got, "Nicolas") {
		t.Errorf("ordinary nested data was lost:\n%s", got)
	}
}

// detail asks for whole bodies instead of shortened ones. It must not be a
// back door: there is deliberately no way to see a credential through MCP.
func TestDetailDoesNotRevealCredentials(t *testing.T) {
	payload := `{"headers":{"Authorization":["Bearer SECRET"]},"body":"ordinary"}`

	got := cleaned(t, payload, true)
	if strings.Contains(got, "SECRET") {
		t.Errorf("detail leaked a credential:\n%s", got)
	}
}

// Replacing a list with a string changes the shape of the reply, and a client
// parsing headers would break on the one field it was never going to read.
func TestRedactionKeepsTheShape(t *testing.T) {
	v, err := cleanAnswer("/api/calls", []byte(`{"headers":{"Cookie":["a","b"]}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	headers := v.(map[string]any)["headers"].(map[string]any)
	values, ok := headers["Cookie"].([]any)
	if !ok {
		t.Fatalf("Cookie is no longer a list: %T", headers["Cookie"])
	}
	if len(values) != 2 {
		t.Errorf("the list lost entries: %v", values)
	}
	for _, entry := range values {
		if entry != redacted {
			t.Errorf("entry = %v, want %q", entry, redacted)
		}
	}
}

// A four megabyte body in an agent's context is expensive and useless.
func TestLongBodiesAreShortenedUnlessAsked(t *testing.T) {
	long := strings.Repeat("x", maxString*3)
	payload := `{"body":"` + long + `"}`

	short := cleaned(t, payload, false)
	if len(short) > maxString+200 {
		t.Errorf("the body was not shortened: %d characters", len(short))
	}
	if !strings.Contains(short, "ask for detail") {
		t.Error("the shortened body does not say how to get the rest")
	}

	whole := cleaned(t, payload, true)
	if !strings.Contains(whole, long) {
		t.Error("detail did not return the whole body")
	}
}

func TestOrdinaryDataIsUntouched(t *testing.T) {
	payload := `{"id":42,"target":"ms-auth","status":500,"grpc_status":13,"ok":false,"path":"/v1/orders"}`
	got := cleaned(t, payload, false)

	for _, want := range []string{`"id":42`, `"ms-auth"`, `"status":500`, `"grpc_status":13`, `"ok":false`, `/v1/orders`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was altered:\n%s", want, got)
		}
	}
}

func TestIsSensitiveDoesNotOverreach(t *testing.T) {
	// These names contain no credential and are worth reading.
	//
	// `code`, `key` and `sig` are in the list on purpose: they are credentials
	// in a query string and nothing of the sort as a field name. `code` is the
	// SQLSTATE of every Postgres error and an HTTP status besides, and putting
	// it in the general list would blank the most useful field a failed capture
	// has — and make namesCredential fire on any statement touching
	// `country_code`.
	for _, key := range []string{
		"target", "status", "path", "method", "duration_ms", "protocol", "content-type", "user-agent",
		"code", "key", "sig", "message",
	} {
		if isSensitive(key) {
			t.Errorf("%q is redacted but carries nothing secret", key)
		}
	}
	// In a query string those three are exactly the credential.
	for _, key := range []string{"code", "key", "sig", "X-Amz-Signature"} {
		if !isSensitiveParam(key) {
			t.Errorf("?%s= is NOT redacted", key)
		}
	}
	for _, key := range []string{"Authorization", "cookie", "Set-Cookie", "apiKey", "x-api-key", "password", "sessionId"} {
		if !isSensitive(key) {
			t.Errorf("%q is NOT redacted", key)
		}
	}
}

// A captured body is stored as one opaque string, so the walk over the reply's
// own structure never sees the keys inside it. This was a real leak, found by
// sending a request through Sonda with a password in the body and reading it
// back: the three headers came out redacted and the body did not.
func TestCredentialsInsideABodyAreRedactedToo(t *testing.T) {
	payload := `{"request":{
	  "headers":{"Authorization":["Bearer HEADER-SECRET"]},
	  "text":"{\"usuario\":\"nicolas\",\"password\":\"BODY-SECRET\",\"sku\":\"ABC-9\"}"
	}}`

	got := cleaned(t, payload, true)

	if strings.Contains(got, "BODY-SECRET") {
		t.Errorf("a credential inside the body survived:\n%s", got)
	}
	if strings.Contains(got, "HEADER-SECRET") {
		t.Errorf("a credential in the headers survived:\n%s", got)
	}
	// And the rest of the body is still readable, or the tool loses its point.
	for _, want := range []string{"nicolas", "ABC-9"} {
		if !strings.Contains(got, want) {
			t.Errorf("ordinary body data was lost (%s):\n%s", want, got)
		}
	}
}

// A body that is not JSON must come back exactly as it was. Re-encoding it, or
// mangling a plain string that merely starts with a brace, would corrupt the
// one thing this tool exists to show.
func TestNonJSONBodiesArePassedThroughUnchanged(t *testing.T) {
	for _, body := range []string{
		"plain text response",
		"user=nicolas&sku=ABC-9",
		"<xml><sku>ABC-9</sku></xml>",
		"{not really json",
		"[unclosed",
		// Query-string redaction reads every string looking for a `?`. One that
		// holds no credential has to come back byte for byte, or an ordinary
		// body is corrupted by the machinery meant to protect it.
		"GET /search?q=hello&sort=asc HTTP/1.1",
		"is that a question? yes = definitely",
	} {
		payload, err := json.Marshal(map[string]any{"text": body})
		if err != nil {
			t.Fatal(err)
		}
		// Compared on the value, not on its encoding: Go escapes &, < and > as
		// & and friends when marshalling, so asserting against the JSON
		// text would fail on bodies this function never touched.
		v, err := cleanAnswer("/api/calls", payload, true)
		if err != nil {
			t.Fatal(err)
		}
		got := v.(map[string]any)["text"]
		if got != body {
			t.Errorf("body came back altered\n  sent: %q\n  got:  %q", body, got)
		}
	}
}

// The decoded GraphQL view hands variables out as real JSON rather than as a
// string holding JSON, so the walk sees the keys directly. A login mutation is
// the highest-value secret in a whole capture file, and it travels here.
func TestGraphQLVariablesAreRedactedLikeAnyOtherPayload(t *testing.T) {
	out := cleaned(t, `{
	  "graphql": {
	    "operations": [
	      {"label": "mutation Login",
	       "variables": {"email": "nico@delpaintl.com", "password": "hunter2"}}
	    ]
	  }
	}`, true)

	if strings.Contains(out, "hunter2") {
		t.Errorf("a password in GraphQL variables left the machine:\n%s", out)
	}
	if !strings.Contains(out, "nico@delpaintl.com") {
		t.Errorf("the rest of the variables were lost with it:\n%s", out)
	}
}

// A captured path is stored as the whole request URI, so a credential in the
// query string reaches an agent under the key "path" — which is not sensitive,
// and which travels in the summary, so it arrives on the first tool call
// without anyone asking for detail. Nothing covered this before.
func TestCredentialsInAQueryStringAreRedacted(t *testing.T) {
	// Built the way the proxy builds it: r.URL.RequestURI() of a real request.
	u, err := url.Parse("http://auth.internal/oauth/callback?state=xyz789&access_token=ya29.A0ARrdaM-SECRET&expires_in=3599")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"calls": []any{map[string]any{
		"id": 41, "target": "auth", "protocol": "http", "method": "GET",
		"path": u.RequestURI(), "status": 302, "duration_ms": 12.4,
	}}})
	if err != nil {
		t.Fatal(err)
	}

	got := cleaned(t, string(payload), false)

	if strings.Contains(got, "ya29.A0ARrdaM-SECRET") {
		t.Errorf("an OAuth token in the query string left the machine:\n%s", got)
	}
	// The path is how a person recognises the call. Blanking it whole would be
	// safe and useless.
	for _, want := range []string{"/oauth/callback", "state=xyz789", "expires_in=3599", "access_token="} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was lost with it:\n%s", want, got)
		}
	}
}

func TestQueryRedactionHandlesTheAwkwardShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		gone     []string
		kept     []string
		verbatim bool
	}{
		{name: "no query at all", path: "/v1/orders/42", verbatim: true},
		{name: "nothing sensitive", path: "/v1/orders?page=2&sort=desc", verbatim: true},
		{
			name: "the same key twice",
			path: "/v1/x?token=FIRST&other=keep&token=SECOND",
			gone: []string{"FIRST", "SECOND"},
			kept: []string{"other=keep"},
		},
		{
			name: "a parameter with no value",
			path: "/v1/x?debug&api_key=LEAK&trace",
			gone: []string{"LEAK"},
			kept: []string{"debug", "trace"},
		},
		{
			name: "a fragment after the query",
			path: "/v1/x?access_token=LEAK#section-2",
			gone: []string{"LEAK"},
			kept: []string{"#section-2"},
		},
		{
			name: "a percent-encoded key",
			path: "/v1/x?%61ccess%5Ftoken=LEAK",
			gone: []string{"LEAK"},
		},
		{
			name: "an absolute URL in a Location header",
			path: "https://app.example.com/land?session=LEAK&ref=email",
			gone: []string{"LEAK"},
			kept: []string{"https://app.example.com/land", "ref=email"},
		},
		{
			// Deprecated, still parsed by real servers, and a filter that only
			// knew `&` returned the whole thing verbatim.
			name: "a semicolon separator, not first",
			path: "/cb?debug=1;api_key=LEAK;page=2",
			gone: []string{"LEAK"},
			kept: []string{"debug=1", "page=2"},
		},
		{
			// Only the first `?` used to be found, and its first pair swallowed
			// everything after it — including a second URL.
			name: "two URLs in one string",
			path: "see https://a.local/x?ok=1 then https://b.local/y?api_key=LEAK",
			gone: []string{"LEAK"},
			kept: []string{"ok=1", "https://b.local/y"},
		},
		{
			name: "an OAuth authorization code",
			path: "/oauth/callback?code=4%2FLEAK&state=xyz789",
			gone: []string{"4%2FLEAK"},
			kept: []string{"state=xyz789"},
		},
		{
			name: "a presigned URL",
			path: "/bucket/o.png?X-Amz-Credential=AKIA%2Fx&X-Amz-Signature=LEAK&X-Amz-Date=20260806",
			gone: []string{"LEAK"},
			kept: []string{"X-Amz-Date=20260806"},
		},
		{
			name: "the short form of a signature",
			path: "/blob?sig=LEAK&se=2026-08-06",
			gone: []string{"LEAK"},
			kept: []string{"se=2026-08-06"},
		},
		{
			// `sig` is matched whole precisely so these are not touched.
			name:     "words that merely contain a sensitive one",
			path:     "/v1/x?design=flat&assign=nico&keyword=sig",
			verbatim: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactQuery(tc.path)
			if tc.verbatim && got != tc.path {
				t.Fatalf("an untouchable path was altered\n  sent: %q\n  got:  %q", tc.path, got)
			}
			for _, secret := range tc.gone {
				if strings.Contains(got, secret) {
					t.Errorf("%q survived: %s", secret, got)
				}
			}
			for _, want := range tc.kept {
				if !strings.Contains(got, want) {
					t.Errorf("%q was lost: %s", want, got)
				}
			}
		})
	}
}

// A redirect to an OAuth callback puts the same credential in a response
// header, under a name the word list has no reason to hold.
func TestCredentialsInARedirectAreRedacted(t *testing.T) {
	got := cleaned(t, `{"response":{"headers":{
	  "Location":["https://app.local/cb?code=4%2Fabc&access_token=ya29.LEAKED"],
	  "Content-Type":["text/html"]}}}`, true)

	if strings.Contains(got, "ya29.LEAKED") {
		t.Errorf("a token in a Location header left the machine:\n%s", got)
	}
	if !strings.Contains(got, "app.local/cb") {
		t.Errorf("the redirect target was lost with it:\n%s", got)
	}
}

// The Postgres shapes below are built from real protocol bytes and read back
// through pgwire, so the tests see the JSON the API actually serves rather than
// a convenient hand-written version of it.

func pgMsg(typ byte, body ...[]byte) []byte {
	var payload []byte
	for _, p := range body {
		payload = append(payload, p...)
	}
	out := []byte{typ}
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)+4))
	return append(out, payload...)
}

func pgStr(s string) []byte { return append([]byte(s), 0) }
func pgU16(v int) []byte    { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
func pgU32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func pgI32(v int32) []byte  { return pgU32(uint32(v)) }

// pgColumn is one entry of a RowDescription: name, table OID, column number,
// type OID, type length, type modifier, format code.
func pgColumn(name string) []byte {
	out := pgStr(name)
	out = append(out, pgU32(16384)...)
	out = append(out, pgU16(1)...)
	out = append(out, pgU32(25)...) // text
	out = append(out, pgU16(65535)...)
	out = append(out, pgU32(4294967295)...)
	return append(out, pgU16(0)...)
}

func pgText(s string) []byte { return append(pgI32(int32(len(s))), s...) }

// pgCapture runs the bytes through the same decoder and the same JSON encoding
// the /api/calls/{id} handler uses.
func pgCapture(t *testing.T, sent, received []byte) string {
	t.Helper()
	fromClient, _ := pgwire.Deframe(sent, true)
	fromServer, _ := pgwire.Deframe(received, false)
	payload, err := json.Marshal(map[string]any{
		"id":       88,
		"protocol": "postgres",
		"postgres": map[string]any{"sent": fromClient, "received": fromServer},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// The column name is in a value position and the secret is in another message
// entirely, so key matching structurally cannot see either.
func TestSensitiveColumnsBlankTheirValues(t *testing.T) {
	sent := pgMsg('Q', pgStr("SELECT api_key, label FROM tokens WHERE owner = 12"))
	received := pgMsg('T', pgU16(2), pgColumn("api_key"), pgColumn("label"))
	received = append(received, pgMsg('D', pgU16(2), pgText("sk_live_9f8e7d6c"), pgText("staging key"))...)
	received = append(received, pgMsg('C', pgStr("SELECT 1"))...)

	got := cleanedCall(t, pgCapture(t, sent, received), true)

	if strings.Contains(got, "sk_live_9f8e7d6c") {
		t.Errorf("a secret column value left the machine:\n%s", got)
	}
	// The aligned column beside it is ordinary data and the statement is the
	// reason the capture was opened. Both stay.
	for _, want := range []string{"staging key", "api_key", "FROM tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was lost:\n%s", want, got)
		}
	}
}

// A statement that names a credential carries its literals in the clear, and
// "sql" is not a sensitive key.
func TestLiteralsInACredentialStatementAreBlanked(t *testing.T) {
	sent := pgMsg('Q', pgStr("INSERT INTO users (email, password) VALUES ('nico@delpaintl.com', 'hunter2')"))

	got := cleanedCall(t, pgCapture(t, sent, nil), true)

	if strings.Contains(got, "hunter2") {
		t.Errorf("a password literal left the machine:\n%s", got)
	}
	// Everything that makes the statement readable survives.
	for _, want := range []string{"INSERT INTO users", "email", "password", "VALUES"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was lost, the statement is no longer readable:\n%s", want, got)
		}
	}
}

// Dollar quoting needs no escaping, so a scanner that only knows about single
// quotes walks straight past the secret.
func TestDollarQuotedLiteralsAreBlankedToo(t *testing.T) {
	sent := pgMsg('Q', pgStr("UPDATE accounts SET secret = $tag$ hun'ter2 $tag$ WHERE id = 3"))

	got := cleanedCall(t, pgCapture(t, sent, nil), true)

	if strings.Contains(got, "hun'ter2") {
		t.Errorf("a dollar-quoted secret left the machine:\n%s", got)
	}
	if !strings.Contains(got, "UPDATE accounts") {
		t.Errorf("the statement was lost with it:\n%s", got)
	}
}

// A bind parameter is a position with no name. The statement that gives it
// meaning arrived in an earlier Parse, under a name the Bind refers back to.
func TestBindParametersOfACredentialStatementAreBlanked(t *testing.T) {
	parse := pgMsg('P', pgStr("s1"), pgStr("UPDATE users SET password_hash = $1 WHERE id = $2"), pgU16(2), pgU32(25), pgU32(23))
	bind := pgMsg('B', pgStr(""), pgStr("s1"), pgU16(0), pgU16(2), pgText("$2a$10$REALHASH"), pgText("4711"))

	got := cleanedCall(t, pgCapture(t, append(parse, bind...), nil), true)

	if strings.Contains(got, "REALHASH") {
		t.Errorf("a bound credential left the machine:\n%s", got)
	}
	if !strings.Contains(got, "password_hash") {
		t.Errorf("the statement was lost with it:\n%s", got)
	}
}

// The blunt instruments above only fire on a statement that names a
// credential. An ordinary query is the reason the tool exists, and it must come
// back whole — values included.
func TestOrdinaryStatementsKeepTheirValues(t *testing.T) {
	parse := pgMsg('P', pgStr(""), pgStr("SELECT id, total FROM orders WHERE status = 'paid' AND city = $1"), pgU16(1), pgU32(25))
	bind := pgMsg('B', pgStr(""), pgStr(""), pgU16(0), pgU16(1), pgText("Santiago"))
	received := pgMsg('T', pgU16(2), pgColumn("id"), pgColumn("total"))
	received = append(received, pgMsg('D', pgU16(2), pgText("9001"), pgText("14990"))...)

	got := cleanedCall(t, pgCapture(t, append(parse, bind...), received), true)

	for _, want := range []string{"'paid'", "Santiago", "9001", "14990"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was redacted from an ordinary query:\n%s", want, got)
		}
	}
}

// Truncation used to happen while walking, so the credential gate read a
// statement that had already been cut at maxString and the literals past the
// cut came back in the clear. That inverted the safety property: detail:false,
// the default an agent gets without asking, was the leaky answer.
func TestTheCredentialGateSeesTheWholeStatement(t *testing.T) {
	bio := strings.Repeat("x", maxString+100)
	sent := pgMsg('Q', pgStr("UPDATE users SET nickname='hunter2', bio='"+bio+"', password='p4ssw0rd'"))
	capture := pgCapture(t, sent, nil)

	for _, detail := range []bool{false, true} {
		got := cleanedCall(t, capture, detail)
		for _, secret := range []string{"hunter2", "p4ssw0rd"} {
			if strings.Contains(got, secret) {
				t.Errorf("detail=%v leaked %q:\n%s", detail, secret, got)
			}
		}
	}
}

// The Postgres pass reads neighbours, which is the one thing a per-key walk
// cannot do — and it used to do it to any list at all. A captured body that
// happened to carry the same key names was rewritten, which breaks the property
// the whole tool rests on: for a body, the stored bytes are the record, and this
// is the one surface where the reader cannot check them against the interface.
func TestAnUnrelatedPayloadIsNotRewrittenAsPostgres(t *testing.T) {
	got := cleaned(t, `{"response":{"json":[
	  {"sql":"SELECT * FROM tokens"},
	  {"columns":[{"name":"api_key"}]},
	  {"params":[{"text":"totally unrelated"}],"values":[{"text":"also unrelated"}]}
	]}}`, true)

	for _, want := range []string{"totally unrelated", "also unrelated"} {
		if !strings.Contains(got, want) {
			t.Errorf("an ordinary payload was rewritten as if it were Postgres (%q gone):\n%s", want, got)
		}
	}
}

// A row longer than the description before it cannot be aligned past the last
// column. The tail used to be skipped, which reads "no name to check" as "safe".
func TestValuesBeyondTheDescriptionAreBlanked(t *testing.T) {
	sent := pgMsg('Q', pgStr("SELECT label FROM labels"))
	received := pgMsg('T', pgU16(1), pgColumn("label"))
	received = append(received, pgMsg('D', pgU16(2), pgText("staging"), pgText("sk_live_EXTRA"))...)

	got := cleanedCall(t, pgCapture(t, sent, received), true)

	if strings.Contains(got, "sk_live_EXTRA") {
		t.Errorf("an unaligned value left the machine:\n%s", got)
	}
	if !strings.Contains(got, "staging") {
		t.Errorf("the aligned value was lost with it:\n%s", got)
	}
}

// An alias defeats alignment entirely: the RowDescription names "k" and the key
// comes back in the clear. The statement is in the same capture and it names a
// credential, so when no described column does, the row goes whole — the same
// blunt rule bind parameters already follow.
func TestAnAliasedCredentialColumnBlanksTheRow(t *testing.T) {
	sent := pgMsg('Q', pgStr("SELECT api_key AS k, owner AS o FROM tokens"))
	received := pgMsg('T', pgU16(2), pgColumn("k"), pgColumn("o"))
	received = append(received, pgMsg('D', pgU16(2), pgText("sk_live_9f8e7d6c"), pgText("12"))...)

	got := cleanedCall(t, pgCapture(t, sent, received), true)

	if strings.Contains(got, "sk_live_9f8e7d6c") {
		t.Errorf("an aliased secret column left the machine:\n%s", got)
	}
	if !strings.Contains(got, "SELECT api_key AS k") {
		t.Errorf("the statement was lost with it:\n%s", got)
	}
}

// A server error echoes the value it choked on, under field names no key match
// has any reason to hold.
func TestAServerErrorDoesNotEchoTheValue(t *testing.T) {
	sent := pgMsg('Q', pgStr("SELECT id FROM users WHERE password_hash = 'x'"))
	received := pgError("28P01", `password authentication failed for user "hunter3"`)

	got := cleanedCall(t, pgCapture(t, sent, received), true)

	if strings.Contains(got, "hunter3") {
		t.Errorf("a value echoed by the server left the machine:\n%s", got)
	}
	if !strings.Contains(got, "28P01") {
		t.Errorf("the SQLSTATE went with it, which is the actionable part:\n%s", got)
	}
}

// --- through a real tool ---

// pgError builds an ErrorResponse: severity, SQLSTATE and message, then the
// zero byte that ends the field list.
func pgError(code, message string) []byte {
	body := append([]byte{'S'}, pgStr("ERROR")...)
	body = append(body, 'C')
	body = append(body, pgStr(code)...)
	body = append(body, 'M')
	body = append(body, pgStr(message)...)
	return pgMsg('E', append(body, 0))
}

// sondaHolding writes the calls into a real store and hands back the MCP server
// in front of the real API. Every other test in this file calls cleanAnswer
// directly, which is how a leak in a field no test constructed — the one-line
// summary — survived: it is derived on insert and it is a plain string, so
// nothing in a hand-written payload ever contained it.
func sondaHolding(t *testing.T, calls ...*store.Call) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sonda.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if _, err := db.Insert(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return sonda(t, path)
}

func pgCall(sent, received []byte) *store.Call {
	return &store.Call{
		Target: "db", Protocol: config.ProtocolPostgres, Method: "STATEMENT", Path: "app",
		StartedAt: time.Now().UTC(),
		Request:   store.Message{Body: sent, Size: int64(len(sent))},
		Response:  store.Message{Body: received, Size: int64(len(received))},
	}
}

// The summary is the field an agent reads first, on the first tool call, with
// no detail flag — and it is where a secret was travelling verbatim.
func TestNoSecretLeavesThroughARealToolCall(t *testing.T) {
	s := sondaHolding(t,
		pgCall(
			pgMsg('Q', pgStr("INSERT INTO users (email, password) VALUES ('nico@delpaintl.com', 'hunter2')")),
			pgMsg('C', pgStr("INSERT 0 1")),
		),
		pgCall(
			pgMsg('Q', pgStr("SELECT id FROM users WHERE password_hash = 'p4ssw0rd'")),
			pgError("28P01", `password authentication failed for user "hunter3"`),
		),
	)

	found, isError := callTool(t, s, "search_calls", `{"protocol":"postgres"}`)
	if isError {
		t.Fatalf("search_calls failed: %s", found)
	}
	failures, isError := callTool(t, s, "recent_failures", "")
	if isError {
		t.Fatalf("recent_failures failed: %s", failures)
	}

	for _, answer := range []string{found, failures} {
		for _, secret := range []string{"hunter2", "hunter3", "p4ssw0rd"} {
			if strings.Contains(answer, secret) {
				t.Errorf("%q reached an agent through a tool call:\n%s", secret, answer)
			}
		}
	}
	// And the summaries are still what they are for: recognising which capture
	// is which, and why one of them failed.
	if !strings.Contains(found, "INSERT INTO users") {
		t.Errorf("the statement summary was lost:\n%s", found)
	}
	if !strings.Contains(failures, "28P01") {
		t.Errorf("the SQLSTATE was lost:\n%s", failures)
	}

	// The same capture in full, which is the surface detail:true opens.
	var listed struct {
		Calls []struct {
			ID     int64  `json:"id"`
			Failed bool   `json:"failed"`
			Errors int    `json:"postgres_errors"`
			Line   string `json:"postgres_summary"`
		} `json:"calls"`
	}
	if err := json.Unmarshal([]byte(found), &listed); err != nil {
		t.Fatal(err)
	}
	for _, call := range listed.Calls {
		whole, isError := callTool(t, s, "get_call", `{"id":`+strconv.FormatInt(call.ID, 10)+`,"detail":true}`)
		if isError {
			t.Fatalf("get_call failed: %s", whole)
		}
		for _, secret := range []string{"hunter2", "hunter3", "p4ssw0rd"} {
			if strings.Contains(whole, secret) {
				t.Errorf("%q reached an agent through get_call:\n%s", secret, whole)
			}
		}
	}
}

// A trace repeats each node's one-line reading twice: as the node's `detail`,
// and inside the tree it also renders as text. Both are gated, and the SQLSTATE
// — the part worth having — survives in both.
func TestTheTraceSummaryIsGatedInBothCopies(t *testing.T) {
	s := sondaHolding(t, pgCall(
		pgMsg('Q', pgStr("SELECT id FROM users WHERE password_hash = 'p4ssw0rd'")),
		pgError("28P01", `password authentication failed for user "hunter3"`),
	))

	tree, isError := callTool(t, s, "trace_call", `{"id":1}`)
	if isError {
		t.Fatalf("trace_call failed: %s", tree)
	}

	for _, secret := range []string{"hunter3", "p4ssw0rd"} {
		if strings.Contains(tree, secret) {
			t.Errorf("%q reached an agent through trace_call:\n%s", secret, tree)
		}
	}
	// Twice, because the drawn tree used to carry what the node no longer did.
	if got := strings.Count(tree, "28P01"); got != 2 {
		t.Errorf("the SQLSTATE survives in %d of the two copies:\n%s", got, tree)
	}
}

// `detail` is FastAPI's error shape and RFC 7807's, so it is the most common
// failure body there is. It used to be sent through the Postgres summary gate
// wherever it appeared, and an apostrophe in ordinary prose opens a SQL literal
// that never closes — so the body came back cut off at the apostrophe, in the
// tool that exists to show failures.
func TestAnOrdinaryDetailFieldIsNotReadAsSQL(t *testing.T) {
	body := `{"detail":"the user's password was rejected by the identity provider","status":401}`
	s := sondaHolding(t, &store.Call{
		Target: "api", Protocol: config.ProtocolHTTP, Method: "POST", Path: "/v1/login",
		Status: 401, StartedAt: time.Now().UTC(),
		Response: store.Message{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    []byte(body), Size: int64(len(body)),
		},
	})

	got, isError := callTool(t, s, "get_call", `{"id":1,"detail":true}`)
	if isError {
		t.Fatalf("get_call failed: %s", got)
	}
	if !strings.Contains(got, `rejected by the identity provider`) {
		t.Errorf("an ordinary error body was corrupted by the summary gate:\n%s", got)
	}
}

// --- the second copy of a decoded stream ---

// wsFrame builds one whole WebSocket frame the way an endpoint would. A frame
// from a client is masked, which is what makes a captured request stream
// invalid UTF-8 and sends it out as base64.
func wsFrame(opcode int, payload []byte, mask *[4]byte) []byte {
	out := []byte{byte(opcode) | 0x80}
	second := byte(0)
	if mask != nil {
		second |= 0x80
	}
	out = append(out, second|byte(len(payload)))
	if mask == nil {
		return append(out, payload...)
	}
	out = append(out, mask[:]...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	return out
}

// grpcFrame is the length-prefixed message framing: one compression byte, four
// bytes of length, then the protobuf.
func grpcFrame(message []byte) []byte {
	out := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(message)))
	return append(out, message...)
}

// protoString encodes one length-delimited protobuf field.
func protoString(number int, value string) []byte {
	out := []byte{byte(number<<3 | 2), byte(len(value))}
	return append(out, value...)
}

// Every one of these protocols is served twice — decoded under its own view and
// verbatim beside it — and the second copy used to walk straight out. Read back
// through a real tool call, because that is the only place the two copies exist
// together.
func TestTheVerbatimCopyOfADecodedStreamDoesNotLeave(t *testing.T) {
	// The server's frames are never masked, which is what makes the second copy
	// of this direction plainly legible. The client's are, and that direction is
	// checked further down.
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	socket := &store.Call{
		Target: "chat", Protocol: config.ProtocolWebSocket, Method: "GET", Path: "/ws",
		StartedAt: time.Now().UTC(),
		Request: store.Message{
			Body: wsFrame(1, []byte(`{"access_token":"WS-SENT-SECRET"}`), &mask),
		},
		Response: store.Message{Body: wsFrame(1, []byte(`{"refresh_token":"WS-SECRET","room":"ops"}`), nil)},
	}

	events := "data: {\"access_token\":\"SSE-SECRET\"}\n\n"
	stream := &store.Call{
		Target: "events", Protocol: config.ProtocolHTTP, Method: "GET", Path: "/v1/stream",
		Status: 200, StartedAt: time.Now().UTC(),
		Request: store.Message{Body: []byte(`{"channel":"orders"}`)},
		Response: store.Message{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:    []byte(events), Size: int64(len(events)),
		},
	}

	grpc := &store.Call{
		Target: "auth", Protocol: config.ProtocolGRPC, Method: "Login", Path: "/auth.v1.Auth/Login",
		Status: 200, StartedAt: time.Now().UTC(),
		Request:  store.Message{Body: grpcFrame(protoString(1, "GRPC-SECRET"))},
		Response: store.Message{Body: grpcFrame(protoString(1, "session-4711"))},
	}

	s := sondaHolding(t, socket, stream, grpc)

	for _, tc := range []struct {
		id     int
		secret string

		// closed says the decoded view is redacted as well, so nothing anywhere
		// in the answer carries the secret. It is false for a gRPC capture with
		// no schema: a protobuf field decoded without a descriptor has a number
		// and no name, so key matching has nothing to match on and the value is
		// returned in the clear. Dropping the verbatim copy still removes a
		// second, differently shaped leak; the decoded half stays open and
		// SECURITY.md says which half is which.
		closed bool
	}{
		{id: 1, secret: "WS-SECRET", closed: true},
		{id: 2, secret: "SSE-SECRET", closed: true},
		{id: 3, secret: "GRPC-SECRET"},
	} {
		got, isError := callTool(t, s, "get_call", `{"id":`+strconv.Itoa(tc.id)+`,"detail":true}`)
		if isError {
			t.Fatalf("get_call %d failed: %s", tc.id, got)
		}
		if strings.Contains(verbatimCopy(t, got), tc.secret) {
			t.Errorf("the verbatim copy carried %q out:\n%s", tc.secret, got)
		}
		if tc.closed && strings.Contains(got, tc.secret) {
			t.Errorf("%q left through the decoded view:\n%s", tc.secret, got)
		}
	}

	socketCall, _ := callTool(t, s, "get_call", `{"id":1,"detail":true}`)

	// A frame a client sends is masked, so its secret is not literally in the
	// captured bytes: it is one XOR away, with the key sitting in the frame
	// beside it. Searching that copy for the plaintext would call the direction
	// clean while it leaks, so it is pinned on the copy being gone instead.
	var sent struct {
		Request rawMessage `json:"request"`
	}
	if err := json.Unmarshal([]byte(socketCall), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Request.Base64 != redacted {
		t.Errorf("the client's half of the socket kept its bytes: %q", sent.Request.Base64)
	}

	// And the decoded view still answers what the raw copy used to. A socket
	// keeps every frame, an event stream keeps its events, and the request of
	// an event stream — which nothing decodes — is left alone rather than
	// blanked for symmetry.
	if !strings.Contains(socketCall, "ops") {
		t.Errorf("the decoded frames went with the raw stream:\n%s", socketCall)
	}
	streamCall, _ := callTool(t, s, "get_call", `{"id":2,"detail":true}`)
	if !strings.Contains(streamCall, "orders") {
		t.Errorf("the request body of an event stream was dropped, and nothing decodes it:\n%s", streamCall)
	}
}

// verbatimCopy returns just the two raw copies of a capture, base64 decoded so
// that the encoding cannot hide a secret from a search. Asserting on these
// rather than on the whole answer is what keeps this test about the second
// copy and not about what the decoded view does or does not reach.
func verbatimCopy(t *testing.T, answer string) string {
	t.Helper()
	var call struct {
		Request  rawMessage `json:"request"`
		Response rawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(answer), &call); err != nil {
		t.Fatal(err)
	}
	return call.Request.plain(t) + call.Response.plain(t)
}

type rawMessage struct {
	Text   string `json:"text"`
	Base64 string `json:"base64"`
}

func (m rawMessage) plain(t *testing.T) string {
	t.Helper()
	if m.Base64 == "" {
		return m.Text
	}
	// A blanked field is the marker, not base64, and failing to decode it is
	// not a finding.
	raw, err := base64.StdEncoding.DecodeString(m.Base64)
	if err != nil {
		return m.Text + m.Base64
	}
	return m.Text + string(raw)
}

// The other half of the same capture. With a descriptor set the field has its
// real name back, which is all ordinary key matching ever needed — this pins
// that it really is enough, and that the raw copy goes with it.
func TestAGRPCMessageWithASchemaIsRedactedByItsFieldNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonda.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "auth-system")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDescriptorSet(context.Background(), project.ID, "auth.binpb", authDescriptorSet(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveService(context.Background(), store.Service{
		ProjectID: project.ID, Name: "auth", Listen: "127.0.0.1:59051",
		Upstream: "http://127.0.0.1:50051", Protocol: config.ProtocolGRPC,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateProject(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}

	body := append(protoString(1, "SCHEMA-SECRET"), protoString(2, "nicolas")...)
	if _, err := db.Insert(context.Background(), &store.Call{
		Target: "auth", Protocol: config.ProtocolGRPC, Method: "Login",
		Path: "/auth.v1.Auth/Login", Status: 200, Project: "auth-system",
		StartedAt: time.Now().UTC(),
		Request:   store.Message{Body: grpcFrame(body), Size: int64(len(body) + 5)},
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	got, isError := callTool(t, sonda(t, path), "get_call", `{"id":1,"detail":true}`)
	if isError {
		t.Fatalf("get_call failed: %s", got)
	}
	if strings.Contains(verbatimCopy(t, got), "SCHEMA-SECRET") {
		t.Errorf("the verbatim copy carried the token out:\n%s", got)
	}
	if strings.Contains(got, "SCHEMA-SECRET") {
		t.Errorf("a named protobuf field was not redacted:\n%s", got)
	}
	// The field beside it is ordinary data, and the schema is what made the
	// difference — both have to be visible or the answer proves nothing.
	for _, want := range []string{"nicolas", "descriptor_set"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing, so this is not the schema-backed path:\n%s", want, got)
		}
	}
}

// authDescriptorSet compiles a one-message schema in memory. The demo proto
// this repository ships has no credential-shaped field in it, and adding one
// would mean regenerating a committed descriptor set to prove something about
// field names.
func authDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := descpb.FieldDescriptorProto_TYPE_STRING
	optional := descpb.FieldDescriptorProto_LABEL_OPTIONAL
	field := func(name string, number int32) *descpb.FieldDescriptorProto {
		return &descpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number),
			Type: &str, Label: &optional, JsonName: proto.String(name),
		}
	}
	set := &descpb.FileDescriptorSet{File: []*descpb.FileDescriptorProto{{
		Name:    proto.String("auth/v1/auth.proto"),
		Package: proto.String("auth.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descpb.DescriptorProto{
			{
				Name:  proto.String("LoginRequest"),
				Field: []*descpb.FieldDescriptorProto{field("access_token", 1), field("user", 2)},
			},
			{Name: proto.String("LoginReply"), Field: []*descpb.FieldDescriptorProto{field("session", 1)}},
		},
		Service: []*descpb.ServiceDescriptorProto{{
			Name: proto.String("Auth"),
			Method: []*descpb.MethodDescriptorProto{{
				Name:       proto.String("Login"),
				InputType:  proto.String(".auth.v1.LoginRequest"),
				OutputType: proto.String(".auth.v1.LoginReply"),
			}},
		}},
	}}}
	out, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- position, not pattern ---
//
// Every test below goes through a real tool call. Four rounds of fixes were
// checked by calling the redaction function with a hand-written payload, and
// three of the leaks they left survived precisely because no hand-written
// payload has the shape the API actually serves.

// A tree is redacted at every node, at every depth. The rule this replaced
// recognised a summary by counting the words before the first colon, and a
// branch character counts as a word — so it fired on the root and on the last
// child and left everything in between in the clear.
func TestEveryNodeOfADrawnTreeIsRedacted(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute)
	span := func(offset, duration time.Duration, c *store.Call) *store.Call {
		c.StartedAt = base.Add(offset)
		c.Duration = duration
		c.TraceID = "0af7651916cd43dd8448eb211c80319c"
		return c
	}
	plain := func(path string) *store.Call {
		return &store.Call{Target: "api", Protocol: config.ProtocolHTTP, Method: "GET", Path: path, Status: 200}
	}
	// A failed statement, whose one-line summary is what a node's detail holds.
	failing := func(user string) *store.Call {
		return pgCall(
			pgMsg('Q', pgStr("SELECT id FROM users WHERE password_hash = 'p4ssw0rd'")),
			pgError("28P01", `password authentication failed for user "`+user+`"`),
		)
	}

	// Nested by containment, so the two statements land as a middle child and a
	// grandchild — the two places the old rule could not reach.
	s := sondaHolding(t,
		span(0, 300*time.Millisecond, plain("/v1/checkout")),
		span(10*time.Millisecond, 200*time.Millisecond, failing("MIDDLE-CHILD")),
		span(20*time.Millisecond, 50*time.Millisecond, failing("GRANDCHILD")),
		span(230*time.Millisecond, 30*time.Millisecond, plain("/v1/receipt")),
	)

	tree, isError := callTool(t, s, "trace_call", `{"id":2}`)
	if isError {
		t.Fatalf("trace_call failed: %s", tree)
	}

	// The shape this test claims, asserted rather than assumed: a flat tree
	// would pass everything below without proving anything.
	if !strings.Contains(tree, "├─") {
		t.Fatalf("no node has a sibling after it, so this is not the shape that leaked:\n%s", tree)
	}
	for _, secret := range []string{"MIDDLE-CHILD", "GRANDCHILD", "p4ssw0rd"} {
		if strings.Contains(tree, secret) {
			t.Errorf("%q reached an agent through trace_call:\n%s", secret, tree)
		}
	}
	// Two failing nodes, each carrying its SQLSTATE twice: as the node's detail
	// and in the drawing. Fewer means a copy was blanked whole instead of gated.
	if got := strings.Count(tree, "28P01"); got != 4 {
		t.Errorf("the SQLSTATE survives %d times, want 4 (two nodes, two copies each):\n%s", got, tree)
	}
}

// A node's detail is a Postgres summary in one of four cases and prose in the
// other three. Reading prose as SQL is how an apostrophe opened a literal that
// never closed and the message came back cut in half — in the tool that exists
// to show failures. This message also names two words from the credential list,
// so blanking it whole would fail here too.
func TestAGRPCStatusMessageSurvivesInAllItsCopies(t *testing.T) {
	code := int32(13)
	s := sondaHolding(t, &store.Call{
		Target: "auth", Protocol: config.ProtocolGRPC, Method: "Refresh",
		Path: "/auth.v1.Auth/Refresh", Status: 200,
		StartedAt: time.Now().UTC(), Duration: 5 * time.Millisecond,
		GRPCStatus: &code, GRPCMessage: "couldn't refresh the session cookie",
	})

	tree, isError := callTool(t, s, "trace_call", `{"id":1}`)
	if isError {
		t.Fatalf("trace_call failed: %s", tree)
	}
	// The node's detail, and the drawing beside it.
	if got := strings.Count(tree, "Internal: couldn't refresh the session cookie"); got != 2 {
		t.Errorf("the status message survives whole in %d of the two copies, want 2:\n%s", got, tree)
	}
}

// The verbatim copy of a capture is dropped where the decoded view replaces it,
// and a view that decoded nothing replaces nothing. A 502 page served under
// `text/event-stream` is an event stream by its content type and holds no
// events, and dropping the body left a reader with no page and no bytes.
func TestACaptureWhoseViewIsEmptyKeepsItsRawCopy(t *testing.T) {
	page := "<html><head><title>502 Bad Gateway</title></head><body><h1>502 Bad Gateway</h1><hr><p>nginx</p></body></html>"
	s := sondaHolding(t, &store.Call{
		Target: "events", Protocol: config.ProtocolHTTP, Method: "GET", Path: "/v1/stream",
		Status: 502, StartedAt: time.Now().UTC(),
		Response: store.Message{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:    []byte(page), Size: int64(len(page)),
		},
	})

	got, isError := callTool(t, s, "get_call", `{"id":1,"detail":true}`)
	if isError {
		t.Fatalf("get_call failed: %s", got)
	}
	var call struct {
		Response rawMessage `json:"response"`
		Stream   *struct {
			Events []any `json:"events"`
		} `json:"stream"`
	}
	if err := json.Unmarshal([]byte(got), &call); err != nil {
		t.Fatal(err)
	}
	// The premise: this really is a capture whose event-stream view decoded
	// nothing. Without it the assertion below proves nothing.
	if call.Stream == nil || len(call.Stream.Events) != 0 {
		t.Fatalf("this capture is not the empty-view case:\n%s", got)
	}
	if call.Response.Text != page {
		t.Errorf("the error page went with the raw copy:\n  got: %q", call.Response.Text)
	}
}

// A body of somebody else's making that happens to be shaped like a trace node.
// Recognising a node by its neighbouring field names sent this through the
// Postgres summary gate, and `the user's password was rejected` came back cut
// at the apostrophe.
func TestABodyShapedLikeATraceNodeIsUntouched(t *testing.T) {
	assertBodySurvives(t, `{"id":7,"target":"warehouse","status":"queued",`+
		`"started_at":"2026-08-06T10:00:00Z","duration_ms":42,"failed":false,`+
		`"detail":"the user's password was rejected by the identity provider"}`)
}

// The same for a body carrying the names of Sonda's own decoded views. Those
// names mean something at one position in one answer and nothing anywhere else,
// and a payload that used them was being rewritten as if it were a capture —
// which breaks the property the whole tool rests on, because for a body the
// stored bytes are the record.
// The `postgres` half is spelled out in full — a statement naming a credential,
// a description naming the column, and a row under it — because that is the
// only arrangement the correlation would actually rewrite. A shorter one comes
// back untouched whether the rule is scoped or not, and would pass over the
// broken code it exists to catch.
func TestABodyShapedLikeADecodedViewIsUntouched(t *testing.T) {
	assertBodySurvives(t, `{"stream":{"events":[{"data":"ok"}]},`+
		`"socket":{"sent":[{"kind":"text","text":"hello"}]},`+
		`"grpc":{"request":[{"index":0,"json":{"ok":true}}]},`+
		`"postgres":{"sent":[{"kind":"query","sql":"SELECT api_key FROM tokens"}],`+
		`"received":[{"kind":"row_description","columns":[{"name":"api_key"}]},`+
		`{"kind":"data_row","values":[{"text":"sk_live_KEEPME"}]}]}}`)
}

// A structural diff addresses a changed field by a path, so the field's name is
// a value and the walk over keys — which reads `path`, `a` and `b` — goes
// straight past it. diff_calls is the tool an agent reaches for when a login
// worked once and then did not, which is the request most likely to differ in
// exactly one password.
func TestADiffDoesNotShowBothSidesOfAChangedCredential(t *testing.T) {
	login := func(password string) *store.Call {
		body := `{"user":"nicolas","password":"` + password + `","device":"laptop"}`
		return &store.Call{
			Target: "auth", Protocol: config.ProtocolHTTP, Method: "POST", Path: "/v1/login",
			Status: 200, StartedAt: time.Now().UTC(),
			Request: store.Message{
				Headers: http.Header{"Content-Type": []string{"application/json"}},
				Body:    []byte(body), Size: int64(len(body)),
			},
		}
	}
	s := sondaHolding(t, login("WORKED-BEFORE"), login("FAILS-NOW"))

	got, isError := callTool(t, s, "diff_calls", `{"a":1,"b":2}`)
	if isError {
		t.Fatalf("diff_calls failed: %s", got)
	}
	// The premise: the diff really did find that field, or nothing below is
	// being tested.
	if !strings.Contains(got, `"password"`) {
		t.Fatalf("the diff did not report the password field at all:\n%s", got)
	}
	for _, secret := range []string{"WORKED-BEFORE", "FAILS-NOW"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q reached an agent through diff_calls:\n%s", secret, got)
		}
	}
	// And the diff is still worth reading: the field that changed is named, and
	// the ordinary fields beside it are not reported as changed at all.
	if strings.Contains(got, "nicolas") || strings.Contains(got, "laptop") {
		t.Errorf("unchanged fields turned up in the diff:\n%s", got)
	}
}

// assertBodySurvives stores a response body, reads the capture back through
// get_call, and checks that what comes out decodes to what went in. Compared
// after decoding because a JSON body is re-encoded on the way through, so the
// key order and the spacing are not the same bytes and never were.
func assertBodySurvives(t *testing.T, body string) {
	t.Helper()
	s := sondaHolding(t, &store.Call{
		Target: "api", Protocol: config.ProtocolHTTP, Method: "POST", Path: "/v1/jobs",
		Status: 200, StartedAt: time.Now().UTC(),
		Response: store.Message{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    []byte(body), Size: int64(len(body)),
		},
	})

	got, isError := callTool(t, s, "get_call", `{"id":1,"detail":true}`)
	if isError {
		t.Fatalf("get_call failed: %s", got)
	}
	var call struct {
		Response rawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(got), &call); err != nil {
		t.Fatal(err)
	}

	var sent, back any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(call.Response.Text), &back); err != nil {
		t.Fatalf("the body did not come back as JSON at all: %q", call.Response.Text)
	}
	if !reflect.DeepEqual(sent, back) {
		t.Errorf("a third-party body was rewritten\n  sent: %s\n  got:  %s", body, call.Response.Text)
	}
}

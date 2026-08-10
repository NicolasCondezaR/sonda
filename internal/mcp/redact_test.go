package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Sonda stores real bearer tokens and session cookies. Everything that leaves
// through MCP lands in whatever model the agent is driving, so these are the
// tests that matter most in this package: a regression here does not produce a
// wrong answer, it produces a leaked production credential.

func cleaned(t *testing.T, payload string, detail bool) string {
	t.Helper()
	v, err := cleanJSON([]byte(payload), detail)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
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
	v, err := cleanJSON([]byte(`{"headers":{"Cookie":["a","b"]}}`), false)
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
	for _, key := range []string{"target", "status", "path", "method", "duration_ms", "protocol", "content-type", "user-agent"} {
		if isSensitive(key) {
			t.Errorf("%q is redacted but carries nothing secret", key)
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
	} {
		payload, err := json.Marshal(map[string]any{"text": body})
		if err != nil {
			t.Fatal(err)
		}
		// Compared on the value, not on its encoding: Go escapes &, < and > as
		// & and friends when marshalling, so asserting against the JSON
		// text would fail on bodies this function never touched.
		v, err := cleanJSON(payload, true)
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

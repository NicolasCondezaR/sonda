package mcp

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// wait_for_call exists to answer "did the thing I just did reach the service",
// so a capture taken before the wait began is the one answer it must never
// give. The column it is filtered against holds microseconds: a bound written
// to the second is really the start of that second, and everything captured
// earlier in it comes back as though it had been waited for.
func TestWaitForCallCannotMatchTrafficFromBeforeItStarted(t *testing.T) {
	api := &fakeAPI{body: `{"calls":[{"id":9,"target":"ms-auth","path":"/v1/token"}]}`}
	s := New(api, "test")

	before := time.Now().UTC()
	text, isError := callTool(t, s, "wait_for_call", `{"service":"ms-auth","timeout_seconds":1}`)
	if isError {
		t.Fatalf("wait_for_call failed: %s", text)
	}

	_, query, ok := strings.Cut(api.last(), "?")
	if !ok {
		t.Fatalf("wait_for_call asked %q, with no query at all", api.last())
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	since, err := time.Parse(time.RFC3339Nano, values.Get("since"))
	if err != nil {
		t.Fatalf("since = %q, which is not a timestamp: %v", values.Get("since"), err)
	}
	if since.Before(before) {
		t.Errorf("wait_for_call asked for calls since %s, but only started waiting at %s: traffic captured before the wait can satisfy it",
			since.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano))
	}
}

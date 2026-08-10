package tui

import (
	"strings"
	"testing"
)

// The terminal is a client of the same API as the browser, and a capability
// only the browser can see is half a capability.

func TestTheTerminalShowsTheStatementAndItsParameters(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderPostgres("SENT", []PGMessage{
		{Kind: "startup", Parameters: map[string]string{"database": "orders", "user": "app"}},
		{Kind: "authentication_response", Note: "not decoded: this message carries a password or a SASL exchange"},
		{Kind: "parse", SQL: "SELECT name FROM users WHERE id = $1"},
		{Kind: "bind", Params: []PGValue{{Size: 3, Text: "417"}}},
	}, false))

	for _, want := range []string{"SENT", "database=orders", "SELECT name FROM users", "$1 = 417"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, out)
		}
	}
}

// A NULL and an empty string are different on the wire and different in a WHERE
// clause, and collapsing them is how an afternoon disappears.
func TestTheTerminalTellsNullFromEmpty(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderPostgres("SENT", []PGMessage{
		{Kind: "parse", SQL: "SELECT 1"},
		{Kind: "bind", Params: []PGValue{{Null: true}, {Size: 0}}},
	}, false))

	if !strings.Contains(out, "$1 = NULL") || !strings.Contains(out, "$2 = ''") {
		t.Errorf("NULL and the empty string read the same:\n%s", out)
	}
}

// A session is mostly data rows. Drawing forty of them would bury the answer,
// and dropping them silently would hide how many came back.
func TestTheTerminalCountsRowsInsteadOfDrawingThem(t *testing.T) {
	m := Model{width: 100}
	msgs := []PGMessage{{Kind: "row_description"}}
	for range 40 {
		msgs = append(msgs, PGMessage{Kind: "data_row"})
	}
	msgs = append(msgs, PGMessage{Kind: "command_complete", Tag: "SELECT 40"})

	out := rendered(m.renderPostgres("RECEIVED", msgs, false))
	if !strings.Contains(out, "40 data row(s)") {
		t.Errorf("the row count is missing:\n%s", out)
	}
	if !strings.Contains(out, "SELECT 40") {
		t.Errorf("the command tag was buried:\n%s", out)
	}
}

// The whole reason the error count reaches the listing: nothing about a
// session's transport says it failed.
func TestTheTerminalShowsAServerError(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderPostgres("RECEIVED", []PGMessage{{
		Kind: "error_response", Severity: "ERROR", Code: "42P01",
		Message: `relation "ordrs" does not exist`,
		Hint:    "Perhaps you meant orders.",
	}}, false))

	for _, want := range []string{"ERROR 42P01", "does not exist", "Perhaps you meant orders."} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering is missing %q:\n%s", want, out)
		}
	}
}

// The mechanism is worth showing; the exchange was blanked before it was
// stored, so there is nothing else to show.
func TestTheTerminalNamesTheAuthenticationMechanism(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderPostgres("RECEIVED", []PGMessage{{Kind: "authentication", Auth: "sasl"}}, false))
	if !strings.Contains(out, "AUTHENTICATION") || !strings.Contains(out, "sasl") {
		t.Errorf("the mechanism is missing:\n%s", out)
	}
}

// A gap the tool knows about is stated, never hidden.
func TestTheTerminalSaysASessionWasCutShort(t *testing.T) {
	m := Model{width: 100}
	out := rendered(m.renderPostgres("SENT", []PGMessage{{Kind: "query", SQL: "SELECT 1"}}, true))
	if !strings.Contains(out, "bytes remain") {
		t.Errorf("a truncated session rendered as a whole one:\n%s", out)
	}
}

// The listing has no bodies, so the summary is the only thing that says which
// session this was — and the outcome is the only thing that says it failed.
func TestASessionIsNamedAndJudgedInTheListing(t *testing.T) {
	call := Call{
		Protocol: "postgres", Method: "SESSION", Path: "orders",
		PostgresSummary: "SELECT id FROM orders -> SELECT 12",
	}
	if got := call.Label(); got != "SESSION orders · SELECT id FROM orders -> SELECT 12" {
		t.Errorf("label = %q", got)
	}
	if got := call.Outcome(); got != "SESSION" {
		t.Errorf("outcome = %q, want no invented status", got)
	}
	if call.Fault() {
		t.Error("a session with no server error was called a fault")
	}

	call.PostgresErrors = 1
	if !call.Fault() {
		t.Error("a SQL error is a failure and the terminal has to agree with the server")
	}
	if got := call.Outcome(); got != "SQL ERROR" {
		t.Errorf("outcome = %q", got)
	}
}

package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Sonda stores whatever crossed the wire, which in a real system means bearer
// tokens, session cookies and personal data, in a file with no encryption and
// no authentication. That is acceptable while the only thing reading it is a
// person on their own machine.
//
// An MCP server breaks that assumption: the answers leave the machine and land
// in whatever model the agent is driving. So credentials never go out through
// here, and there is deliberately no setting to change that — a flag for this
// would be left on by someone testing against a toy project and then forgotten
// when they switched to a real one. The web interface still shows everything,
// because there the reader is the owner.

const redacted = "[redacted by Sonda]"

// sensitive matches on the key, not the value. Value-sniffing sounds cleverer
// and fails in both directions: it misses an opaque session id and mangles a
// legitimate field that happens to look like base64.
var sensitive = []string{
	"authorization", "proxy-authorization", "authentication",
	"cookie", "set-cookie", "session", "sessionid", "jsessionid",
	"api-key", "apikey", "x-api-key",
	"token", "access-token", "refresh-token", "id-token", "auth-token",
	"csrf", "xsrf",
	"password", "passwd", "secret", "client-secret", "private-key",
	"credential", "credentials",
	// Matched as a substring, so X-Amz-Signature and any hmac-signature land
	// here. Its neighbour X-Amz-Credential was already covered and the
	// signature beside it was not, which is the half of a presigned URL that
	// actually authorises the request.
	"signature",
}

// sensitiveParam names parameters that are a credential in a query string and
// something else everywhere else, so they are matched there and nowhere else.
//
// `code` is the OAuth authorization code — the one credential that travels
// through the `/oauth/callback` this file exists for. It is also the SQLSTATE
// of a Postgres error, an HTTP status, a currency and a country; putting it in
// the list above would blank the most useful field of every error a capture
// holds and would make namesCredential fire on any statement touching a
// `country_code` column. `key` rides a Google API key in `?key=`, and is a
// perfectly ordinary word as a JSON field. `sig` is the short form in a
// presigned URL, and also lives inside design, assign and consignment, so it
// is matched whole rather than as a substring.
var sensitiveParam = []string{"code", "key", "sig"}

// isSensitive normalises separators so that api_key, apiKey, API-KEY and
// x-api-key all land on the same answer.
func isSensitive(key string) bool {
	k := strings.ToLower(key)
	k = strings.ReplaceAll(k, "_", "-")
	// camelCase to kebab, so accessToken matches access-token.
	var b strings.Builder
	for i, r := range key {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	camel := strings.ToLower(strings.ReplaceAll(b.String(), "_", "-"))

	for _, s := range sensitive {
		if k == s || camel == s {
			return true
		}
		// Substring, so x-company-auth-token and Set-Cookie2 are caught too.
		// Over-redacting is the safe direction: the cost is one field a person
		// can still read in the interface, and the alternative cost is a
		// production token in someone else's model.
		if strings.Contains(k, s) || strings.Contains(camel, s) {
			return true
		}
	}
	return false
}

// isSensitiveParam answers the same question for a query-string parameter,
// where a few more names are credentials than are anywhere else.
func isSensitiveParam(key string) bool {
	if isSensitive(key) {
		return true
	}
	k := strings.ToLower(strings.ReplaceAll(key, "_", "-"))
	for _, s := range sensitiveParam {
		if k == s {
			return true
		}
	}
	return false
}

// maxString bounds any single string in a reply. A four megabyte body in an
// agent's context is expensive and useless; the agent asks for detail when it
// actually needs the whole thing.
const maxString = 2000

// clean returns a payload with credentials removed and, unless detail was
// asked for, long strings shortened.
//
// The two are separate passes, in this order, and that is the whole point.
// Doing them in one walk made the default the unsafe path: a statement longer
// than maxString reached the credential gate already cut short, so `UPDATE
// users SET nickname='hunter2', bio='<2100 chars>', password='p4ssw0rd'` came
// back with its first literal in the clear at detail:false and fully blanked at
// detail:true. Redaction now sees whatever crossed the wire, whole, whatever is
// later dropped for display.
func clean(v any, detail bool) any {
	out := redact(v)
	if detail {
		return out
	}
	return shorten(out)
}

// redact walks a decoded payload and removes credentials. It allocates new
// maps and slices rather than editing in place, which is what lets shorten
// afterwards edit in place.
func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitive(k) {
				out[k] = redactedLike(val)
				continue
			}
			if text, ok := val.(string); ok && isSummaryKey(k) {
				val = redactSummary(text)
			}
			out[k] = redact(val)
		}
		// Everything above matches on keys. A Postgres capture puts the
		// sensitive name and the sensitive value in different messages, so it
		// needs the one thing a per-key walk cannot do: look at the neighbours.
		if view, ok := out["postgres"]; ok {
			redactPostgres(view)
			dropRawStream(out)
		}
		return out

	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redact(val)
		}
		return out

	case string:
		// A captured body arrives as one opaque string, so a credential inside
		// it is invisible to the walk above — the field is "text", not
		// "password". Looking inside anything that is itself JSON is what
		// closes that hole, and it costs a parse attempt only for strings that
		// already look the part.
		//
		// Found by sending a real request through Sonda and reading it back:
		// the headers were clean and the password in the body was not.
		if inner, ok := insideJSON(t); ok {
			if out, err := json.Marshal(redact(inner)); err == nil {
				return string(out)
			}
		}
		return redactQuery(t)

	default:
		return v
	}
}

// shorten cuts long strings down for display. It runs after redaction and only
// when detail was not asked for, so nothing it drops was ever the difference
// between a credential leaving and staying.
func shorten(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = shorten(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = shorten(val)
		}
		return t
	case string:
		return truncate(t)
	default:
		return v
	}
}

// insideJSON reports whether a string is itself a JSON object or array, and
// returns it decoded. The prefix check is not an optimisation for its own
// sake: without it every ordinary body would go through a failed parse, and a
// bare number or quoted word would come back re-encoded for no reason.
func insideJSON(s string) (any, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 2 {
		return nil, false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return nil, false
	}
	return v, true
}

func truncate(s string) string {
	if len(s) <= maxString {
		return s
	}
	return s[:maxString] + fmt.Sprintf("… [%d more characters, ask for detail]", len(s)-maxString)
}

// redactedLike keeps the shape of what it replaces. Headers arrive as
// {"Authorization": ["Bearer …"]}, and turning a list into a string is the
// kind of surprise that makes a client's parser fail on the one field it was
// never going to read anyway.
func redactedLike(v any) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = redacted
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k := range t {
			out[k] = redacted
		}
		return out
	default:
		return redacted
	}
}

// redactQuery blanks the sensitive parameters of a query string and leaves
// everything else exactly as it was.
//
// A captured path is stored as the full request URI, so `GET
// /oauth/callback?access_token=ya29…` reaches an agent whole: the key is
// "path", which is not sensitive, and the credential is in the value. It is the
// most likely leak in ordinary use — OAuth callbacks, signed URLs, `?api_key=`
// — and it travels in the *summary*, so it arrives on the first tool call
// without anyone asking for detail.
//
// Blanking the whole path instead would be safe and useless: the path is how a
// person recognises which call they are looking at.
//
// This runs on every string rather than on a list of URL-bearing field names.
// A second list would have to be kept in step with every place a URL is stored
// — `Location` and `Referer` headers, diff and trace paths, a link inside a
// body — and the one that drifts is the one that leaks. A string with no `?` is
// returned byte for byte, so a body that is not a URL cannot be altered.
//
// Every `?` is examined, not the first. A body or a log line holds more than
// one URL, and stopping at the first meant the second was never looked at.
func redactQuery(s string) string {
	var b strings.Builder
	changed := false
	rest := 0
	for {
		mark := strings.IndexByte(s[rest:], '?')
		if mark < 0 {
			break
		}
		mark += rest
		b.WriteString(s[rest : mark+1])
		end := mark + 1 + queryEnd(s[mark+1:])
		if redactPairs(&b, s[mark+1:end]) {
			changed = true
		}
		rest = end
	}
	if !changed {
		return s
	}
	b.WriteString(s[rest:])
	return b.String()
}

// queryEnd returns how much of s can still be one query string. A URL holds no
// raw whitespace and no quote, and a fragment is not part of the query — so the
// first of any of those ends it. Without the bound, the first parameter after
// the first `?` swallows the rest of the string: `see https://a/x?ok=1 and
// https://b/y?api_key=SECRET` was one parameter named `ok` and came back whole.
func queryEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '#', '"', '\'', '<', '>', '\\':
			return i
		}
	}
	return len(s)
}

// redactPairs writes one query string, blanking the value of every sensitive
// parameter, and reports whether it changed anything.
//
// It splits on `;` as well as `&`. The semicolon separator is deprecated and
// still parsed by real servers, and reading only `&` meant
// `?debug=1;api_key=SECRET` was a single parameter named `debug` and left
// verbatim. Separators are written back as they arrived, and so is the key, so
// a repeated parameter still reads as two occurrences.
func redactPairs(b *strings.Builder, query string) bool {
	changed := false
	start := 0
	for i := 0; i <= len(query); i++ {
		if i < len(query) && query[i] != '&' && query[i] != ';' {
			continue
		}
		pair := query[start:i]
		// A parameter with no value carries nothing to leak, and its name is
		// worth keeping: `?debug` says something about the call.
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			key := pair[:eq]
			// `?%61pi_key=` is the same parameter as `?api_key=`, and a sender
			// that wanted to hide from a filter would spell it the first way.
			if decoded, err := url.QueryUnescape(key); err == nil {
				key = decoded
			}
			if isSensitiveParam(key) {
				pair = pair[:eq+1] + redacted
				changed = true
			}
		}
		b.WriteString(pair)
		if i < len(query) {
			b.WriteByte(query[i])
		}
		start = i + 1
	}
	return changed
}

// isSummaryKey names the fields that carry a Postgres one-line reading. Both
// are Sonda's own: `postgres_summary` on every listed call, and `detail` on a
// node of a trace, which is the same string under another name.
func isSummaryKey(key string) bool {
	switch strings.ToLower(key) {
	case "postgres_summary", "detail":
		return true
	}
	return false
}

// redactSummary gates that one-line reading. It is the field that reaches an
// agent from recent_failures and search_calls before it has asked for
// anything, and it is a plain string — the walk over keys sees a value under
// "postgres_summary" and nothing that says the value is SQL. So
// `INSERT INTO users (email, password) VALUES ('a@b.c','hunter2')` arrived
// complete, on the first tool call, with no detail flag.
//
// pgwire.Summarise produces one of two shapes and they need opposite
// treatment. A statement leaks through its literals, and its double-quoted runs
// are identifiers worth keeping — an ORM writes every one of them. A server
// error leaks through the message, where Postgres puts the offending value in
// double quotes (`invalid input syntax for type uuid: "hunter2"`) and in forms
// with no quoting at all (`Key (password)=(hunter2)`). There is no structure
// worth mining in prose, so the message goes whole and the SQLSTATE — the part
// that is actionable — stays.
func redactSummary(s string) string {
	if !namesCredential(s) {
		return s
	}
	if head, _, ok := strings.Cut(s, ": "); ok && isErrorHead(head) {
		return head + ": " + redacted
	}
	return blankSQLLiterals(s)
}

// isErrorHead recognises what pgwire puts before a server message: "error",
// optionally followed by the SQLSTATE. Reading another package's format is
// coupling, and it is pinned by a test that builds the summary through
// pgwire.Summarise itself, so a change there fails loudly rather than leaking.
func isErrorHead(head string) bool {
	fields := strings.Fields(head)
	return len(fields) > 0 && len(fields) <= 2 && fields[0] == "error"
}

// redactPostgres closes the hole that key matching cannot reach in a Postgres
// capture: the protocol is column oriented, so the sensitive *name* and the
// sensitive *value* arrive in different messages.
//
// A RowDescription names the columns and the DataRows that follow carry the
// values, aligned by position — `SELECT api_key FROM tokens` comes back as
// `columns:[{name:"api_key"}]` and `values:[{text:"sk_live_…"}]`, and nothing
// in a per-key walk ever sees the two together. Same for a Bind: its parameters
// are positions, and the statement that gives them meaning was a Parse earlier.
//
// It takes the `postgres` view rather than any list it is handed. The earlier
// version ran on every `[]any` and rewrote whatever happened to carry the same
// key names: a captured body holding `{"params":[{"text":"totally
// unrelated"}]}` had that text blanked because a different object earlier in
// the same array carried an `sql` naming a credential. Rewriting a payload
// nobody can check against the web interface breaks the property the whole tool
// rests on — the stored bytes are the record — so the correlation now happens
// only where the protocol actually is, and only across messages that carry a
// pgwire `kind`.
func redactPostgres(v any) {
	view, ok := v.(map[string]any)
	if !ok {
		return
	}
	sent, sentOK := view["sent"].([]any)
	received, receivedOK := view["received"].([]any)
	if !sentOK && !receivedOK {
		return
	}

	// The client's half first: which statements touch a credential, and their
	// literals. Keyed by prepared statement name, which is "" for the unnamed
	// statement every driver uses for a one-shot query — so the common case
	// correlates like any other.
	credentialStatement := map[string]bool{}
	tainted := false
	for _, item := range sent {
		m, ok := pgMessage(item)
		if !ok {
			continue
		}
		statement, _ := m["statement"].(string)
		if sql, ok := m["sql"].(string); ok && namesCredential(sql) {
			m["sql"] = blankSQLLiterals(sql)
			credentialStatement[statement] = true
			tainted = true
		}
		if params, ok := m["params"].([]any); ok && credentialStatement[statement] {
			// A bind parameter is a position with no name, so there is no way
			// to tell which of them is the password. When the statement they
			// belong to touches a credential, all of them go.
			for _, p := range params {
				blankText(p)
			}
		}
	}

	// The server's half: the rows, aligned against the description before them.
	var names []string
	described, blankWhole := false, false
	for _, item := range received {
		m, ok := pgMessage(item)
		if !ok {
			continue
		}
		switch m["kind"] {
		case "row_description":
			names = names[:0]
			named := false
			for _, c := range asList(m["columns"]) {
				col, _ := c.(map[string]any)
				name, _ := col["name"].(string)
				names = append(names, name)
				named = named || isSensitive(name)
			}
			described = true
			// `SELECT api_key AS k FROM tokens` describes a column called "k",
			// and the key comes back in the clear: the alias defeats alignment
			// entirely. When the statement names a credential and not one
			// described column does, alignment cannot say where the secret went
			// — so the row goes whole, the same blunt rule bind parameters
			// already follow. It costs an id and an email on a login lookup,
			// which the web interface still shows.
			blankWhole = tainted && !named
		case "data_row":
			for i, value := range asList(m["values"]) {
				// A row longer than its description cannot be aligned past the
				// last column. The tail is blanked rather than skipped, because
				// "no name to check" is not the same as "safe".
				unaligned := described && i >= len(names)
				if blankWhole || unaligned || (i < len(names) && isSensitive(names[i])) {
					blankText(value)
				}
			}
		case "error_response", "notice_response":
			// A server error echoes the value it choked on — `password
			// authentication failed for user "nico"`, `invalid input syntax for
			// type uuid: "hunter2"` — and none of these fields is named
			// anything a key match would catch.
			for _, field := range []string{"message", "detail", "hint"} {
				text, ok := m[field].(string)
				if !ok || !(tainted || namesCredential(text)) {
					continue
				}
				m[field] = blankQuoted(blankSQLLiterals(text))
			}
		}
	}
}

// dropRawStream removes the second copy of a Postgres conversation.
//
// The same bytes are served twice: decoded and redacted under `postgres`, and
// verbatim under request.text — or request.base64, when a binary value made the
// stream invalid UTF-8. There the statement, its bind parameters and every row
// value are perfectly legible and everything above counts for nothing. They
// cannot be blanked selectively either: a Postgres value is a length followed
// by a run of bytes at an arbitrary offset, with no quoting to scan for.
//
// Nothing readable is lost that the decoded view does not already carry, the
// sizes stay, and the web interface still shows the stream — because there the
// reader is the owner.
func dropRawStream(out map[string]any) {
	for _, side := range []string{"request", "response"} {
		part, ok := out[side].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "base64"} {
			if _, has := part[field]; has {
				part[field] = redacted
			}
		}
	}
}

// pgMessage accepts an object only if it carries the `kind` every pgwire
// message has. It is what keeps the correlation above from touching an
// ordinary payload that happens to hold a `values` or a `params` key.
func pgMessage(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	if _, ok := m["kind"].(string); !ok {
		return nil, false
	}
	return m, true
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

// blankText blanks the decoded text of one Postgres value. A binary value has
// no text field — its bytes are never serialised — so there is nothing to do
// and nothing is invented. The size stays, because a reader still needs to see
// that a value was there.
func blankText(v any) {
	value, ok := v.(map[string]any)
	if !ok {
		return
	}
	if _, has := value["text"]; has {
		value["text"] = redacted
	}
}

// namesCredential reports whether a statement mentions a credential-like
// identifier — a `password` column, a `tokens` table.
//
// This is the trigger for the blunt things above, and it is deliberately the
// same word list the rest of the file uses. It answers "could this statement be
// carrying a secret", never "where in it".
func namesCredential(sql string) bool {
	for _, word := range strings.FieldsFunc(sql, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) {
		if isSensitive(word) {
			return true
		}
	}
	return false
}

// blankSQLLiterals replaces the contents of every string literal in a
// statement, keeping the statement itself readable.
//
// It only runs on a statement namesCredential already flagged. Doing it to
// every statement would mangle the thing the capture was opened for, and doing
// it to none returns `INSERT INTO users (email, password) VALUES ('a',
// 'hunter2')` verbatim. Blanking the literals and nothing else keeps the shape
// — the tables, the columns, the operators — which is what an agent reads a
// statement for, and drops the part that is a credential.
//
// It cannot tell which literal is the secret, so it takes them all. Within a
// statement that names a credential that is the right direction: the cost is a
// literal a person can still read in the web interface.
func blankSQLLiterals(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); {
		switch sql[i] {
		case '\'':
			b.WriteString("'" + redacted + "'")
			i = endOfQuoted(sql, i)
		case '$':
			if tag, end, ok := dollarQuoted(sql, i); ok {
				b.WriteString(tag + redacted + tag)
				i = end
				continue
			}
			b.WriteByte(sql[i])
			i++
		default:
			b.WriteByte(sql[i])
			i++
		}
	}
	return b.String()
}

// blankQuoted blanks every double-quoted run. This is deliberately not part of
// blankSQLLiterals: in a statement a double-quoted run is an identifier, and
// every ORM quotes all of them, so blanking those would leave `SELECT
// "[redacted]"."[redacted]" FROM "[redacted]"`. In a server message the same
// punctuation is where the offending value went.
func blankQuoted(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '"' {
			b.WriteByte(text[i])
			i++
			continue
		}
		b.WriteString(`"` + redacted + `"`)
		// An unterminated run blanks the rest, which is the safe way to be
		// wrong about where a value ended.
		end := strings.IndexByte(text[i+1:], '"')
		if end < 0 {
			break
		}
		i += end + 2
	}
	return b.String()
}

// endOfQuoted returns the index one past the closing quote of the literal that
// opens at start. A doubled quote is an escaped one and does not end it.
//
// An unterminated literal blanks the rest of the statement. That is what a
// backslash escape inside an E” string produces, and running long is the safe
// way to be wrong here.
func endOfQuoted(sql string, start int) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != '\'' {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == '\'' {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}

// dollarQuoted recognises Postgres' $tag$…$tag$ form, which needs no escaping
// and would otherwise carry a password straight past the quote scanner.
//
// A tag never starts with a digit, which is what keeps `$1` — a bind
// placeholder, and far more common than dollar quoting — from being read as the
// opening of a literal.
func dollarQuoted(sql string, start int) (tag string, end int, ok bool) {
	i := start + 1
	for i < len(sql) && (sql[i] == '_' || sql[i] >= 'a' && sql[i] <= 'z' || sql[i] >= 'A' && sql[i] <= 'Z' || (i > start+1 && sql[i] >= '0' && sql[i] <= '9')) {
		i++
	}
	if i >= len(sql) || sql[i] != '$' {
		return "", 0, false
	}
	tag = sql[start : i+1]
	closing := strings.Index(sql[i+1:], tag)
	if closing < 0 {
		// Unterminated, so the body runs to the end of the statement.
		return tag, len(sql), true
	}
	return tag, i + 1 + closing + len(tag), true
}

// cleanJSON is the single door every payload leaves through. Tools call this
// rather than redacting for themselves, so a tool added later cannot forget.
func cleanJSON(payload []byte, detail bool) (any, error) {
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, fmt.Errorf("Sonda answered something that is not JSON: %w", err)
	}
	return clean(v, detail), nil
}

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
}

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

// maxString bounds any single string in a reply. A four megabyte body in an
// agent's context is expensive and useless; the agent asks for detail when it
// actually needs the whole thing.
const maxString = 2000

// clean walks a decoded payload and returns it with credentials removed and,
// unless detail was asked for, long strings shortened.
func clean(v any, detail bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitive(k) {
				out[k] = redactedLike(val)
				continue
			}
			out[k] = clean(val, detail)
		}
		return out

	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = clean(val, detail)
		}
		// Everything above matches on keys. A Postgres capture puts the
		// sensitive name and the sensitive value in different messages of this
		// same list, so it needs the one thing a per-key walk cannot do: look
		// at the neighbours.
		alignPostgres(out)
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
			redacted := clean(inner, detail)
			if out, err := json.Marshal(redacted); err == nil {
				return truncate(string(out), detail)
			}
		}
		return truncate(redactQuery(t), detail)

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

func truncate(s string, detail bool) string {
	if detail || len(s) <= maxString {
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
func redactQuery(s string) string {
	mark := strings.IndexByte(s, '?')
	if mark < 0 {
		return s
	}
	query := s[mark+1:]

	// A fragment is not part of the query, and letting it ride along on the
	// last parameter would put `#section` inside the value being examined.
	fragment := ""
	if hash := strings.IndexByte(query, '#'); hash >= 0 {
		query, fragment = query[:hash], query[hash:]
	}

	pairs := strings.Split(query, "&")
	changed := false
	for i, pair := range pairs {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			// A parameter with no value carries nothing to leak, and its name
			// is worth keeping: `?debug` says something about the call.
			continue
		}
		key := pair[:eq]
		// `?%61pi_key=` is the same parameter as `?api_key=`, and a sender that
		// wanted to hide from a filter would spell it the first way.
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if !isSensitive(key) {
			continue
		}
		// The key is kept as it arrived, so a repeated parameter still reads as
		// two occurrences rather than collapsing into one.
		pairs[i] = pair[:eq+1] + redacted
		changed = true
	}
	if !changed {
		return s
	}
	return s[:mark+1] + strings.Join(pairs, "&") + fragment
}

// alignPostgres closes the hole that key matching cannot reach in a Postgres
// capture: the protocol is column oriented, so the sensitive *name* and the
// sensitive *value* arrive in different places.
//
// A RowDescription names the columns and the DataRows that follow carry the
// values, aligned by position — `SELECT api_key FROM tokens` comes back as
// `columns:[{name:"api_key"}]` and `values:[{text:"sk_live_…"}]`, and nothing
// in a per-key walk ever sees the two together. Same for a Bind: its parameters
// are positions, and the statement that gives them meaning was a Parse earlier
// in the list.
//
// It walks any list, not only a Postgres one. Guarding on protocol would mean
// this function has to be told where the messages are, which is the kind of
// coupling that survives exactly until someone adds a tool that returns them
// somewhere else.
func alignPostgres(list []any) {
	var names []string
	// Keyed by prepared statement name, which is "" for the unnamed statement
	// every driver uses for a one-shot query — so the common case correlates
	// like any other.
	credentialStatement := map[string]bool{}

	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}

		if cols, ok := m["columns"].([]any); ok {
			names = names[:0]
			for _, c := range cols {
				col, _ := c.(map[string]any)
				name, _ := col["name"].(string)
				names = append(names, name)
			}
		}
		if values, ok := m["values"].([]any); ok {
			for i, v := range values {
				if i < len(names) && isSensitive(names[i]) {
					blankText(v)
				}
			}
		}

		statement, _ := m["statement"].(string)
		if sql, ok := m["sql"].(string); ok && namesCredential(sql) {
			m["sql"] = blankSQLLiterals(sql)
			credentialStatement[statement] = true
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
// This is the trigger for the two blunt things below, and it is deliberately
// the same word list the rest of the file uses. It answers "could this
// statement be carrying a secret", never "where in it".
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

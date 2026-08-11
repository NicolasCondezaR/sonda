package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/NicolasCondezaR/sonda/internal/trace"
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

// Redaction is decided by *position* in Sonda's own answer, never by
// pattern-matching a key name or a string shape inside arbitrary content.
//
// The distinction is the whole design. A field Sonda builds — the path, a
// Postgres summary line, a trace node's detail, the drawn tree — is something
// Sonda already knows the meaning of, so it gets a rule written for exactly
// that field and nothing else reaches it. Anything that came off the wire —
// a body, a frame payload, a decoded message — is somebody else's data of
// unknown shape, and there the key-based walk below is the right and only tool.
//
// Four rounds of fixes went the other way, inferring meaning from key names and
// string shapes over captured traffic, and each round broke a payload the
// previous one had not thought of: a `columns` key in a non-Postgres body, a
// `detail` field read as SQL, a tree line miscounted because a branch character
// is a word. Every one of those was the same mistake, so what changed is not
// the rules but how a rule is chosen.

// at is one position in Sonda's own answer: what to do here, and where the
// children are. A key with no entry is captured content — the walk falls
// through to redactCaptured and nothing positional can fire on it, which is
// what makes a third-party body carrying `postgres` or `detail` untouchable.
type at struct {
	// rule replaces everything else at this position.
	rule func(any) any

	// field and item are the children of a map and of a list.
	field map[string]*at
	item  *at

	// after runs once this map is finished, for a rule that needs siblings. It
	// gets the original alongside the redacted copy, because a rule that has to
	// find text elsewhere in the answer needs to know what to look for.
	after func(before, after map[string]any)
}

// positionFor maps the API endpoint that produced a payload to the root of
// Sonda's schema for it. This is the only place position comes from: the tools
// call the API by path, so the path is known before the bytes are.
//
// An endpoint with no entry is treated as captured content from top to bottom,
// which is the safe direction — the key walk still runs, and no positional rule
// can fire somewhere it was not meant to.
func positionFor(endpoint string) *at {
	path, _, _ := strings.Cut(endpoint, "?")
	switch {
	case path == "/api/calls":
		return &at{field: map[string]*at{"calls": {item: summaryPosition()}}}
	case strings.HasPrefix(path, "/api/calls/") && strings.HasSuffix(path, "/replay"):
		// The replay report carries none of the capture, only where it was sent
		// and how it went.
		return nil
	case strings.HasPrefix(path, "/api/calls/"):
		return callPosition()
	case path == "/api/trace":
		return &at{rule: cleanTrace}
	case path == "/api/diff":
		return diffPosition()
	default:
		return nil
	}
}

// summaryPosition is one row of a listing. Everything in it is a scalar Sonda
// wrote except the one-line Postgres summary, which is a statement.
func summaryPosition() *at {
	return &at{field: map[string]*at{"postgres_summary": {rule: ruleSummary}}}
}

// callPosition is one capture in full: a summary, both raw messages, and
// whichever decoded views the protocol produced.
func callPosition() *at {
	p := summaryPosition()
	p.field["postgres"] = &at{rule: rulePostgresView}
	p.after = dropRawStreams
	return p
}

// diffPosition is a structural comparison of two captures. Its changes are the
// one place where a field's *name* travels as a value.
func diffPosition() *at {
	change := &at{after: blankSensitiveChange}
	side := &at{field: map[string]*at{
		"changes": {item: change},
		// A gRPC diff reports per message rather than once for the body.
		"messages": {item: &at{field: map[string]*at{"changes": {item: change}}}},
	}}
	return &at{field: map[string]*at{
		"metadata": {item: change},
		"request":  side,
		"response": side,
	}}
}

// blankSensitiveChange blanks the two sides of a changed field whose name is a
// credential.
//
// calldiff addresses a field by a path — `{"path":"user.password","kind":
// "changed","a":"hunter2","b":"hunter3"}` — so here the name is a value and the
// keys are `path`, `a` and `b`. A walk that reads keys goes straight past it,
// and diff_calls is exactly the tool an agent reaches for when a login worked
// once and then did not.
func blankSensitiveChange(_, out map[string]any) {
	path, ok := out["path"].(string)
	if !ok || !namesSensitiveField(path) {
		return
	}
	for _, side := range []string{"a", "b"} {
		if value, has := out[side]; has {
			out[side] = redactedLike(value)
		}
	}
}

// namesSensitiveField reports whether any segment of a calldiff path is a
// credential — any, not just the last, because a change under
// `credentials.value` is one too and over-redacting a field is the cheaper
// mistake.
func namesSensitiveField(path string) bool {
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '.' || r == '[' || r == ']'
	}) {
		if isSensitive(segment) {
			return true
		}
	}
	return false
}

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
func clean(v any, p *at, detail bool) any {
	out := walk(v, p)
	if detail {
		return out
	}
	return shorten(out)
}

// walk descends Sonda's schema and hands everything outside it to the key-based
// walk. It allocates new maps and slices rather than editing in place, which is
// what lets shorten afterwards edit in place.
func walk(v any, p *at) any {
	if p == nil {
		return redactCaptured(v)
	}
	if p.rule != nil {
		return p.rule(v)
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			// A credential-shaped name wins over the schema. No position below
			// carries one, and if one ever does, blanking it is the answer.
			if isSensitive(k) {
				out[k] = redactedLike(val)
				continue
			}
			out[k] = walk(val, p.field[k])
		}
		if p.after != nil {
			p.after(t, out)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = walk(val, p.item)
		}
		return out
	default:
		return redactCaptured(v)
	}
}

// redactCaptured walks content Sonda knows nothing about and removes what is
// named like a credential. Matching on the key is all there is to go on here,
// and that is deliberate: value-sniffing misses an opaque session id and
// mangles a legitimate field that happens to look like base64.
//
// Nothing in here reads a string as SQL or looks at the shape of a key's
// neighbours. Those questions have answers only where Sonda built the field.
func redactCaptured(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitive(k) {
				out[k] = redactedLike(val)
				continue
			}
			out[k] = redactCaptured(val)
		}
		return out

	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactCaptured(val)
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
			if out, err := json.Marshal(redactCaptured(inner)); err == nil {
				return string(out)
			}
		}
		return redactQuery(t)

	default:
		return v
	}
}

// ruleSummary gates the one-line Postgres reading of a capture. It fires only
// where pgwire.Summarise wrote it, so it is never handed prose.
func ruleSummary(v any) any {
	if line, ok := v.(string); ok {
		return redactSummary(line)
	}
	return redactCaptured(v)
}

// rulePostgresView runs the ordinary key walk over a decoded Postgres session
// and then the cross-message correlation that only makes sense here.
func rulePostgresView(v any) any {
	out := redactCaptured(v)
	redactPostgres(out)
	return out
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

// cleanTrace redacts the answer of /api/trace, which carries the same readings
// twice: once as structure, and once drawn as a block of text.
//
// The drawing is not scanned. Scanning it is what left the third node of a tree
// leaking — a rule that recognised a summary by counting words saw the branch
// character `├─` as one of them, so it fired on the root and on the last child
// and nowhere else. Instead each node reports what its own line became, and
// those exact strings are substituted into the drawing. There is nothing left
// to recognise, so there is nothing left to recognise wrongly.
func cleanTrace(v any) any {
	in, ok := v.(map[string]any)
	if !ok {
		return redactCaptured(v)
	}

	var edits []edit
	root := &at{field: map[string]*at{
		"trace": tracePosition(&edits),
		// Left exactly as it arrived by the walk, and rewritten below from the
		// nodes. Passing it through the ordinary string handling first would
		// change the text the substitutions have to match.
		"rendered": {rule: func(v any) any { return v }},
	}}
	out, _ := walk(in, root).(map[string]any)
	if out == nil {
		return redactCaptured(v)
	}
	if drawn, ok := in["rendered"].(string); ok {
		out["rendered"] = applyEdits(drawn, edits)
	}
	return out
}

// tracePosition is the tree itself. Every node is the same position, at every
// depth, which is the property the old line scanner did not have.
func tracePosition(edits *[]edit) *at {
	node := &at{}
	node.field = map[string]*at{
		"call": {after: func(before, after map[string]any) {
			*edits = append(*edits, redactNode(before, after)...)
		}},
		"children": {item: node},
	}
	return &at{field: map[string]*at{"root": node}}
}

// redactNode gates the one line a trace node carries, and reports what changed
// so the drawing can be kept in step.
//
// `detail` is a Postgres summary in one of the four cases toTraceCall produces
// and prose in the other three, and the node says which. Gating it by its name
// instead is what sent every `{"detail": …}` — FastAPI's error shape, and RFC
// 7807's — through a SQL scanner, where an apostrophe opens a literal that
// never closes and `the user's password was rejected` came back cut in half.
// Prose is now left alone, including a gRPC message that happens to say the
// word "cookie".
func redactNode(before, after map[string]any) []edit {
	var out []edit
	// The path is a captured request URI and the walk has already blanked its
	// sensitive parameters; the drawing holds the same URI and would keep them.
	out = append(out, changed(before, after, "path")...)

	if before["detail_kind"] == trace.DetailPostgres {
		if line, ok := after["detail"].(string); ok {
			after["detail"] = redactSummary(line)
		}
	}
	return append(out, changed(before, after, "detail")...)
}

// edit is one exact substitution to make in the drawn tree.
type edit struct{ from, to string }

func changed(before, after map[string]any, key string) []edit {
	was, ok := before[key].(string)
	now, alsoOK := after[key].(string)
	if !ok || !alsoOK || was == "" || was == now {
		return nil
	}
	return []edit{{from: was, to: now}}
}

// applyEdits substitutes the redacted lines back into the drawing, longest
// first so that a short line cannot corrupt a longer one that contains it.
func applyEdits(drawn string, edits []edit) string {
	sort.Slice(edits, func(i, j int) bool { return len(edits[i].from) > len(edits[j].from) })
	for _, e := range edits {
		drawn = strings.ReplaceAll(drawn, e.from, e.to)
	}
	return drawn
}

// redactSummary gates the one-line reading of a Postgres capture. It is the
// field that reaches an agent from recent_failures and search_calls before it
// has asked for anything, so `INSERT INTO users (email, password) VALUES
// ('a@b.c','hunter2')` arrived complete, on the first tool call, with no detail
// flag.
//
// It is only ever handed a string pgwire.Summarise wrote — the
// `postgres_summary` of a listing, or the detail of a trace node that said it
// was a Postgres one. That guarantee is what the shapes below rest on, and it
// is a property of where this is called from, not of anything it can check.
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
//
// The word count is only sound because the input is a summary and nothing else.
// It used to be run over the lines of a drawn tree as well, where `├─` counts as
// a word and every node but the root and the last child failed the test — and
// the branch that was supposed to blank a message never ran.
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
// It runs at one position — the `postgres` view of one capture — and is
// unreachable from anywhere else. An earlier version ran on every `[]any` and
// rewrote whatever happened to carry the same key names: a captured body
// holding `{"params":[{"text":"totally unrelated"}]}` had that text blanked
// because a different object earlier in the same array carried an `sql` naming
// a credential. Rewriting a payload nobody can check against the web interface
// breaks the property the whole tool rests on — for a body, the stored bytes
// are the record.
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

// decodedViews pairs each decoded view of a capture with the raw copy it
// stands in for. Read as: the events of `stream` are the response, so the
// response's verbatim bytes are the second copy of them.
var decodedViews = []struct{ view, messages, side string }{
	{"postgres", "sent", "request"},
	{"postgres", "received", "response"},
	{"socket", "sent", "request"},
	{"socket", "received", "response"},
	{"stream", "events", "response"},
	{"grpc", "request", "request"},
	{"grpc", "response", "response"},
}

// dropRawStreams removes the second copy of a capture.
//
// Four protocols are served twice: decoded and redacted under their own view,
// and verbatim under request.text — or request.base64, when the bytes are not
// valid UTF-8. In the second copy the statement, the frame payload, the event
// and the protobuf field are all perfectly legible, and everything the walk
// above did counts for nothing. None of them can be blanked selectively
// either: a Postgres value, a WebSocket frame and a gRPC message are each a
// length followed by a run of bytes at an arbitrary offset, with no quoting to
// scan for.
//
// So the raw copy goes only where the decoded view beside it genuinely replaces
// it, side by side, and a view that decoded nothing replaces nothing. A 502
// HTML page served as `text/event-stream` is still an event stream by its
// content type, and its decoded view is empty: dropping the body there left a
// reader with no error page and no bytes, which is worse than either. The same
// holds for a socket whose bytes never framed and for a gRPC side whose frames
// were compressed.
//
// This runs at one position — a whole capture — and nowhere else, so a body
// that happens to carry a `stream` or `socket` key is not a capture and is left
// alone. The sizes always stay, and the web interface still shows the stream,
// because there the reader is the owner.
func dropRawStreams(_, out map[string]any) {
	for _, v := range decodedViews {
		view, ok := out[v.view].(map[string]any)
		if !ok {
			continue
		}
		messages, _ := view[v.messages].([]any)
		if len(messages) == 0 || !allDecoded(v.view, messages) {
			continue
		}
		dropRawStream(out, v.side)
	}
}

// allDecoded reports whether every message of a side came back readable.
//
// Only gRPC can fail this. A compressed frame is not decoded — the encoding is
// negotiated in a header Sonda does not hold — and the verbatim bytes are then
// the only record there is. Postgres messages, WebSocket frames and events are
// decoded or they are not in the list at all, so a non-empty list is proof
// enough for those.
func allDecoded(view string, messages []any) bool {
	if view != "grpc" {
		return true
	}
	for _, m := range messages {
		message, _ := m.(map[string]any)
		if message["json"] == nil && message["fields"] == nil {
			return false
		}
	}
	return true
}

// dropRawStream replaces the verbatim bytes of one side.
func dropRawStream(out map[string]any, side string) {
	part, ok := out[side].(map[string]any)
	if !ok {
		return
	}
	for _, field := range []string{"text", "base64"} {
		if _, has := part[field]; has {
			part[field] = redacted
		}
	}
}

// pgMessage accepts an object only if it carries the `kind` every pgwire
// message has. Position already guarantees these are pgwire messages; this
// keeps the correlation from acting on a truncated or garbled one, where the
// alignment it depends on would be meaningless.
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

// cleanAnswer is the single door every payload leaves through. Tools reach it
// through get and post rather than redacting for themselves, so a tool added
// later cannot forget — and because those two know which endpoint they called,
// the answer's position is known before its bytes are parsed.
func cleanAnswer(endpoint string, payload []byte, detail bool) (any, error) {
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, fmt.Errorf("Sonda answered something that is not JSON: %w", err)
	}
	return clean(v, positionFor(endpoint), detail), nil
}

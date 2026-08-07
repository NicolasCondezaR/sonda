package mcp

import (
	"encoding/json"
	"fmt"
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
		return truncate(t, detail)

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

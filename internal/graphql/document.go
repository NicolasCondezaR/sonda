package graphql

import "strings"

// The document scan.
//
// A GraphQL document is only read far enough to name the operation: its
// keyword, its name and the fields directly under its selection set. Everything
// else — arguments, nested selections, variable definitions, directives,
// fragment bodies — is skipped over as balanced text.
//
// This is deliberately not a parser. It never rejects a document, because
// Sonda's job is to show what crossed the wire, not to judge it: a document the
// server would refuse still has to appear in the field with whatever could be
// read off it.

// parseDocument names the operation a document carries.
//
// wanted is the request's operationName. A document may hold several
// operations, and the client says which one it is running; without a name the
// first operation is the one, which is what a server does too.
func parseDocument(doc, wanted string) (kind, name string, fields []string) {
	s := &scanner{src: doc}

	for {
		tok := s.next()
		switch {
		case tok == "":
			return "", "", nil

		case tok == "{":
			// The shorthand document: no keyword, no name, just a selection
			// set. It is always a query.
			f := selectionFields(s)
			if wanted == "" {
				return "query", "", f
			}

		case tok == "query" || tok == "mutation" || tok == "subscription":
			opName, f, ok := operation(s)
			if ok && (wanted == "" || wanted == opName) {
				return tok, opName, f
			}

		case tok == "fragment":
			// A fragment definition is not an operation, and its selection set
			// would otherwise be read as one.
			skipToBlock(s)

		default:
			// A schema definition, an unknown keyword, or text this scan does
			// not model. Stepping over one token at a time recovers at the next
			// operation instead of giving up on the document.
		}
	}
}

// operation reads what follows an operation keyword: an optional name, the
// variable definitions and directives, then the selection set.
func operation(s *scanner) (name string, fields []string, ok bool) {
	tok := s.next()
	if isName(tok) {
		name, tok = tok, s.next()
	}
	for tok != "{" {
		switch tok {
		case "":
			return name, nil, false
		case "(":
			skipBalanced(s, "(", ")")
		}
		tok = s.next()
	}
	return name, selectionFields(s), true
}

// selectionFields reads the field names directly inside a selection set whose
// opening brace has already been consumed.
func selectionFields(s *scanner) []string {
	var fields []string
	for {
		tok := s.next()
		switch {
		case tok == "" || tok == "}":
			return fields

		case tok == "{":
			// A nested selection set belongs to the field just read.
			skipBalanced(s, "{", "}")

		case tok == "(":
			skipBalanced(s, "(", ")")

		case tok == "@":
			s.next() // the directive name; its arguments are the "(" case

		case tok == "...":
			// A fragment spread contributes fields this scan cannot resolve
			// without the fragment definition, and an inline fragment's own
			// selection set is skipped as a block. Both are stepped over: a
			// guessed field list would read as a measurement.
			mark := s.pos
			if s.next() == "on" {
				s.next() // the type condition
			} else {
				s.pos = mark
				s.next() // the fragment name
			}

		case isName(tok):
			// `alias: field` — the wire name is the one after the colon, and
			// reporting the alias would name something the server never saw.
			mark := s.pos
			if s.next() == ":" {
				if actual := s.next(); isName(actual) {
					fields = append(fields, actual)
					continue
				}
			}
			s.pos = mark
			fields = append(fields, tok)
		}
	}
}

// skipToBlock steps forward to the next selection set and past it.
func skipToBlock(s *scanner) {
	for {
		switch s.next() {
		case "":
			return
		case "{":
			skipBalanced(s, "{", "}")
			return
		}
	}
}

// skipBalanced consumes up to and including the closer matching an opener that
// has already been read.
func skipBalanced(s *scanner, opener, closer string) {
	depth := 1
	for depth > 0 {
		switch s.next() {
		case "":
			return
		case opener:
			depth++
		case closer:
			depth--
		}
	}
}

// scanner walks a document token by token.
//
// It returns names verbatim and everything else as a single punctuator, which
// is all the scan above distinguishes. Strings and numbers collapse to a marker
// because they only ever appear inside argument lists, which are skipped.
type scanner struct {
	src string
	pos int
}

const (
	tokenString = `"`
	tokenNumber = "0"
)

func (s *scanner) next() string {
	s.skipIgnored()
	if s.pos >= len(s.src) {
		return ""
	}

	c := s.src[s.pos]
	switch {
	case isNameStart(c):
		start := s.pos
		for s.pos < len(s.src) && isNameByte(s.src[s.pos]) {
			s.pos++
		}
		return s.src[start:s.pos]

	case c == '-' || (c >= '0' && c <= '9'):
		for s.pos < len(s.src) && !isPunctuator(s.src[s.pos]) && !isSpace(s.src[s.pos]) && s.src[s.pos] != ',' {
			s.pos++
		}
		return tokenNumber

	case c == '"':
		s.skipString()
		return tokenString

	case strings.HasPrefix(s.src[s.pos:], "..."):
		s.pos += 3
		return "..."

	default:
		s.pos++
		return string(c)
	}
}

// skipIgnored steps over whitespace, commas and comments. A `#` inside a string
// is not a comment, which is why strings are consumed as whole tokens.
func (s *scanner) skipIgnored() {
	for s.pos < len(s.src) {
		switch c := s.src[s.pos]; {
		case isSpace(c) || c == ',':
			s.pos++
		case c == '#':
			for s.pos < len(s.src) && s.src[s.pos] != '\n' {
				s.pos++
			}
		default:
			return
		}
	}
}

// skipString consumes a string, block or otherwise, from its opening quote.
func (s *scanner) skipString() {
	if strings.HasPrefix(s.src[s.pos:], `"""`) {
		s.pos += 3
		if end := strings.Index(s.src[s.pos:], `"""`); end >= 0 {
			s.pos += end + 3
			return
		}
		s.pos = len(s.src) // unterminated: the capture was cut
		return
	}

	s.pos++ // the opening quote
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case '"':
			s.pos++
			return
		case '\n':
			return // an ordinary string cannot span lines
		default:
			s.pos++
		}
	}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isPunctuator(c byte) bool {
	return strings.IndexByte("!$&()...:=@[]{|}", c) >= 0
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameByte(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// isName reports whether a token is a name rather than a punctuator. The two
// markers the scanner emits for strings and numbers are not names.
func isName(tok string) bool {
	return tok != "" && isNameStart(tok[0])
}

// Package sql turns SQL text into an abstract syntax tree.
//
// The lexer splits text into tokens; the parser is a hand-written
// recursive-descent parser with one function per grammar rule and
// precedence climbing for expressions.
package sql

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tQuotedIdent
	tNumber
	tString
	tParam
	tOp
)

type token struct {
	kind tokKind
	text string // identifiers are lower-cased; keywords are matched on this
	pos  int
}

// Error is a syntax error with the byte position where it happened.
type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("syntax error at position %d: %s", e.Pos+1, e.Msg) }

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, &Error{i, "unterminated comment"}
			}
			i += end + 4
		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			toks = append(toks, token{tIdent, strings.ToLower(src[start:i]), start})
		case c >= 0x80:
			r, size := utf8.DecodeRuneInString(src[i:])
			if !unicode.IsLetter(r) {
				return nil, &Error{i, fmt.Sprintf("unexpected character %q", r)}
			}
			start := i
			i += size
			for i < len(src) {
				r, size := utf8.DecodeRuneInString(src[i:])
				if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
					break
				}
				i += size
			}
			toks = append(toks, token{tIdent, strings.ToLower(src[start:i]), start})
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			start := i
			for i < len(src) && (src[i] >= '0' && src[i] <= '9' || src[i] == '.') {
				i++
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && src[j] >= '0' && src[j] <= '9' {
					i = j
					for i < len(src) && src[i] >= '0' && src[i] <= '9' {
						i++
					}
				}
			}
			toks = append(toks, token{tNumber, src[start:i], start})
		case c == '\'':
			start := i
			var sb strings.Builder
			i++
			for {
				if i >= len(src) {
					return nil, &Error{start, "unterminated string"}
				}
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						sb.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(src[i])
				i++
			}
			toks = append(toks, token{tString, sb.String(), start})
		case c == '"':
			start := i
			end := strings.IndexByte(src[i+1:], '"')
			if end < 0 {
				return nil, &Error{i, "unterminated quoted identifier"}
			}
			toks = append(toks, token{tQuotedIdent, src[i+1 : i+1+end], start})
			i += end + 2
		case c == '$' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			start := i
			i++
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			toks = append(toks, token{tParam, src[start+1 : i], start})
		default:
			start := i
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "<>", "!=", "||", "::":
				toks = append(toks, token{tOp, two, start})
				i += 2
				continue
			}
			if strings.IndexByte("(),;.*+-/%=<>", c) < 0 {
				return nil, &Error{i, fmt.Sprintf("unexpected character %q", c)}
			}
			toks = append(toks, token{tOp, string(c), start})
			i++
		}
	}
	return append(toks, token{tEOF, "", len(src)}), nil
}

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' || c == '$' }

// reserved words cannot be used as bare identifiers or aliases.
var reserved = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true, "not": true,
	"insert": true, "into": true, "values": true, "update": true, "set": true,
	"delete": true, "create": true, "drop": true, "table": true, "index": true,
	"on": true, "join": true, "inner": true, "left": true, "outer": true, "cross": true,
	"group": true, "by": true, "having": true, "order": true, "limit": true,
	"offset": true, "as": true, "null": true, "is": true, "in": true, "like": true,
	"ilike": true, "between": true, "distinct": true, "primary": true,
	"true": true, "false": true, "asc": true, "desc": true,
	"case": true, "when": true, "then": true, "else": true, "end": true,
	"union": true,
	"all":   true, "exists": true, "cast": true,
}

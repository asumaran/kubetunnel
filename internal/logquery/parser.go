// Package logquery implements a small DSL for filtering log entries.
//
// Grammar (informal):
//
//	query      := orExpr
//	orExpr     := andExpr ("OR" andExpr)*
//	andExpr    := term ("AND" term)*
//	term       := "NOT" term | "(" orExpr ")" | predicate | textSearch
//	predicate  := FIELD ":" VALUE
//	textSearch := QUOTED_STRING | BAREWORD
//
// Field predicates:
//
//	level:error        equality, case-insensitive
//	tunnel:web         substring match on tunnel name
//	status:5xx         range 500-599 (also 4xx, 3xx, 2xx, 1xx)
//	status:>=400       numeric comparison (<, <=, >, >=)
//	status:200         numeric equality
//	duration_ms:>500   numeric
//	path:/api/health   substring
//	event:restart      equality
//	msg:timeout        substring
//
// Operators are case-insensitive (AND / and, OR / or, NOT / not).
// Bare words do a substring match against msg + raw line.
package logquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asumaran/kubetunnel/internal/logging"
)

// Predicate is a function that accepts or rejects an Entry.
type Predicate func(logging.Entry) bool

// Always is a predicate that matches everything.
var Always Predicate = func(logging.Entry) bool { return true }

// Parse parses a query string into a Predicate. Empty query returns Always.
func Parse(query string) (Predicate, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Always, nil
	}
	toks, err := tokenize(query)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	pred, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.eof() {
		return nil, fmt.Errorf("unexpected token %q at position %d", p.peek().value, p.pos)
	}
	return pred, nil
}

// ---- tokenizer ----

type tokKind int

const (
	tkWord tokKind = iota
	tkString
	tkLParen
	tkRParen
	tkAnd
	tkOr
	tkNot
	tkEOF
)

type token struct {
	kind  tokKind
	value string
}

func tokenize(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(':
			out = append(out, token{tkLParen, "("})
			i++
		case c == ')':
			out = append(out, token{tkRParen, ")"})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string starting at %d", i)
			}
			out = append(out, token{tkString, s[i+1 : j]})
			i = j + 1
		default:
			j := i
			for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '(' && s[j] != ')' {
				j++
			}
			word := s[i:j]
			kind := tkWord
			switch strings.ToUpper(word) {
			case "AND":
				kind = tkAnd
			case "OR":
				kind = tkOr
			case "NOT":
				kind = tkNot
			}
			out = append(out, token{kind, word})
			i = j
		}
	}
	out = append(out, token{tkEOF, ""})
	return out, nil
}

// ---- parser ----

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) eof() bool   { return p.toks[p.pos].kind == tkEOF }

func (p *parser) parseOr() (Predicate, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tkOr {
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l, r := left, right
		left = func(e logging.Entry) bool { return l(e) || r(e) }
	}
	return left, nil
}

func (p *parser) parseAnd() (Predicate, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		// Implicit AND: two consecutive terms.
		if t.kind == tkAnd {
			p.pos++
		} else if t.kind == tkEOF || t.kind == tkRParen || t.kind == tkOr {
			break
		}
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		l, r := left, right
		left = func(e logging.Entry) bool { return l(e) && r(e) }
	}
	return left, nil
}

func (p *parser) parseTerm() (Predicate, error) {
	t := p.peek()
	switch t.kind {
	case tkNot:
		p.pos++
		inner, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		return func(e logging.Entry) bool { return !inner(e) }, nil
	case tkLParen:
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tkRParen {
			return nil, fmt.Errorf("expected ')'")
		}
		p.pos++
		return inner, nil
	case tkString:
		p.pos++
		needle := strings.ToLower(t.value)
		return func(e logging.Entry) bool {
			return strings.Contains(strings.ToLower(e.Msg), needle) ||
				strings.Contains(strings.ToLower(e.RawLine), needle)
		}, nil
	case tkWord:
		p.pos++
		return buildPredicate(t.value)
	default:
		return nil, fmt.Errorf("unexpected token %q", t.value)
	}
}

// buildPredicate parses a single "word" token into a predicate.
// If the word has a "field:value" form, it becomes a field match; otherwise
// it's a substring search against msg/raw.
func buildPredicate(word string) (Predicate, error) {
	colon := strings.IndexByte(word, ':')
	if colon == -1 {
		needle := strings.ToLower(word)
		return func(e logging.Entry) bool {
			return strings.Contains(strings.ToLower(e.Msg), needle) ||
				strings.Contains(strings.ToLower(e.RawLine), needle)
		}, nil
	}
	field := strings.ToLower(word[:colon])
	value := word[colon+1:]
	return fieldPredicate(field, value)
}

func fieldPredicate(field, value string) (Predicate, error) {
	// Status ranges like "5xx".
	if field == "status" && len(value) == 3 && (value[1] == 'x' || value[1] == 'X') && (value[2] == 'x' || value[2] == 'X') {
		first := value[0]
		if first < '1' || first > '5' {
			return nil, fmt.Errorf("invalid status range %q", value)
		}
		lo := int(first-'0') * 100
		hi := lo + 99
		return numericRange("status", lo, hi), nil
	}

	// Numeric comparisons.
	if op, n, ok := parseNumericOp(value); ok {
		return numericCompare(field, op, n), nil
	}

	// Plain numeric equality for known numeric fields.
	if n, err := strconv.ParseFloat(value, 64); err == nil && isNumericField(field) {
		return numericCompare(field, "==", n), nil
	}

	// String fields.
	lower := strings.ToLower(value)
	switch field {
	case "level", "stream", "event":
		return func(e logging.Entry) bool {
			v, ok := e.Field(field)
			if !ok {
				return false
			}
			s, _ := v.(string)
			return strings.EqualFold(s, value)
		}, nil
	case "tunnel", "path", "host", "method", "msg":
		return func(e logging.Entry) bool {
			v, ok := e.Field(field)
			if !ok {
				return false
			}
			s := fmt.Sprintf("%v", v)
			return strings.Contains(strings.ToLower(s), lower)
		}, nil
	}

	// Unknown field: substring match against Fields[field] if present.
	return func(e logging.Entry) bool {
		v, ok := e.Field(field)
		if !ok {
			return false
		}
		s := fmt.Sprintf("%v", v)
		return strings.Contains(strings.ToLower(s), lower)
	}, nil
}

var numericFields = map[string]bool{
	"status":      true,
	"duration_ms": true,
	"bytes":       true,
	"pid":         true,
	"restarts":    true,
	"attempt":     true,
	"delay_s":     true,
	"exit_code":   true,
}

func isNumericField(f string) bool { return numericFields[f] }

func parseNumericOp(value string) (op string, n float64, ok bool) {
	ops := []string{">=", "<=", ">", "<", "=="}
	for _, o := range ops {
		if strings.HasPrefix(value, o) {
			n, err := strconv.ParseFloat(value[len(o):], 64)
			if err != nil {
				return "", 0, false
			}
			return o, n, true
		}
	}
	return "", 0, false
}

func numericCompare(field, op string, n float64) Predicate {
	return func(e logging.Entry) bool {
		v, ok := e.Field(field)
		if !ok {
			return false
		}
		got, ok := asFloat(v)
		if !ok {
			return false
		}
		switch op {
		case ">=":
			return got >= n
		case "<=":
			return got <= n
		case ">":
			return got > n
		case "<":
			return got < n
		case "==":
			return got == n
		}
		return false
	}
}

func numericRange(field string, lo, hi int) Predicate {
	return func(e logging.Entry) bool {
		v, ok := e.Field(field)
		if !ok {
			return false
		}
		got, ok := asFloat(v)
		if !ok {
			return false
		}
		return got >= float64(lo) && got <= float64(hi)
	}
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

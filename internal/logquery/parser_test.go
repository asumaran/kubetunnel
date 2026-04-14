package logquery

import (
	"testing"

	"github.com/asumaran/kubetunnel/internal/logging"
)

func entry(level, tunnel, event, msg string, fields map[string]any) logging.Entry {
	return logging.Entry{
		Level:  level,
		Tunnel: tunnel,
		Event:  event,
		Msg:    msg,
		Fields: fields,
	}
}

func mustParse(t *testing.T, q string) Predicate {
	t.Helper()
	p, err := Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	return p
}

func TestParseEmptyMatchesAll(t *testing.T) {
	p := mustParse(t, "")
	if !p(logging.Entry{}) {
		t.Error("empty should match all")
	}
}

func TestParseLevelEquality(t *testing.T) {
	p := mustParse(t, "level:error")
	if !p(entry("error", "", "", "", nil)) {
		t.Error("expected match")
	}
	if p(entry("info", "", "", "", nil)) {
		t.Error("unexpected match")
	}
}

func TestParseTunnelSubstring(t *testing.T) {
	p := mustParse(t, "tunnel:web")
	if !p(entry("", "web-a-frontend", "", "", nil)) {
		t.Error("expected substring match")
	}
	if p(entry("", "api", "", "", nil)) {
		t.Error("unexpected match")
	}
}

func TestParseStatusRange(t *testing.T) {
	p := mustParse(t, "status:5xx")
	if !p(entry("", "", "", "", map[string]any{"status": 503})) {
		t.Error("503 should match 5xx")
	}
	if p(entry("", "", "", "", map[string]any{"status": 200})) {
		t.Error("200 should not match 5xx")
	}
}

func TestParseNumericComparison(t *testing.T) {
	p := mustParse(t, "status:>=400")
	if !p(entry("", "", "", "", map[string]any{"status": 404})) {
		t.Error("404 should match >=400")
	}
	if p(entry("", "", "", "", map[string]any{"status": 200})) {
		t.Error("200 should not match >=400")
	}
}

func TestParseDurationGt(t *testing.T) {
	p := mustParse(t, "duration_ms:>500")
	if !p(entry("", "", "", "", map[string]any{"duration_ms": 800})) {
		t.Error("expected match")
	}
	if p(entry("", "", "", "", map[string]any{"duration_ms": 100})) {
		t.Error("unexpected match")
	}
}

func TestParseAnd(t *testing.T) {
	p := mustParse(t, "level:error AND tunnel:api")
	if !p(entry("error", "api-eshop", "", "", nil)) {
		t.Error("expected match")
	}
	if p(entry("error", "web-a", "", "", nil)) {
		t.Error("unexpected match")
	}
	if p(entry("info", "api-eshop", "", "", nil)) {
		t.Error("unexpected match")
	}
}

func TestParseImplicitAnd(t *testing.T) {
	p := mustParse(t, "level:error tunnel:api")
	if !p(entry("error", "api-eshop", "", "", nil)) {
		t.Error("expected match")
	}
	if p(entry("info", "api-eshop", "", "", nil)) {
		t.Error("unexpected match")
	}
}

func TestParseOr(t *testing.T) {
	p := mustParse(t, "level:error OR level:warn")
	if !p(entry("error", "", "", "", nil)) {
		t.Error("expected error to match")
	}
	if !p(entry("warn", "", "", "", nil)) {
		t.Error("expected warn to match")
	}
	if p(entry("info", "", "", "", nil)) {
		t.Error("info should not match")
	}
}

func TestParseNot(t *testing.T) {
	p := mustParse(t, "NOT level:info")
	if !p(entry("error", "", "", "", nil)) {
		t.Error("expected NOT info to match error")
	}
	if p(entry("info", "", "", "", nil)) {
		t.Error("NOT info should not match info")
	}
}

func TestParseQuotedTextSearch(t *testing.T) {
	p := mustParse(t, `"timeout"`)
	if !p(entry("", "", "", "connection timeout occurred", nil)) {
		t.Error("expected quoted substring match")
	}
	if p(entry("", "", "", "all good", nil)) {
		t.Error("unexpected match")
	}
}

func TestParseBareTextSearch(t *testing.T) {
	p := mustParse(t, "EOF")
	if !p(entry("", "", "", "connection EOF", nil)) {
		t.Error("expected match")
	}
}

func TestParseParentheses(t *testing.T) {
	p := mustParse(t, "level:error AND (tunnel:api OR tunnel:web)")
	if !p(entry("error", "api", "", "", nil)) {
		t.Error("expected match")
	}
	if !p(entry("error", "web-a", "", "", nil)) {
		t.Error("expected match")
	}
	if p(entry("error", "backend", "", "", nil)) {
		t.Error("unexpected match")
	}
}

func TestParseSyntaxError(t *testing.T) {
	if _, err := Parse("("); err == nil {
		t.Error("expected error for unclosed paren")
	}
	if _, err := Parse(`"unterminated`); err == nil {
		t.Error("expected error for unterminated string")
	}
}

func TestParseStatusRangeInvalid(t *testing.T) {
	if _, err := Parse("status:9xx"); err == nil {
		t.Error("expected error for invalid range")
	}
}

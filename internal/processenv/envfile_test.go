package processenv

import (
	"strings"
	"testing"
)

func TestParseEnvFileParsesCoreSyntax(t *testing.T) {
	content := `# leading comment
ANTHROPIC_AUTH_TOKEN=sk-live-123

export OPENAI_API_KEY=sk-openai-456
GC_DOLT_PASSWORD = secret with spaces
QUOTED_DOUBLE="value with = and # inside"
QUOTED_SINGLE='single value'
   # indented comment
EMPTY_VALUE=
TRAILING_INLINE=keep#notacomment
`
	got, err := ParseEnvFile(content)
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	want := map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "sk-live-123",
		"OPENAI_API_KEY":       "sk-openai-456",
		"GC_DOLT_PASSWORD":     "secret with spaces",
		"QUOTED_DOUBLE":        "value with = and # inside",
		"QUOTED_SINGLE":        "single value",
		"EMPTY_VALUE":          "",
		"TRAILING_INLINE":      "keep#notacomment",
	}
	if len(got) != len(want) {
		t.Fatalf("ParseEnvFile returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ParseEnvFile()[%q] = %q, want %q", key, got[key], wantVal)
		}
	}
}

func TestParseEnvFileEmptyContentReturnsEmptyMap(t *testing.T) {
	got, err := ParseEnvFile("")
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ParseEnvFile(\"\") = %v, want empty map", got)
	}
}

func TestParseEnvFileRejectsMalformedLines(t *testing.T) {
	for name, content := range map[string]string{
		"missing equals":       "ANTHROPIC_AUTH_TOKEN sk-live-123",
		"empty key":            "=value",
		"empty key after trim": "   =value",
	} {
		if _, err := ParseEnvFile(content); err == nil {
			t.Errorf("ParseEnvFile(%s) = nil error, want error", name)
		}
	}
}

// TestParseEnvFileErrorsOmitLineContent guards the no-value-leak contract:
// secrets files are the primary input, and a malformed credential line (e.g.
// a token pasted without '=') must not be echoed into error messages that
// callers print to stderr or service logs.
func TestParseEnvFileErrorsOmitLineContent(t *testing.T) {
	for name, tc := range map[string]struct{ content, secret string }{
		"missing equals": {"OPENAI_API_KEY sk-leak-probe-123", "sk-leak-probe-123"},
		"empty key":      {"=sk-leak-probe-456", "sk-leak-probe-456"},
	} {
		_, err := ParseEnvFile(tc.content)
		if err == nil {
			t.Errorf("ParseEnvFile(%s) = nil error, want error", name)
			continue
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Errorf("ParseEnvFile(%s) error %q echoes the line content", name, err)
		}
	}
}

func TestParseEnvFileLastDuplicateWins(t *testing.T) {
	got, err := ParseEnvFile("KEY=first\nKEY=second\n")
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	if got["KEY"] != "second" {
		t.Errorf("ParseEnvFile duplicate KEY = %q, want %q", got["KEY"], "second")
	}
}

// TestQuoteEnvValueRoundTripsThroughParse asserts the writer/parser contract:
// for any line-break-free value, KEY=QuoteEnvValue(val) parses back to val.
func TestQuoteEnvValueRoundTripsThroughParse(t *testing.T) {
	for name, val := range map[string]string{
		"plain token":        "sk-live-123",
		"internal spaces":    "secret with spaces",
		"leading whitespace": "  padded",
		"trailing space":     "padded ",
		"equals and hash":    "a=b#c",
		"interior quotes":    `with "quotes" inside`,
		"quote wrapped":      `"wrapped"`,
		"single quoted":      "'wrapped'",
		"lone quote":         `"`,
		"trailing backslash": `value\`,
	} {
		got, err := ParseEnvFile("KEY=" + QuoteEnvValue(val) + "\n")
		if err != nil {
			t.Errorf("ParseEnvFile(%s) returned error: %v", name, err)
			continue
		}
		if got["KEY"] != val {
			t.Errorf("round trip (%s) = %q, want %q", name, got["KEY"], val)
		}
	}
}

func TestEnvFileAssignmentKey(t *testing.T) {
	for name, tc := range map[string]struct {
		line    string
		wantKey string
		wantOK  bool
	}{
		"plain assignment": {"OPENAI_API_KEY=sk-1", "OPENAI_API_KEY", true},
		"export prefix":    {"export OPENAI_API_KEY=sk-1", "OPENAI_API_KEY", true},
		"padded key":       {"  OPENAI_API_KEY = sk-1", "OPENAI_API_KEY", true},
		"comment":          {"# OPENAI_API_KEY=sk-1", "", false},
		"blank":            {"   ", "", false},
		"missing equals":   {"OPENAI_API_KEY sk-1", "", false},
		"empty key":        {"=sk-1", "", false},
	} {
		key, ok := EnvFileAssignmentKey(tc.line)
		if key != tc.wantKey || ok != tc.wantOK {
			t.Errorf("EnvFileAssignmentKey(%s) = (%q, %v), want (%q, %v)", name, key, ok, tc.wantKey, tc.wantOK)
		}
	}
}

package processenv

import (
	"fmt"
	"strings"
)

// ParseEnvFile parses dotenv-style content into a key/value map. The format is
// the common EnvironmentFile / docker --env-file subset:
//
//   - one KEY=VALUE assignment per line;
//   - blank lines and lines whose first non-space character is '#' are ignored;
//   - an optional leading "export " prefix on the key is stripped;
//   - surrounding whitespace around the key and value is trimmed;
//   - a value fully wrapped in matching single or double quotes is unquoted,
//     preserving '=' and '#' characters inside the quotes.
//
// Values are treated literally otherwise: there is no variable interpolation
// and no inline-comment stripping on unquoted values (a '#' mid-value is kept).
// A line missing '=' or with an empty key is a parse error so a malformed
// secrets file fails loudly rather than silently dropping a credential. Parse
// errors carry only the line number, never the line content — the input is
// typically a secrets file and errors flow to stderr and service logs. The
// last assignment wins when a key repeats.
func ParseEnvFile(content string) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: missing '='", i+1)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}
		out[key] = unquoteEnvValue(strings.TrimSpace(val))
	}
	return out, nil
}

// unquoteEnvValue strips one layer of matching surrounding single or double
// quotes from a trimmed value; otherwise it returns the value unchanged.
func unquoteEnvValue(val string) string {
	if len(val) < 2 {
		return val
	}
	first, last := val[0], val[len(val)-1]
	if (first == '"' || first == '\'') && last == first {
		return val[1 : len(val)-1]
	}
	return val
}

// QuoteEnvValue renders val so a KEY=<rendered> line parses back to exactly
// val under ParseEnvFile's rules. Values that survive the parser's trim and
// unquote untouched are written bare; anything else (surrounding whitespace,
// quote-wrapped lookalikes) gains one layer of double quotes, which the
// parser strips while preserving the interior verbatim. Values containing
// line breaks cannot be represented in the line-oriented format; callers
// must reject them before writing.
func QuoteEnvValue(val string) string {
	if val == unquoteEnvValue(strings.TrimSpace(val)) {
		return val
	}
	return `"` + val + `"`
}

// EnvFileAssignmentKey returns the key a dotenv line assigns, applying the
// same per-line rules as ParseEnvFile, and false for blank lines, comments,
// and malformed lines. It lets callers patch an env file line-by-line
// without re-implementing the format.
func EnvFileAssignmentKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", false
	}
	key = strings.TrimSpace(key)
	return key, key != ""
}

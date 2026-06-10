package packlint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOperationalAwarenessNoHardcodedDoltPort guards gc-cte: the
// operational-awareness template fragment described the Dolt data plane as
// running "on port 3307", but that number is wrong for any city configured on
// a different port (the live city runs on 56801). A hardcoded port misleads
// diagnostics — a human or agent reading the rendered context reaches for the
// wrong port. The fragment must direct readers to the configured port (via
// `gc dolt status` / dolt-config.yaml) instead of naming a specific number.
func TestOperationalAwarenessNoHardcodedDoltPort(t *testing.T) {
	root := repoRoot()
	fragment := filepath.Join(root, "examples", "gastown", "packs", "gastown",
		"template-fragments", "operational-awareness.template.md")

	data, err := os.ReadFile(fragment)
	if err != nil {
		t.Fatalf("reading %s: %v", fragment, err)
	}

	// Matches a prose port reference like "port 3307" or "on port 3307".
	re := regexp.MustCompile(`port\s+3307`)

	var hits []string
	for lineNo, line := range strings.Split(string(data), "\n") {
		if re.MatchString(line) {
			hits = append(hits, fmt.Sprintf("line %d: %s", lineNo+1, strings.TrimSpace(line)))
		}
	}

	if len(hits) > 0 {
		rel, _ := filepath.Rel(root, fragment)
		t.Fatalf("%s hardcodes a Dolt port (gc-cte); direct readers to the configured port (`gc dolt status` / dolt-config.yaml) instead:\n  %s",
			rel, strings.Join(hits, "\n  "))
	}
}

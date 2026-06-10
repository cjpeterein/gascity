//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statusLineScriptPath returns the path to the gastown pack's
// status-line.sh helper under test.
func statusLineScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown",
		"assets", "scripts", "status-line.sh")
}

// runStatusLine executes status-line.sh for the given agent with a fake
// `gc` on PATH that emits the supplied hook and mail output, then returns
// the script's combined output.
func runStatusLine(t *testing.T, agent, hookOutput string, hookExit int, mailOutput string, mailExit int) string {
	t.Helper()

	fakeDir := t.TempDir()
	writeFakeGCForStatusLine(t, filepath.Join(fakeDir, "gc"), hookOutput, hookExit, mailOutput, mailExit)

	env := newIsolatedToolEnv(t, false)
	envMap := parseEnvList(env)
	env = replaceEnv(env, "PATH", prependPath(fakeDir, envMap["PATH"]))
	// Drop city-path vars so the script's optional _bd_trace.sh sourcing
	// stays a no-op and cannot reach a real maintenance pack.
	env = filterEnvMany(env, "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT")

	out, err := runCommand(fakeDir, env, 30*time.Second, "bash", statusLineScriptPath(t), agent)
	if err != nil {
		t.Fatalf("status-line.sh failed: %v\noutput: %s", err, out)
	}
	return out
}

// writeFakeGCForStatusLine writes a fake `gc` that responds to `gc hook
// <agent>` and `gc mail count <agent> --json` with canned output and exit
// codes, mirroring the real CLI's contract (hook prints a JSON array; mail
// count prints {"total":N,"unread":N}).
func writeFakeGCForStatusLine(t *testing.T, path, hookOutput string, hookExit int, mailOutput string, mailExit int) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

cmd="$1"
shift || true

case "$cmd" in
  hook)
    printf '%%s' %q
    exit %d
    ;;
  mail)
    # real CLI: "gc mail count <agent> --json"
    printf '%%s' %q
    exit %d
    ;;
  *)
    echo "unexpected gc command: $cmd" >&2
    exit 1
    ;;
esac
`, hookOutput, hookExit, mailOutput, mailExit)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gc command: %v", err)
	}
}

// TestStatusLineEmptyHookRendersNoBadge is the regression guard for gc-0s5:
// `gc hook` prints the literal `[]` when there is no work, and the old
// `grep -c .` counted that as one non-empty line, rendering "🪝 1". The
// badge must be suppressed for an empty JSON array.
func TestStatusLineEmptyHookRendersNoBadge(t *testing.T) {
	// Empty hook with exit 1 (no work, like a freshly-drained agent).
	out := runStatusLine(t, "mayor", "[]", 1, "", 1)
	if strings.Contains(out, "🪝") {
		t.Fatalf("empty hook rendered a hook badge: %q", out)
	}
	if !strings.Contains(out, "mayor") {
		t.Fatalf("status line missing agent name: %q", out)
	}
}

// TestStatusLineEmptyHookExitZeroRendersNoBadge covers the case where an
// agent's hook is empty but `gc hook` exits 0 (e.g. the agent just claimed
// its only work). The empty array must still suppress the badge, proving
// the count is driven by array length, not exit code.
func TestStatusLineEmptyHookExitZeroRendersNoBadge(t *testing.T) {
	out := runStatusLine(t, "polecat", "[]", 0, "", 1)
	if strings.Contains(out, "🪝") {
		t.Fatalf("empty hook (exit 0) rendered a hook badge: %q", out)
	}
}

// TestStatusLineNonEmptyHookRendersCount asserts the badge shows the JSON
// array length, not the line count. A single-line array of three items
// must render "🪝 3", not "🪝 1".
func TestStatusLineNonEmptyHookRendersCount(t *testing.T) {
	hook := `[{"id":"a"},{"id":"b"},{"id":"c"}]`
	out := runStatusLine(t, "witness", hook, 0, "", 1)
	if !strings.Contains(out, "🪝 3") {
		t.Fatalf("hook with 3 items rendered %q, want '🪝 3'", out)
	}
}

// TestStatusLineMailSegmentStillRenders confirms the mail segment renders
// from the count-only endpoint: the unread field of `gc mail count --json`
// drives the mail badge.
func TestStatusLineMailSegmentStillRenders(t *testing.T) {
	out := runStatusLine(t, "mayor", "[]", 1, `{"total":3,"unread":2}`, 0)
	if strings.Contains(out, "🪝") {
		t.Fatalf("empty hook rendered a hook badge alongside mail: %q", out)
	}
	if !strings.Contains(out, "📬 2") {
		t.Fatalf("mail segment rendered %q, want '📬 2'", out)
	}
}

// TestStatusLineHookAndMailTogether exercises both segments at once.
func TestStatusLineHookAndMailTogether(t *testing.T) {
	hook := `[{"id":"a"},{"id":"b"}]`
	out := runStatusLine(t, "deacon", hook, 0, `{"total":5,"unread":5}`, 0)
	if !strings.Contains(out, "🪝 2") {
		t.Fatalf("hook segment rendered %q, want '🪝 2'", out)
	}
	if !strings.Contains(out, "📬 5") {
		t.Fatalf("mail segment rendered %q, want '📬 5'", out)
	}
}

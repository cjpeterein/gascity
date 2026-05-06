package bootstrap

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// readCorePolecatCommitFormula returns the raw contents of the embedded
// mol-polecat-commit formula used by the core pack.
func readCorePolecatCommitFormula(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(bootstrapAssets, "packs/core/formulas/mol-polecat-commit.toml")
	if err != nil {
		t.Fatalf("reading mol-polecat-commit.toml from embedded assets: %v", err)
	}
	return string(data)
}

func TestPolecatCommitFormulaGatesDrainExitCleanup(t *testing.T) {
	body := readCorePolecatCommitFormula(t)

	for _, want := range []string{
		`git status --porcelain`,
		`git rev-list --count origin/{{base_branch}}..HEAD`,
		`skip-cleanup: working tree is dirty`,
		`skip-cleanup:`,
		`git worktree remove "$WORKTREE_PATH"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("polecat-commit formula missing gated cleanup guidance %q", want)
		}
	}

	if strings.Contains(body, `git worktree remove "$WORKTREE_PATH" --force`) {
		t.Errorf("polecat-commit formula must not force-remove worktree after adding gated cleanup")
	}

	assertContainsInOrderCommit(t, body,
		`git push origin HEAD:{{base_branch}}`,
		`git status --porcelain`,
		`git rev-list --count origin/{{base_branch}}..HEAD`,
		`git worktree remove "$WORKTREE_PATH"`,
		`gc runtime drain-ack`,
	)
}

func TestPolecatCommitFormulaVersionBumpedForDrainExitCleanup(t *testing.T) {
	body := readCorePolecatCommitFormula(t)
	var parsed struct {
		Version int `toml:"version"`
	}
	if _, err := toml.Decode(body, &parsed); err != nil {
		t.Fatalf("decoding polecat-commit formula: %v", err)
	}
	if parsed.Version < 3 {
		t.Errorf("mol-polecat-commit version = %d, want >= 3 (bumped for adhoc session-close on drain-exit)", parsed.Version)
	}
}

// TestPolecatCommitFormulaGatesAdhocSessionCloseOnDrainExit verifies that the
// drain-exit cleanup closes the session bead only when the alias matches the
// adhoc pattern, and that the close runs before gc runtime drain-ack.
func TestPolecatCommitFormulaGatesAdhocSessionCloseOnDrainExit(t *testing.T) {
	body := readCorePolecatCommitFormula(t)

	for _, want := range []string{
		`-adhoc-[0-9a-f]+$`,
		`gc session close "$GC_SESSION_ID" || true`,
		`GC_SESSION_NAME`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("polecat-commit formula missing adhoc session-close guidance %q", want)
		}
	}

	if strings.Contains(body, `gc session close "$GC_SESSION_ID"`) &&
		!strings.Contains(body, `gc session close "$GC_SESSION_ID" || true`) {
		t.Errorf("polecat-commit formula must not let session close mask drain-ack (missing || true)")
	}

	assertContainsInOrderCommit(t, body,
		`git worktree remove "$WORKTREE_PATH"`,
		`-adhoc-[0-9a-f]+$`,
		`gc session close "$GC_SESSION_ID" || true`,
		`gc runtime drain-ack`,
	)
}

// assertContainsInOrderCommit checks that each fragment appears in body in the
// given order. Duplicated in this package to avoid a cross-package test helper
// dependency.
func assertContainsInOrderCommit(t *testing.T, body string, fragments ...string) {
	t.Helper()
	pos := 0
	for _, frag := range fragments {
		idx := strings.Index(body[pos:], frag)
		if idx < 0 {
			t.Errorf("formula missing %q after position %d", frag, pos)
			return
		}
		pos += idx + len(frag)
	}
}

package githooks

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHooksConfig is an in-memory [HooksConfig] for tests.
type fakeHooksConfig struct {
	values map[string]string // rigPath -> hooksPath
}

func newFakeHooksConfig() *fakeHooksConfig {
	return &fakeHooksConfig{values: map[string]string{}}
}

func (f *fakeHooksConfig) GetHooksPath(rigPath string) (string, bool, error) {
	v, ok := f.values[rigPath]
	return v, ok, nil
}

func (f *fakeHooksConfig) SetHooksPath(rigPath, value string) error {
	f.values[rigPath] = value
	return nil
}

func TestWriteCityHook_FreshCity(t *testing.T) {
	cityPath := t.TempDir()
	wrote, err := WriteCityHook(cityPath)
	if err != nil {
		t.Fatalf("WriteCityHook: %v", err)
	}
	if !wrote {
		t.Errorf("expected wrote=true for fresh city, got false")
	}
	hookPath := filepath.Join(cityPath, ".gc", "hooks", "prepare-commit-msg")
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read written hook: %v", err)
	}
	if !bytes.Equal(got, CityHookScript()) {
		t.Errorf("written hook content does not match embedded script")
	}
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook file is not executable: mode=%v", info.Mode())
	}
}

func TestWriteCityHook_IdempotentWhenContentMatches(t *testing.T) {
	cityPath := t.TempDir()
	if _, err := WriteCityHook(cityPath); err != nil {
		t.Fatalf("first write: %v", err)
	}
	wrote, err := WriteCityHook(cityPath)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if wrote {
		t.Errorf("expected wrote=false on second invocation, got true")
	}
}

func TestWriteCityHook_RestoresExecBitWhenStrippedExternally(t *testing.T) {
	cityPath := t.TempDir()
	if _, err := WriteCityHook(cityPath); err != nil {
		t.Fatalf("write: %v", err)
	}
	hookPath := filepath.Join(cityPath, ".gc", "hooks", "prepare-commit-msg")
	if err := os.Chmod(hookPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := WriteCityHook(cityPath); err != nil {
		t.Fatalf("re-write: %v", err)
	}
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("expected exec bit restored, got mode=%v", info.Mode())
	}
}

func TestWriteCityHook_EmptyCityPathErrors(t *testing.T) {
	if _, err := WriteCityHook(""); err == nil {
		t.Errorf("expected error for empty cityPath")
	}
}

func TestWriteCityHook_RewritesWhenEmbeddedScriptChanges(t *testing.T) {
	cityPath := t.TempDir()
	hookPath := filepath.Join(cityPath, ".gc", "hooks", "prepare-commit-msg")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nold content\n"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	wrote, err := WriteCityHook(cityPath)
	if err != nil {
		t.Fatalf("WriteCityHook: %v", err)
	}
	if !wrote {
		t.Errorf("expected wrote=true when content differs, got false")
	}
	got, _ := os.ReadFile(hookPath)
	if !bytes.Equal(got, CityHookScript()) {
		t.Errorf("expected content replaced with embedded script")
	}
}

func TestInstallStub_FreshRig_CreatesDotGithooksAndSetsHooksPath(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	cfg := newFakeHooksConfig()

	res, err := InstallStub(cfg, rigPath, cityPath)
	if err != nil {
		t.Fatalf("InstallStub: %v", err)
	}
	wantDir := filepath.Join(rigPath, ".githooks")
	if res.HooksDir != wantDir {
		t.Errorf("HooksDir = %q, want %q", res.HooksDir, wantDir)
	}
	if !res.CreatedDir {
		t.Errorf("expected CreatedDir=true")
	}
	if !res.SetHooksPath {
		t.Errorf("expected SetHooksPath=true")
	}
	if !res.WroteFile {
		t.Errorf("expected WroteFile=true")
	}
	if !res.BlockAppended {
		t.Errorf("expected BlockAppended=true on fresh install")
	}
	if cfg.values[rigPath] != ".githooks" {
		t.Errorf("expected core.hooksPath set to .githooks, got %q", cfg.values[rigPath])
	}

	hookFile := filepath.Join(wantDir, "prepare-commit-msg")
	got, err := os.ReadFile(hookFile)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	gotStr := string(got)
	if !strings.HasPrefix(gotStr, "#!/usr/bin/env sh\n") {
		t.Errorf("hook missing shebang; got prefix %q", gotStr[:clamp(40, len(gotStr))])
	}
	if !strings.Contains(gotStr, "BEGIN GASCITY FOOTER "+MarkerVersion) {
		t.Errorf("hook missing BEGIN marker line")
	}
	if !strings.Contains(gotStr, "END GASCITY FOOTER "+MarkerVersion) {
		t.Errorf("hook missing END marker line")
	}
	// City path is baked into the ${GC_CITY:-...} default expansion.
	if !strings.Contains(gotStr, "${GC_CITY:-"+cityPath+"}/.gc/hooks/prepare-commit-msg") {
		t.Errorf("hook missing baked-in city path %q", cityPath)
	}
	info, err := os.Stat(hookFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook not executable: mode=%v", info.Mode())
	}
}

func TestInstallStub_CoexistsWithBeadsBlock(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	cfg := newFakeHooksConfig()
	cfg.values[rigPath] = ".beads/hooks"

	hooksDir := filepath.Join(rigPath, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bdContent := `#!/usr/bin/env sh
# --- BEGIN BEADS INTEGRATION v1.0.4 ---
# This section is managed by beads. Do not remove these markers.
if command -v bd >/dev/null 2>&1; then
  export BD_GIT_HOOK=1
  bd hooks run prepare-commit-msg "$@"
  _bd_exit=$?; if [ $_bd_exit -ne 0 ]; then exit $_bd_exit; fi
fi
# --- END BEADS INTEGRATION v1.0.4 ---
`
	hookFile := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookFile, []byte(bdContent), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := InstallStub(cfg, rigPath, cityPath)
	if err != nil {
		t.Fatalf("InstallStub: %v", err)
	}
	if res.HooksDir != hooksDir {
		t.Errorf("HooksDir = %q, want %q", res.HooksDir, hooksDir)
	}
	if res.CreatedDir {
		t.Errorf("expected CreatedDir=false (dir already existed)")
	}
	if res.SetHooksPath {
		t.Errorf("expected SetHooksPath=false (already set)")
	}
	if !res.BlockAppended {
		t.Errorf("expected BlockAppended=true (no prior gc block)")
	}

	got, err := os.ReadFile(hookFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(got, []byte("BEGIN BEADS INTEGRATION")) {
		t.Errorf("bd block lost after install")
	}
	if !bytes.Contains(got, []byte("BEGIN GASCITY FOOTER")) {
		t.Errorf("gascity block missing")
	}
	// Order: bd block before gascity block.
	bdIdx := bytes.Index(got, []byte("BEGIN BEADS INTEGRATION"))
	gcIdx := bytes.Index(got, []byte("BEGIN GASCITY FOOTER"))
	if bdIdx < 0 || gcIdx < 0 || gcIdx < bdIdx {
		t.Errorf("expected gascity block after bd block; bdIdx=%d gcIdx=%d", bdIdx, gcIdx)
	}
}

func TestInstallStub_IdempotentSecondRun(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	cfg := newFakeHooksConfig()

	if _, err := InstallStub(cfg, rigPath, cityPath); err != nil {
		t.Fatalf("first install: %v", err)
	}
	hookFile := filepath.Join(rigPath, ".githooks", "prepare-commit-msg")
	first, err := os.ReadFile(hookFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	res, err := InstallStub(cfg, rigPath, cityPath)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if res.WroteFile {
		t.Errorf("expected WroteFile=false on idempotent re-install")
	}
	second, err := os.ReadFile(hookFile)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("file content changed on idempotent re-install:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestInstallStub_ReplacesExistingGascityBlockInPlace(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	cfg := newFakeHooksConfig()

	// Pre-existing file with an old-version gascity block + bd block + tail.
	hooksDir := filepath.Join(rigPath, ".githooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := `#!/usr/bin/env sh
# --- BEGIN BEADS INTEGRATION v1.0.4 ---
echo bd
# --- END BEADS INTEGRATION v1.0.4 ---

# --- BEGIN GASCITY FOOTER v0.9 ---
old gascity content
# --- END GASCITY FOOTER v0.9 ---

# user-added trailing logic
echo trailing
`
	hookFile := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookFile, []byte(old), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg.values[rigPath] = ".githooks"

	res, err := InstallStub(cfg, rigPath, cityPath)
	if err != nil {
		t.Fatalf("InstallStub: %v", err)
	}
	if !res.WroteFile {
		t.Errorf("expected WroteFile=true (block version changed)")
	}
	if !res.BlockUpdated {
		t.Errorf("expected BlockUpdated=true")
	}
	if res.BlockAppended {
		t.Errorf("expected BlockAppended=false (replaced in place)")
	}

	got, _ := os.ReadFile(hookFile)
	if bytes.Contains(got, []byte("BEGIN GASCITY FOOTER v0.9")) {
		t.Errorf("old version marker still present")
	}
	if !bytes.Contains(got, []byte("BEGIN GASCITY FOOTER "+MarkerVersion)) {
		t.Errorf("new version marker missing")
	}
	if !bytes.Contains(got, []byte("echo trailing")) {
		t.Errorf("user-added trailing content was clobbered")
	}
	if !bytes.Contains(got, []byte("BEGIN BEADS INTEGRATION")) {
		t.Errorf("bd block was clobbered")
	}
}

func TestInstallStub_PrependsShebangWhenMissing(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	cfg := newFakeHooksConfig()

	hooksDir := filepath.Join(rigPath, ".githooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	hookFile := filepath.Join(hooksDir, "prepare-commit-msg")
	// No shebang.
	if err := os.WriteFile(hookFile, []byte("echo hello\n"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg.values[rigPath] = ".githooks"

	if _, err := InstallStub(cfg, rigPath, cityPath); err != nil {
		t.Fatalf("InstallStub: %v", err)
	}
	got, _ := os.ReadFile(hookFile)
	if !bytes.HasPrefix(got, []byte("#!/usr/bin/env sh\n")) {
		t.Errorf("expected shebang prepended; got prefix %q", string(got[:clamp(40, len(got))]))
	}
}

func TestInstallStub_AbsoluteHooksPathHonored(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	abs := filepath.Join(t.TempDir(), "absolute-hooks")
	cfg := newFakeHooksConfig()
	cfg.values[rigPath] = abs

	res, err := InstallStub(cfg, rigPath, cityPath)
	if err != nil {
		t.Fatalf("InstallStub: %v", err)
	}
	if res.HooksDir != abs {
		t.Errorf("HooksDir = %q, want absolute %q", res.HooksDir, abs)
	}
	if !res.CreatedDir {
		t.Errorf("expected CreatedDir=true (absolute dir didn't exist)")
	}
	// Not changing core.hooksPath since it was already set.
	if res.SetHooksPath {
		t.Errorf("expected SetHooksPath=false (already set)")
	}
}

func TestInstallStub_EmptyArgsError(t *testing.T) {
	cfg := newFakeHooksConfig()
	if _, err := InstallStub(cfg, "", "/some/city"); err == nil {
		t.Errorf("expected error for empty rigPath")
	}
	if _, err := InstallStub(cfg, "/some/rig", ""); err == nil {
		t.Errorf("expected error for empty cityPath")
	}
}

// errorHooksConfig surfaces an error from GetHooksPath to verify the
// installer wraps the error with rig context.
type errorHooksConfig struct{ err error }

func (e errorHooksConfig) GetHooksPath(string) (string, bool, error) { return "", false, e.err }
func (e errorHooksConfig) SetHooksPath(string, string) error         { return e.err }

func TestInstallStub_PropagatesGetHooksPathError(t *testing.T) {
	rigPath := t.TempDir()
	cityPath := t.TempDir()
	sentinel := errors.New("boom")
	if _, err := InstallStub(errorHooksConfig{err: sentinel}, rigPath, cityPath); err == nil {
		t.Errorf("expected error propagated, got nil")
	} else if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel, got %v", err)
	}
}

func clamp(a, b int) int {
	if a < b {
		return a
	}
	return b
}

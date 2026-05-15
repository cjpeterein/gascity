package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/githooks"
)

// fakeRepo creates a stub `.git` marker file so isGitRepo() returns true.
// Tests that don't need real git plumbing can use this to satisfy the
// repo-presence guard.
func fakeRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("fakeRepo: %v", err)
	}
}

// fakeMaintHooksConfig is the test double for githooks.HooksConfig.
type fakeMaintHooksConfig struct {
	values map[string]string
}

func newFakeMaintHooksConfig() *fakeMaintHooksConfig {
	return &fakeMaintHooksConfig{values: map[string]string{}}
}

func (f *fakeMaintHooksConfig) GetHooksPath(rigPath string) (string, bool, error) {
	v, ok := f.values[rigPath]
	return v, ok, nil
}

func (f *fakeMaintHooksConfig) SetHooksPath(rigPath, value string) error {
	f.values[rigPath] = value
	return nil
}

func TestDoMaintenanceInstallHooks_FreshCity_InstallsCityAndRigs(t *testing.T) {
	cityPath := t.TempDir()
	rig1 := filepath.Join(t.TempDir(), "rig1")
	rig2 := filepath.Join(t.TempDir(), "rig2")
	if err := os.MkdirAll(rig1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rig2, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRepo(t, rig1)
	fakeRepo(t, rig2)
	fakeRepo(t, cityPath)

	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "rig1"
path = "` + rig1 + `"

[[rigs]]
name = "rig2"
path = "` + rig2 + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := newFakeMaintHooksConfig()
	var stdout, stderr bytes.Buffer
	code := doMaintenanceInstallHooks(cfg, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMaintenanceInstallHooks returned %d, stderr: %s", code, stderr.String())
	}

	// City hook present and executable.
	hookPath := githooks.CityHookPath(cityPath)
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("city hook not written: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("city hook not executable: mode=%v", info.Mode())
	}

	// Each rig has a stub.
	for _, rig := range []string{rig1, rig2, cityPath} {
		stub := filepath.Join(rig, ".githooks", "prepare-commit-msg")
		got, err := os.ReadFile(stub)
		if err != nil {
			t.Errorf("rig %s stub not written: %v", rig, err)
			continue
		}
		if !bytes.Contains(got, []byte("BEGIN GASCITY FOOTER "+githooks.MarkerVersion)) {
			t.Errorf("rig %s stub missing marker block", rig)
		}
	}

	// stdout summarizes each rig.
	out := stdout.String()
	if !strings.Contains(out, "rig rig1: stub appended") {
		t.Errorf("expected stdout summary for rig1, got:\n%s", out)
	}
	if !strings.Contains(out, "rig rig2: stub appended") {
		t.Errorf("expected stdout summary for rig2, got:\n%s", out)
	}
	if !strings.Contains(out, "rig test-city: stub appended") {
		t.Errorf("expected HQ summary, got:\n%s", out)
	}
}

func TestDoMaintenanceInstallHooks_Idempotent(t *testing.T) {
	cityPath := t.TempDir()
	rig := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRepo(t, rig)
	fakeRepo(t, cityPath)
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "rig"
path = "` + rig + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := newFakeMaintHooksConfig()
	var stdout, stderr bytes.Buffer
	if code := doMaintenanceInstallHooks(cfg, cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("first run failed (%d): %s", code, stderr.String())
	}

	// Snapshot.
	hookContent, _ := os.ReadFile(githooks.CityHookPath(cityPath))
	stubContent, _ := os.ReadFile(filepath.Join(rig, ".githooks", "prepare-commit-msg"))

	stdout.Reset()
	stderr.Reset()
	if code := doMaintenanceInstallHooks(cfg, cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("second run failed (%d): %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "city hook up to date") {
		t.Errorf("expected 'city hook up to date' on second run, got:\n%s", out)
	}
	if !strings.Contains(out, "rig rig: up to date") {
		t.Errorf("expected 'rig rig: up to date' on second run, got:\n%s", out)
	}

	gotHook, _ := os.ReadFile(githooks.CityHookPath(cityPath))
	gotStub, _ := os.ReadFile(filepath.Join(rig, ".githooks", "prepare-commit-msg"))
	if !bytes.Equal(hookContent, gotHook) {
		t.Errorf("city hook content changed across runs")
	}
	if !bytes.Equal(stubContent, gotStub) {
		t.Errorf("rig stub content changed across runs")
	}
}

func TestDoMaintenanceInstallHooks_SkipsNonGitRig(t *testing.T) {
	cityPath := t.TempDir()
	fakeRepo(t, cityPath)
	// rig dir exists but is not a git repo.
	rig := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "not-a-repo"
path = "` + rig + `"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := newFakeMaintHooksConfig()
	var stdout, stderr bytes.Buffer
	if code := doMaintenanceInstallHooks(cfg, cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rig not-a-repo: skipped (not a git repository)") {
		t.Errorf("expected non-git skip message, got stdout:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(rig, ".githooks")); err == nil {
		t.Errorf("expected no .githooks dir in non-git rig")
	}
}

func TestDoMaintenanceInstallHooks_SkipsRigWithoutPath(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "no-path-rig"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := newFakeMaintHooksConfig()
	var stdout, stderr bytes.Buffer
	if code := doMaintenanceInstallHooks(cfg, cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `skipping rig "no-path-rig"`) {
		t.Errorf("expected skip message, got stderr:\n%s", stderr.String())
	}
}

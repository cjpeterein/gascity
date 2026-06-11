package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// gcEnvVars lists the GC_* identity and session-routing variables that
// tests should clear to isolate from host session state (e.g., running
// inside a gc-managed tmux session).
var gcEnvVars = []string{
	"GC_ALIAS",
	"GC_AGENT",
	"GC_BEADS",
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_SESSION_ORIGIN",
	"GC_SHARED_SKILL_CATALOG_SNAPSHOT",
	"GC_TEMPLATE",
	"GC_TMUX_SESSION",
	"GC_CITY",
	"GC_DIR",
}

var liveTestEnvVars = []string{
	"BEADS_ACTOR",
	"BEADS_CREDENTIALS_FILE",
	"BEADS_DB_PATH",
	"BEADS_DIR",
	"BEADS_DOLT_PASSWORD",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"DOLT_CONFIG_PATH",
	"DOLT_ROOT_PATH",
	"GC_BEADS_PREFIX",
	"GC_CITY_RUNTIME_DIR",
	"GC_CONTROL_DISPATCHER_TRACE_DEFAULT",
	"GC_DOLT",
	"GC_DOLT_HOST",
	"GC_DOLT_PASSWORD",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_HOME",
	"GC_INSTANCE_TOKEN",
	"GC_PROVIDER",
	"GC_READY_PROMPT_PREFIX",
	"GC_STARTUP_PROMPT_DELIVERED",
}

// inheritedCityRoutingEnvVars lists GC_* variables that an outer gc-managed
// shell injects to pin commands at a specific city or scope. Unit tests that
// build a fresh temp city must clear them so cityForStoreDir/findCity and the
// scope-aware bead provider resolution actually resolve to the tempdir rather
// than the parent shell's city.
var inheritedCityRoutingEnvVars = []string{
	"GC_CITY_PATH",
	"GC_CITY_ROOT",
	"GC_BEADS_SCOPE_ROOT",
	"GC_DIR",
	"GC_BIN",
	"GC_RIG",
	"GC_RIG_ROOT",
	"GC_CONTINUATION_EPOCH",
	"GC_RUNTIME_EPOCH",
}

// clearGCEnv clears inherited GC, BEADS, and DOLT state for the duration of
// the test, preventing host session state from redirecting temp fixtures into
// live city, rig, or beads stores. GC_HOME is isolated to a temp dir because
// supervisor registry code fails closed when tests leave it empty.
func clearGCEnv(t *testing.T) {
	t.Helper()
	for _, k := range liveEnvKeysForTests() {
		t.Setenv(k, "")
	}
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	// Restore the package-wide managed-dolt default-off the scrub above just
	// erased (see TestMain). Tests that exercise managed-dolt behavior opt in
	// with t.Setenv("GC_DOLT", "") after calling clearGCEnv.
	t.Setenv("GC_DOLT", "skip")
}

func clearProcessLiveEnvForTests() {
	for _, k := range liveEnvKeysForTests() {
		_ = os.Unsetenv(k)
	}
}

// isolateRepoCacheRoot points the machine-local repo cache at a fresh per-test
// directory via GC_REPO_CACHE_ROOT. The package TestMain sets a single GC_HOME
// for the whole process, so any test that stages or materializes a repo cache
// must isolate its own root or it will collide with concurrent tests staging
// the same source+commit (identical content yields an identical cache key). It
// also guarantees no test stamps the shared synthetic-pack marker into the live
// ~/.gc/cache/repos (gc-c7l). Resolve the active root with config.RepoCacheRoot.
func isolateRepoCacheRoot(t *testing.T) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo-cache")
	t.Setenv(config.RepoCacheRootEnv, root)
}

// TestRepoCacheRootNeverResolvesToRealUserHome is the gc-c7l regression guard.
// The package TestMain isolates GC_HOME to a temp dir; config.RepoCacheRoot
// must therefore resolve the machine-local cache under that isolated GC_HOME
// (or an explicit GC_REPO_CACHE_ROOT), never under the real OS user home. If
// this fails, a `go test ./cmd/gc/...` run can stamp the shared synthetic-pack
// marker into the live ~/.gc/cache/repos and brick the running city.
func TestRepoCacheRootNeverResolvesToRealUserHome(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir: %v", err)
	}
	liveCache := filepath.Join(realHome, ".gc", "cache", "repos")

	root, err := config.RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	if root == liveCache {
		t.Fatalf("RepoCacheRoot resolved to the live shared cache %q; the test harness must isolate it via GC_HOME or GC_REPO_CACHE_ROOT", liveCache)
	}
}

// TestPackageDefaultDisablesManagedDolt pins the gc-6ut central guard:
// TestMain exports GC_DOLT=skip for the whole package so a test that reaches
// a store-open or bd transport-recovery path cannot boot a real managed dolt
// server by default. Tests that exercise managed-dolt behavior must opt in
// explicitly with t.Setenv("GC_DOLT", "").
func TestPackageDefaultDisablesManagedDolt(t *testing.T) {
	if os.Getenv("GC_DOLT") != "skip" {
		t.Fatalf("GC_DOLT = %q, want package-wide %q default from TestMain", os.Getenv("GC_DOLT"), "skip")
	}
}

func TestClearGCEnvRestoresManagedDoltSkipDefault(t *testing.T) {
	clearGCEnv(t)
	if os.Getenv("GC_DOLT") != "skip" {
		t.Fatalf("GC_DOLT = %q after clearGCEnv, want restored %q default", os.Getenv("GC_DOLT"), "skip")
	}
}

func TestClearProcessLiveEnvForTestsUnsetsInheritedState(t *testing.T) {
	cleared := []string{
		"BEADS_ACTOR",
		"BEADS_DIR",
		"DOLT_CONFIG_PATH",
		"GC_BEADS",
		"GC_BEADS_SCOPE_ROOT",
		"GC_CITY_PATH",
		"GC_DOLT_HOST",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_SESSION_NAME",
	}
	preserved := []string{
		"GC_FAST_UNIT",
		"GC_REAL_PROCESS_SIGNAL_TESTS",
		"GC_TEST_KEEP",
	}

	for _, key := range append(cleared, preserved...) {
		t.Setenv(key, "from-parent-session")
	}

	clearProcessLiveEnvForTests()

	for _, key := range cleared {
		if value, ok := os.LookupEnv(key); ok {
			t.Errorf("%s survived scrub with value %q", key, value)
		}
	}
	for _, key := range preserved {
		if value := os.Getenv(key); value != "from-parent-session" {
			t.Errorf("%s = %q, want preserved test-control value", key, value)
		}
	}
}

func TestIsTestscriptCommandInvocation(t *testing.T) {
	tests := []struct {
		name string
		arg0 string
		want bool
	}{
		{name: "gc helper", arg0: "/tmp/testscript-main/bin/gc", want: true},
		{name: "bd helper", arg0: "/tmp/testscript-main/bin/bd", want: true},
		{name: "windows gc helper", arg0: `C:\Temp\testscript-main\bin\gc.exe`, want: true},
		{name: "top level test binary", arg0: "/tmp/go-build/cmd/gc.test", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTestscriptCommandInvocation(tt.arg0); got != tt.want {
				t.Fatalf("isTestscriptCommandInvocation(%q) = %v, want %v", tt.arg0, got, tt.want)
			}
		})
	}
}

func liveEnvKeysForTests() []string {
	keys := make(map[string]struct{})
	for _, group := range [][]string{gcEnvVars, inheritedCityRoutingEnvVars, liveTestEnvVars} {
		for _, k := range group {
			if !preserveTestControlEnv(k) {
				keys[k] = struct{}{}
			}
		}
	}
	for _, env := range os.Environ() {
		k, _, ok := strings.Cut(env, "=")
		if !ok || preserveTestControlEnv(k) {
			continue
		}
		if strings.HasPrefix(k, "GC_") || strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "DOLT_") {
			keys[k] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	return ordered
}

func preserveTestControlEnv(key string) bool {
	return key == "GC_FAST_UNIT" ||
		key == "GC_REAL_PROCESS_SIGNAL_TESTS" ||
		key == managedDoltTestModeEnv ||
		key == managedDoltTestParentPIDEnv ||
		key == "GC_DOLT_REAL_BINARY" ||
		strings.HasPrefix(key, "GC_LIVE_") ||
		strings.HasPrefix(key, "GC_SESSION_CHAOS_") ||
		strings.HasPrefix(key, "GC_TEST_")
}

func TestPreserveTestControlEnvKeepsRealProcessSignalGate(t *testing.T) {
	if !preserveTestControlEnv("GC_REAL_PROCESS_SIGNAL_TESTS") {
		t.Fatal("GC_REAL_PROCESS_SIGNAL_TESTS must survive cmd/gc test env scrubbing")
	}
}

// isTestscriptCommandInvocation reports whether this process is a
// testscript-re-executed command (rogpeppe/go-internal/testscript dispatches
// `exec gc` / `exec bd` by re-invoking the test binary with arg0 set to the
// command name). TestMain must skip the live-env scrub in that case so the
// env directives a testscript injects into its subprocess survive.
func isTestscriptCommandInvocation(arg0 string) bool {
	name := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(arg0, "\\", "/")), ".exe")
	return name == "gc" || name == "bd"
}

// clearInheritedCityRoutingEnv unsets only the city-routing env vars listed in
// inheritedCityRoutingEnvVars for tests that need narrower cleanup than
// clearGCEnv.
func clearInheritedCityRoutingEnv(t *testing.T) {
	t.Helper()
	for _, k := range inheritedCityRoutingEnvVars {
		t.Setenv(k, "")
	}
}

func disableManagedDoltRecoveryForTest(t *testing.T) {
	t.Helper()
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
}

// enableManagedDoltForTest opts a test back into the managed-dolt behavior
// that the package-wide GC_DOLT=skip default (set in TestMain, gc-6ut)
// disables. Only tests that assert managed-dolt lifecycle behavior — provider
// start/recover, transport-recovery classification, doctor dolt checks —
// should call this; the spawn-side test watchdog and the TestMain leak guard
// still bound any server such a test boots. Call after clearGCEnv, which
// restores the skip default.
func enableManagedDoltForTest(t *testing.T) {
	t.Helper()
	t.Setenv("GC_DOLT", "")
}

var testProviderStubCommands = []string{
	"claude",
	"codex",
	"gemini",
	"cursor",
	"copilot",
	"amp",
	"opencode",
	"auggie",
	"pi",
	"omp",
}

func installTestProviderStubs() (string, error) {
	dir, err := os.MkdirTemp("", pidPrefixedTempPattern(testProviderStubDirPrefix))
	if err != nil {
		return "", err
	}
	for _, name := range testProviderStubCommands {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

func builtinProviderAliasesForTest(names ...string) map[string]config.ProviderSpec {
	providers := make(map[string]config.ProviderSpec, len(names))
	for _, name := range names {
		providers[name] = config.BuiltinProviderAlias(name)
	}
	return providers
}

func builtinProviderAliasTOMLForTest(names ...string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString("\n[providers.")
		b.WriteString(name)
		b.WriteString("]\nbase = \"builtin:")
		b.WriteString(name)
		b.WriteString("\"\n")
	}
	return b.String()
}

func withBuiltinProviderAliasesTOMLForTest(content string, names ...string) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return strings.TrimLeft(builtinProviderAliasTOMLForTest(names...), "\n")
	}
	return content + "\n" + builtinProviderAliasTOMLForTest(names...)
}

func TestInstallTestProviderStubsUsesPIDPrefixedDir(t *testing.T) {
	dir, err := installTestProviderStubs()
	if err != nil {
		t.Fatalf("installTestProviderStubs: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	pid, ok := pidFromPrefixedDirName(filepath.Base(dir), testProviderStubDirPrefix)
	if !ok {
		t.Fatalf("provider stubs dir %q does not use prefix %q", dir, testProviderStubDirPrefix)
	}
	if pid != os.Getpid() {
		t.Fatalf("provider stubs dir PID = %d, want current PID %d", pid, os.Getpid())
	}
}

func writeTestGitIdentity(homeDir string) error {
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	data := []byte("[user]\n\tname = gc-test\n\temail = gc-test@test.local\n[beads]\n\trole = maintainer\n")
	return os.WriteFile(gitConfig, data, 0o644)
}

// gcBeadsBdTestHomeEnv creates a temp HOME with a .gitconfig containing user
// identity and beads.role = maintainer, then returns extra env entries suitable
// for appending to sanitizedBaseEnv. Use this for any test that runs the real
// gc-beads-bd.sh op_init, which calls ensure_beads_role and requires a writable
// global git config.
func gcBeadsBdTestHomeEnv(t *testing.T) []string {
	t.Helper()
	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(beads-bd test home): %v", err)
	}
	if err := writeTestGitIdentity(homeDir); err != nil {
		t.Fatalf("write test git identity for beads-bd: %v", err)
	}
	return []string{
		"HOME=" + homeDir,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(homeDir, ".gitconfig"),
	}
}

func writeTestDoltIdentity(homeDir string) error {
	doltDir := filepath.Join(homeDir, ".dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		return err
	}
	data := []byte(`{"user.name":"gc-test","user.email":"gc-test@test.local"}`)
	return os.WriteFile(filepath.Join(doltDir, "config_global.json"), data, 0o644)
}

func configureTestDoltIdentityEnv(t *testing.T) {
	t.Helper()

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(test home): %v", err)
	}
	if err := writeTestGitIdentity(homeDir); err != nil {
		t.Fatalf("write test git identity: %v", err)
	}
	if err := writeTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("write test dolt identity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(homeDir, ".gitconfig"))
	t.Setenv("DOLT_ROOT_PATH", homeDir)
}

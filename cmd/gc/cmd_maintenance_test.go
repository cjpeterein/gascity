package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/githooks"
)

// okMaintenanceStatusHandler emits a plausible GET /maintenance/status
// body with one prior run and no in-flight cycle. Used as the
// api-happy-path fixture in the six-row matrix.
func okMaintenanceStatusHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled":          true,
			"interval_seconds": int64(604800),
			"in_flight":        false,
			"last_run": map[string]any{
				"started_at":   "2026-04-22T03:00:00Z",
				"finished_at":  "2026-04-22T03:00:05Z",
				"stage":        "done",
				"before_bytes": int64(11000000000),
				"after_bytes":  int64(2000000000),
				"duration_s":   5.0,
			},
			"history": []map[string]any{
				{
					"started_at":   "2026-04-22T03:00:00Z",
					"finished_at":  "2026-04-22T03:00:05Z",
					"stage":        "done",
					"before_bytes": int64(11000000000),
					"after_bytes":  int64(2000000000),
					"duration_s":   5.0,
				},
			},
		})
	})
}

// okMaintenanceTriggerHandler emits a 202 response body carrying a
// synthetic started_at; the handler is idempotent so every request the
// test makes sees the same body regardless of order.
func okMaintenanceTriggerHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
		})
	})
}

// okMaintenanceTriggerWaitHandler emits the full 202 body for a synchronous
// (?wait=true) call. The run has stage=done so the CLI exits 0.
func okMaintenanceTriggerWaitHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
			"run": map[string]any{
				"started_at":   "2026-04-22T03:00:00Z",
				"finished_at":  "2026-04-22T03:00:05Z",
				"stage":        "done",
				"before_bytes": int64(11000000000),
				"after_bytes":  int64(2000000000),
				"duration_s":   5.0,
			},
		})
	})
}

// maintenanceProblemHandler returns a Huma-style Problem Details body at
// the configured status/detail. Matches the test scaffolding used by the
// mail and order read-path matrices so the assertion helpers stay shared.
func maintenanceProblemHandler(status int, detail string) func(*testing.T) http.Handler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

// assertMaintenanceRouteLog verifies exactly one route=... line with the
// expected shape is present in stderr. Empty wantRoute means "do not assert
// routing" — some rows assert an API error instead.
func assertMaintenanceRouteLog(t *testing.T, stderrStr, wantRoute, wantReason string) {
	t.Helper()
	if wantRoute == "" {
		return
	}
	want := "route=" + wantRoute
	if wantReason != "" {
		want += " reason=" + wantReason
	}
	if !strings.Contains(stderrStr, want) {
		t.Errorf("stderr missing %q:\n%s", want, stderrStr)
	}
	if n := strings.Count(stderrStr, "route="); n != 1 {
		t.Errorf("route=... lines = %d, want 1:\n%s", n, stderrStr)
	}
}

// TestRouteMaintenanceStatus_SixRowMatrix exercises the six mandatory
// rows for `gc maintenance status`. Exit codes diverge from the generic
// mail pattern because no local fallback exists for maintenance reads —
// the in-memory ring buffer lives only in the supervisor process.
// Fallback rows therefore exit 2 (supervisor unreachable) rather than 0.
func TestRouteMaintenanceStatus_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(*testing.T) http.Handler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okMaintenanceStatusHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Maintenance: enabled=yes",
		},
		{
			name:       "api-cache-not-live",
			handler:    maintenanceProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   2, // no local fallback source
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    maintenanceProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   2,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    maintenanceProblemHandler(http.StatusNotFound, "not_found: no such thing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeMaintenanceStatus(c, tc.nilReason, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertMaintenanceRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteMaintenanceDoltGC_SixRowMatrix exercises the six mandatory
// rows for `gc maintenance dolt-gc`. Like the status subcommand there is
// no local fallback; unlike status, this command is a mutator and uses
// ShouldFallback (not ShouldFallbackForRead) so generic 5xx is a hard
// error (exit 1), not a fallback.
func TestRouteMaintenanceDoltGC_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      func(*testing.T) http.Handler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okMaintenanceTriggerHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Maintenance accepted",
		},
		{
			name:       "api-cache-not-live",
			handler:    maintenanceProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   2,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    maintenanceProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   1, // mutation: generic 5xx is a hard error
			wantStderr: "API error",
		},
		{
			name:       "api-404-error",
			handler:    maintenanceProblemHandler(http.StatusNotFound, "not_found: no such thing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     2,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeMaintenanceDoltGC(c, tc.nilReason, false, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertMaintenanceRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

// TestRouteMaintenanceDoltGC_InProgress verifies 409 with
// maintenance-in-progress body maps to exit 3 (bead's documented exit
// code for "already running").
func TestRouteMaintenanceDoltGC_InProgress(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	body := `maintenance-in-progress: {"type":"maintenance-in-progress","started_at":"2026-04-22T03:00:00Z"}`
	srv := httptest.NewServer(maintenanceProblemHandler(http.StatusConflict, body)(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", false, false, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (already running); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already in progress") {
		t.Errorf("stderr missing 'already in progress':\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "2026-04-22T03:00:00Z") {
		t.Errorf("stderr missing in-flight started_at:\n%s", stderr.String())
	}
}

// TestRouteMaintenanceDoltGC_WaitFailure verifies that --wait returning a
// run with stage!='done' maps to exit 1.
func TestRouteMaintenanceDoltGC_WaitFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted":   true,
			"started_at": "2026-04-22T03:00:00Z",
			"run": map[string]any{
				"started_at":  "2026-04-22T03:00:00Z",
				"finished_at": "2026-04-22T03:00:01Z",
				"stage":       "gc",
				"err":         "CALL DOLT_GC(): lock timeout",
				"duration_s":  1.0,
			},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (wait + failure); stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "stage=gc") {
		t.Errorf("stdout missing stage=gc:\n%s", stdout.String())
	}
}

// TestRouteMaintenanceDoltGC_WaitSuccess verifies that --wait returning a
// run with stage='done' maps to exit 0.
func TestRouteMaintenanceDoltGC_WaitSuccess(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceTriggerWaitHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceDoltGC(c, "", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "stage=done") {
		t.Errorf("stdout missing stage=done:\n%s", stdout.String())
	}
}

// TestRouteMaintenanceStatus_JSONOutput verifies that --json emits a
// stable envelope with a _cache_age_s field mirroring the read-path
// contract.
func TestRouteMaintenanceStatus_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(okMaintenanceStatusHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeMaintenanceStatus(c, "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; want 0; stderr=%q", code, stderr.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, stdout.String())
	}
	if _, ok := env["_cache_age_s"]; !ok {
		t.Errorf("JSON envelope missing _cache_age_s: %v", env)
	}
	if _, ok := env["status"]; !ok {
		t.Errorf("JSON envelope missing status: %v", env)
	}
}

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

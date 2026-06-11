package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// writeHookCacheCity writes the minimal city fixture for supervisor-cache
// hook tests and returns (cityDir, bdLogPath). The fake bd on PATH logs
// every invocation so tests can prove whether the work query shelled out.
func writeHookCacheCity(t *testing.T) (string, string) {
	t.Helper()
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	cityDir := t.TempDir()
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBD := filepath.Join(fakeBin, "bd")
	script := fmt.Sprintf(`#!/bin/sh
printf 'args=%%s\n' "$*" >> %q
case "$*" in
  *"--metadata-field gc.routed_to=worker"*) printf '[{"id":"hw-shell","title":"routed work"}]' ;;
  *) printf '[]' ;;
esac
`, logPath)
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_NO_API", "")
	return cityDir, logPath
}

// workQueryBDInvocations filters the fake bd log to work-query invocations
// (list/ready/query), ignoring unrelated bd calls from config plumbing.
func workQueryBDInvocations(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("ReadFile(%s): %v", logPath, err)
	}
	var filtered strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "args=list --status") ||
			strings.Contains(line, "args=ready") ||
			strings.Contains(line, "args=query") {
			filtered.WriteString(line)
			filtered.WriteByte('\n')
		}
	}
	return filtered.String()
}

// hookCacheAPIServer serves the three supervisor-cache reads the hook path
// performs (session beads by label, ready, in_progress list) and records
// whether each carried cached=true.
func hookCacheAPIServer(t *testing.T, sessionBeadsBody, readyBody, listBody string) (*httptest.Server, *map[string]bool) {
	t.Helper()
	sawCached := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/city/test-city/beads/ready", func(w http.ResponseWriter, r *http.Request) {
		sawCached["ready"] = r.URL.Query().Get("cached") == "true"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, readyBody) //nolint:errcheck
	})
	mux.HandleFunc("/v0/city/test-city/beads", func(w http.ResponseWriter, r *http.Request) {
		body := listBody
		if r.URL.Query().Get("label") == "gc:session" {
			body = sessionBeadsBody
			sawCached["session-beads"] = r.URL.Query().Get("cached") == "true"
		} else {
			sawCached["list"] = r.URL.Query().Get("cached") == "true"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sawCached
}

const emptyHookSessionsBody = `{"items":[],"total":0}`

// installSupervisorClientForHook points the supervisor-managed routing at the
// fake API server: controller socket alive, no standalone [api] port.
func installSupervisorClientForHook(t *testing.T, srvURL string) {
	t.Helper()
	origAlive, origSup := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerAliveHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})
	apiRouteControllerAliveHook = func(string) int { return 4242 }
	apiRouteSupervisorClientHook = func(string) *api.Client {
		return api.NewCityScopedClient(srvURL, "test-city")
	}
}

// TestCmdHookServesFromSupervisorCache is the gc-lqt acceptance shape: with a
// supervisor API reachable, `gc hook <agent>` answers from two cached HTTP
// reads and spawns zero bd work-query subprocesses.
func TestCmdHookServesFromSupervisorCache(t *testing.T) {
	_, logPath := writeHookCacheCity(t)
	srv, sawCached := hookCacheAPIServer(t,
		emptyHookSessionsBody,
		`{"items":[{"id":"hw-api","title":"routed work","status":"open","issue_type":"task","created_at":"2026-06-01T00:00:00Z","metadata":{"gc.routed_to":"worker"}}],"total":1}`,
		`{"items":[],"total":0}`,
	)
	installSupervisorClientForHook(t, srv.URL)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"hw-api"`) {
		t.Fatalf("stdout = %q, want routed work from the API", stdout.String())
	}
	if !(*sawCached)["ready"] {
		t.Fatalf("ready read did not request cached=true")
	}
	if !(*sawCached)["list"] {
		t.Fatalf("in_progress read did not request cached=true")
	}
	if shellLog := workQueryBDInvocations(t, logPath); shellLog != "" {
		t.Fatalf("bd work-query subprocesses ran on the cache path:\n%s", shellLog)
	}
}

// TestCmdHookCacheMatchesPoolSessionIdentity proves the cached path
// preserves cliSessionName's semantics without its bd subprocesses: a pool
// agent's work bead is assigned to the suffixed runtime session name, which
// only the supervisor's session projection knows.
func TestCmdHookCacheMatchesPoolSessionIdentity(t *testing.T) {
	_, logPath := writeHookCacheCity(t)
	srv, _ := hookCacheAPIServer(t,
		`{"items":[{"id":"em-i03x","title":"session","status":"open","issue_type":"session","created_at":"2026-06-01T00:00:00Z","labels":["gc:session","agent:worker"],"metadata":{"agent_name":"worker","session_name":"test-city--worker-em-i03x","state":"active"}}],"total":1}`,
		`{"items":[],"total":0}`,
		`{"items":[{"id":"hw-pool","title":"claimed work","status":"in_progress","issue_type":"task","created_at":"2026-06-01T00:00:00Z","assignee":"test-city--worker-em-i03x"}],"total":1}`,
	)
	installSupervisorClientForHook(t, srv.URL)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"hw-pool"`) {
		t.Fatalf("stdout = %q, want the session-name-assigned bead", stdout.String())
	}
	if shellLog := workQueryBDInvocations(t, logPath); shellLog != "" {
		t.Fatalf("bd work-query subprocesses ran on the cache path:\n%s", shellLog)
	}
}

// TestCmdHookFallsBackToShellOnAPIError proves a degraded supervisor never
// hides work: the hook falls back to the shell work query and still finds
// the routed bead.
func TestCmdHookFallsBackToShellOnAPIError(t *testing.T) {
	_, logPath := writeHookCacheCity(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "supervisor exploded", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	installSupervisorClientForHook(t, srv.URL)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"hw-shell"`) {
		t.Fatalf("stdout = %q, want routed work from the shell fallback", stdout.String())
	}
	if shellLog := workQueryBDInvocations(t, logPath); shellLog == "" {
		t.Fatalf("shell fallback did not run bd work queries")
	}
}

// TestCmdHookCustomWorkQueryKeepsShellPath pins the eligibility gate: an
// agent with a caller-owned work_query never routes through the supervisor
// cache, even when the API is reachable.
func TestCmdHookCustomWorkQueryKeepsShellPath(t *testing.T) {
	cityDir, _ := writeHookCacheCity(t)
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
work_query = "printf '[{\"id\":\"hw-custom\"}]'"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, sawCached := hookCacheAPIServer(t, emptyHookSessionsBody, `{"items":[],"total":0}`, `{"items":[],"total":0}`)
	installSupervisorClientForHook(t, srv.URL)

	var stdout, stderr bytes.Buffer
	code := cmdHook([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"hw-custom"`) {
		t.Fatalf("stdout = %q, want custom work query output", stdout.String())
	}
	if len(*sawCached) != 0 {
		t.Fatalf("custom work query touched the supervisor API: %v", *sawCached)
	}
}

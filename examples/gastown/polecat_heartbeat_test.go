package gastown_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// heartbeatScript returns the absolute path to the detector under test.
func heartbeatScript() string {
	return filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "polecat-heartbeat.sh")
}

// writeHeartbeatGCStub installs a gc stub that logs every invocation to
// GC_CALL_LOG and returns session/bead JSON configured via env vars:
//
//	GC_TEST_SESSIONS_JSON  — response body for `gc session list --state=all --json`
//	GC_TEST_WARRANTS_JSON  — response body for `gc bd list --type=warrant ...`
//
// Every `gc bd create ...` call is logged; the stub prints back a minimal
// success payload so the script's FILED counter increments.
func writeHeartbeatGCStub(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
# Log the full argv (space-separated, one call per line) so tests can
# inspect what the heartbeat script asked gc to do.
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  session)
    if [ "$2" = "list" ]; then
      printf '%s\n' "${GC_TEST_SESSIONS_JSON:-[]}"
      exit 0
    fi
    ;;
  bd)
    case "$2" in
      list)
        # Only warrant queries are issued by the script; return whatever the
        # test asked for. Any other list query gets an empty array.
        for arg in "$@"; do
          case "$arg" in
            --type=warrant)
              printf '%s\n' "${GC_TEST_WARRANTS_JSON:-[]}"
              exit 0
              ;;
          esac
        done
        printf '[]\n'
        exit 0
        ;;
      create)
        # Record the create call and exit success so FILED increments.
        exit 0
        ;;
    esac
    ;;
esac
exit 0
`)
}

// rfc3339Offset returns a timestamp N seconds before now in "YYYY-MM-DDTHH:MM:SS-05:00"
// form (local-offset variant), exercising the script's offset parser.
func rfc3339Offset(secondsAgo int) string {
	t := time.Now().Add(-time.Duration(secondsAgo) * time.Second)
	// Force a known offset so tests are portable. -05:00 matches the offset
	// gc emits on macOS/Eastern hosts; the script must handle both Z and
	// numeric offsets.
	loc := time.FixedZone("TEST", -5*60*60)
	return t.In(loc).Format("2006-01-02T15:04:05-07:00")
}

// rfc3339Z returns a UTC timestamp N seconds before now with "Z" suffix.
func rfc3339Z(secondsAgo int) string {
	return time.Now().Add(-time.Duration(secondsAgo) * time.Second).UTC().Format("2006-01-02T15:04:05Z")
}

func heartbeatBaseEnv(t *testing.T, binDir, gcLog string) map[string]string {
	t.Helper()
	return map[string]string{
		"GC_CITY":                            t.TempDir(),
		"GC_CITY_PATH":                       t.TempDir(),
		"GC_CALL_LOG":                        gcLog,
		"POLECAT_HEARTBEAT_THRESHOLD_SECS":   "600",
		"POLECAT_HEARTBEAT_TEMPLATE_PATTERN": "polecat",
		"POLECAT_HEARTBEAT_DOG_ROUTE":        "dog",
		"POLECAT_HEARTBEAT_REQUESTER":        "polecat-heartbeat",
		"PATH":                               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

// TestPolecatHeartbeatFilesWarrantForWedgedPolecat covers the happy path: an
// active polecat session has produced no I/O for longer than the threshold,
// no prior warrant exists for it, and the script files a warrant.
func TestPolecatHeartbeatFilesWarrantForWedgedPolecat(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	wedged := rfc3339Offset(1800) // 30 min ago — well past 10 min threshold
	sessionsJSON := fmt.Sprintf(`[
{"ID":"ci-1","SessionName":"polecat-ci-1","Alias":"gascity/polecat-1","Template":"gascity/polecat","State":"active","LastActive":"%s"}
]`, wedged)

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "polecat-heartbeat: filed 1 warrants") {
		t.Fatalf("expected script to report 1 filed warrant, got:\n%s", out)
	}

	log := readFileString(t, gcLog)
	if !strings.Contains(log, "bd create --type=warrant") {
		t.Fatalf("expected a warrant create call; gc log:\n%s", log)
	}
	// The warrant must carry target + reason + gc.routed_to metadata, and
	// a pool:dog label so shutdown-dance's current dispatch path works.
	for _, want := range []string{
		`"target":"gascity/polecat-1"`,
		`"gc.routed_to":"dog"`,
		`--labels pool:dog`,
		"heartbeat-timeout",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("gc log missing %q:\n%s", want, log)
		}
	}
}

// TestPolecatHeartbeatSkipsFreshPolecat verifies that an active polecat
// whose LastActive is recent (under the threshold) does NOT get a warrant.
// This is the false-positive guard for legitimately-long tool calls —
// anything producing output within the threshold is healthy.
func TestPolecatHeartbeatSkipsFreshPolecat(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	fresh := rfc3339Z(60) // 60s ago — well under 10 min threshold
	sessionsJSON := fmt.Sprintf(`[
{"ID":"ci-1","SessionName":"polecat-ci-1","Alias":"gascity/polecat-1","Template":"gascity/polecat","State":"active","LastActive":"%s"}
]`, fresh)

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "filed") {
		t.Fatalf("expected no warrants filed for fresh polecat, got:\n%s", out)
	}
	log := readFileString(t, gcLog)
	if strings.Contains(log, "bd create") {
		t.Fatalf("fresh polecat triggered a warrant create:\n%s", log)
	}
}

// TestPolecatHeartbeatSkipsZeroLastActive verifies that a session whose
// LastActive is the zero time (`0001-01-01T...`) — reported when the runtime
// provider can't supply activity data — is NOT warranted. Without a signal
// we don't know the session is wedged.
func TestPolecatHeartbeatSkipsZeroLastActive(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	sessionsJSON := `[
{"ID":"ci-1","SessionName":"polecat-ci-1","Alias":"gascity/polecat-1","Template":"gascity/polecat","State":"active","LastActive":"0001-01-01T00:00:00Z"}
]`
	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "filed") {
		t.Fatalf("expected no warrants for zero LastActive, got:\n%s", out)
	}
	log := readFileString(t, gcLog)
	if strings.Contains(log, "bd create") {
		t.Fatalf("zero-LastActive polecat triggered a warrant create:\n%s", log)
	}
}

// TestPolecatHeartbeatSkipsNonPolecatTemplate verifies that a long-idle
// session whose template doesn't match the polecat pattern (e.g. witness,
// refinery) is NOT warranted. The heartbeat detector is polecat-scoped —
// coordination agents have their own patrol health paths.
func TestPolecatHeartbeatSkipsNonPolecatTemplate(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	wedged := rfc3339Z(1800)
	sessionsJSON := fmt.Sprintf(`[
{"ID":"ci-w","SessionName":"witness-ci-w","Alias":"gascity/witness","Template":"gascity/witness","State":"active","LastActive":"%s"},
{"ID":"ci-r","SessionName":"refinery-ci-r","Alias":"gascity/refinery","Template":"gascity/refinery","State":"active","LastActive":"%s"}
]`, wedged, wedged)

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "filed") {
		t.Fatalf("non-polecat sessions triggered filed output:\n%s", out)
	}
	log := readFileString(t, gcLog)
	if strings.Contains(log, "bd create") {
		t.Fatalf("non-polecat session triggered a warrant create:\n%s", log)
	}
}

// TestPolecatHeartbeatIdempotentWithExistingWarrant verifies that if an
// open warrant already exists for the target, no duplicate is filed. The
// shutdown-dance formula takes up to 7 minutes to run — the 5-minute
// cooldown timer must not stack warrants on the same stuck session.
func TestPolecatHeartbeatIdempotentWithExistingWarrant(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	wedged := rfc3339Z(1800)
	sessionsJSON := fmt.Sprintf(`[
{"ID":"ci-1","SessionName":"polecat-ci-1","Alias":"gascity/polecat-1","Template":"gascity/polecat","State":"active","LastActive":"%s"}
]`, wedged)

	// Warrant already open targeting gascity/polecat-1 — the dog pool is
	// presumably already dancing on it. No new warrant should be filed.
	warrantsJSON := `[
{"id":"ga-w1","status":"open","issue_type":"warrant","metadata":{"target":"gascity/polecat-1","reason":"heartbeat-timeout: 650s without I/O"}}
]`

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = warrantsJSON

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "filed") {
		t.Fatalf("duplicate warrant filed despite existing open warrant:\n%s", out)
	}
	log := readFileString(t, gcLog)
	if strings.Contains(log, "bd create") {
		t.Fatalf("duplicate bd create issued; gc log:\n%s", log)
	}
}

// TestPolecatHeartbeatSkipsNonActiveSession verifies that a session in a
// non-active state (creating, drained, suspended) is NOT warranted even
// when LastActive is stale. Those states belong to the controller and
// are not legitimate wedge indicators.
func TestPolecatHeartbeatSkipsNonActiveSession(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	wedged := rfc3339Z(1800)
	sessionsJSON := fmt.Sprintf(`[
{"ID":"ci-c","SessionName":"polecat-ci-c","Alias":"gascity/polecat-2","Template":"gascity/polecat","State":"creating","LastActive":"%s"},
{"ID":"ci-d","SessionName":"polecat-ci-d","Alias":"gascity/polecat-3","Template":"gascity/polecat","State":"drained","LastActive":"%s"}
]`, wedged, wedged)

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "filed") {
		t.Fatalf("non-active sessions produced filed output:\n%s", out)
	}
	log := readFileString(t, gcLog)
	if strings.Contains(log, "bd create") {
		t.Fatalf("non-active session triggered a warrant create:\n%s", log)
	}
}

// TestPolecatHeartbeatEmptySessionListIsNoop verifies that when no sessions
// exist the script exits quietly without hitting the warrant query — a
// quiet no-op is the correct behavior for an idle city.
func TestPolecatHeartbeatEmptySessionListIsNoop(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["GC_TEST_SESSIONS_JSON"] = "[]"
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed on empty city: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "filed") {
		t.Fatalf("empty city produced filed output:\n%s", out)
	}
}

// TestPolecatHeartbeatHonorsThresholdOverride verifies the tunable
// threshold env var: with a 30-second threshold, a polecat idle for 120s
// must be warranted. Catches regressions where the override isn't
// threaded through to the jq filter.
func TestPolecatHeartbeatHonorsThresholdOverride(t *testing.T) {
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	writeHeartbeatGCStub(t, binDir)

	wedged := rfc3339Z(120)
	sessionsJSON := fmt.Sprintf(`[
{"ID":"ci-1","SessionName":"polecat-ci-1","Alias":"gascity/polecat-1","Template":"gascity/polecat","State":"active","LastActive":"%s"}
]`, wedged)

	env := heartbeatBaseEnv(t, binDir, gcLog)
	env["POLECAT_HEARTBEAT_THRESHOLD_SECS"] = "30"
	env["GC_TEST_SESSIONS_JSON"] = sessionsJSON
	env["GC_TEST_WARRANTS_JSON"] = "[]"

	out, err := runScriptResult(t, heartbeatScript(), env)
	if err != nil {
		t.Fatalf("polecat-heartbeat.sh failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "filed 1 warrants") {
		t.Fatalf("expected 1 warrant filed with 30s threshold, got:\n%s", out)
	}
}

// TestPolecatHeartbeatOrderConfigPointsAtScript sanity-checks that the
// order TOML references the script this package ships. Protects against
// a rename on one side without the other.
func TestPolecatHeartbeatOrderConfigPointsAtScript(t *testing.T) {
	orderPath := filepath.Join(exampleDir(), "packs", "maintenance", "orders", "polecat-heartbeat.toml")
	data, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatalf("ReadFile(order): %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "polecat-heartbeat.sh") {
		t.Fatalf("order does not reference polecat-heartbeat.sh:\n%s", text)
	}
	if !strings.Contains(text, `trigger = "cooldown"`) {
		t.Fatalf("order should trigger on cooldown (timer):\n%s", text)
	}
	if !strings.Contains(text, "interval") {
		t.Fatalf("order should define an interval:\n%s", text)
	}
	scriptPath := heartbeatScript()
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("Stat(script): %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("polecat-heartbeat.sh is not executable: mode=%v", info.Mode())
	}
	// Spot-check that the script is actually runnable.
	if err := exec.Command(scriptPath, "--help").Run(); err != nil {
		// No --help flag is supported, but the script should not return a
		// missing-file error. A non-zero exit from the script itself is
		// fine; ENOENT or permission errors are not.
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() < 0 {
			t.Fatalf("script not executable via exec: %v", err)
		}
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

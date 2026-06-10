//go:build integration

package integration

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// These tests cover the gc-46q contract: the tmux status render path must
// not spawn processes. tmux-theme.sh points status-right at the
// @gc_status_line session option, and status-updater.sh is the session_live
// sidecar that refreshes the option off the render path.

// statusUpdaterScriptPath returns the path to the gastown pack's
// status-updater.sh sidecar under test.
func statusUpdaterScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(gastownPackDir(t), "assets", "scripts", "status-updater.sh")
}

// tmuxThemeScriptPath returns the path to the gastown pack's tmux-theme.sh.
func tmuxThemeScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(gastownPackDir(t), "assets", "scripts", "tmux-theme.sh")
}

// gastownPackDir is the pack config dir scripts receive as ConfigDir.
func gastownPackDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown")
}

// writeFakeGCForUpdater writes a fake `gc` that serves `gc hook <agent>`
// and `gc mail count <agent> --json`, and appends one line per invocation
// to the file named by $FAKE_GC_LOG so tests can count data-production
// ticks.
func writeFakeGCForUpdater(t *testing.T, path, hookOutput, mailCountJSON string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

if [ -n "${FAKE_GC_LOG:-}" ]; then
  printf '%%s\n' "$*" >> "$FAKE_GC_LOG"
fi

case "$1" in
  hook)
    printf '%%s' %q
    ;;
  mail)
    # real CLI: "gc mail count <agent> --json"
    printf '%%s' %q
    ;;
  *)
    echo "unexpected gc command: $1" >&2
    exit 1
    ;;
esac
`, hookOutput, mailCountJSON)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gc command: %v", err)
	}
}

// updaterTestEnv builds the environment for invoking the updater (and the
// daemon it forks): fake gc first on PATH, the guard's tmux socket exposed
// as GC_TMUX_SOCKET, and city-path vars dropped so status-line.sh's
// optional _bd_trace.sh sourcing stays a no-op.
func updaterTestEnv(t *testing.T, guard *tmuxtest.Guard, fakeDir, fakeLog string) []string {
	t.Helper()

	env := newIsolatedToolEnv(t, false)
	envMap := parseEnvList(env)
	env = replaceEnv(env, "PATH", prependPath(fakeDir, envMap["PATH"]))
	env = filterEnvMany(env, "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT")
	env = append(env, "GC_TMUX_SOCKET="+guard.SocketName())
	if fakeLog != "" {
		env = append(env, "FAKE_GC_LOG="+fakeLog)
	}
	return env
}

// startDetachedSession creates a detached long-lived session on the guard's
// socket and returns its name.
func startDetachedSession(t *testing.T, guard *tmuxtest.Guard, name string) string {
	t.Helper()

	out, err := runCommand("", os.Environ(), 10*time.Second,
		"tmux", "-L", guard.SocketName(), "new-session", "-d", "-s", name, "sleep", "600")
	if err != nil {
		t.Fatalf("creating tmux session: %v\noutput: %s", err, out)
	}
	return name
}

// sessionOption reads a session option value (empty string when unset).
func sessionOption(t *testing.T, guard *tmuxtest.Guard, session, option string) string {
	t.Helper()

	out, err := runCommand("", os.Environ(), 10*time.Second,
		"tmux", "-L", guard.SocketName(), "show-options", "-t", session, "-qv", option)
	if err != nil {
		t.Fatalf("reading %s: %v\noutput: %s", option, err, out)
	}
	return strings.TrimSpace(out)
}

// runUpdater invokes the status-updater.sh launcher for the session.
func runUpdater(t *testing.T, env []string, session, agent string, interval int) {
	t.Helper()

	out, err := runCommand("", env, 30*time.Second, "sh", statusUpdaterScriptPath(t),
		session, agent, gastownPackDir(t), strconv.Itoa(interval))
	if err != nil {
		t.Fatalf("status-updater.sh failed: %v\noutput: %s", err, out)
	}
}

// waitForOptionValue polls a session option until it equals want.
func waitForOptionValue(t *testing.T, guard *tmuxtest.Guard, session, option, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = sessionOption(t, guard, session, option)
		if last == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never reached %q (last value %q)", option, want, last)
}

// updaterPID reads and parses the daemon PID the updater records on the
// session.
func updaterPID(t *testing.T, guard *tmuxtest.Guard, session string) int {
	t.Helper()

	raw := sessionOption(t, guard, session, "@gc_status_updater_pid")
	if raw == "" {
		return 0
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("@gc_status_updater_pid is not a pid: %q", raw)
	}
	return pid
}

// waitForUpdaterPID polls until the updater has recorded a live daemon PID.
func waitForUpdaterPID(t *testing.T, guard *tmuxtest.Guard, session string, timeout time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := updaterPID(t, guard, session); pid > 0 && processAlive(pid) {
			return pid
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("updater never recorded a live @gc_status_updater_pid")
	return 0
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func killUpdaterOnCleanup(t *testing.T, pid int) {
	t.Cleanup(func() {
		if pid > 0 && processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// countFakeGCHookCalls counts `gc hook` invocations recorded by the fake.
func countFakeGCHookCalls(t *testing.T, fakeLog string) int {
	t.Helper()

	data, err := os.ReadFile(fakeLog)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading fake gc log: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "hook ") || line == "hook" {
			count++
		}
	}
	return count
}

// TestStatusUpdaterPublishesStatusVar proves the sidecar produces the same
// text status-line.sh renders and publishes it to @gc_status_line.
func TestStatusUpdaterPublishesStatusVar(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	session := startDetachedSession(t, guard, guard.CityName()+"-updater")

	fakeDir := t.TempDir()
	writeFakeGCForUpdater(t, filepath.Join(fakeDir, "gc"),
		`[{"id":"a"},{"id":"b"}]`, `{"total":7,"unread":5}`)
	env := updaterTestEnv(t, guard, fakeDir, "")

	runUpdater(t, env, session, "witness", 1)
	pid := waitForUpdaterPID(t, guard, session, 10*time.Second)
	killUpdaterOnCleanup(t, pid)

	waitForOptionValue(t, guard, session, "@gc_status_line",
		"witness | 🪝 2 | 📬 5", 15*time.Second)
}

// TestStatusUpdaterDedup proves re-applying session_live (the reconciler
// does this on drift) does not stack a second daemon: the recorded PID
// survives a second launcher invocation.
func TestStatusUpdaterDedup(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	session := startDetachedSession(t, guard, guard.CityName()+"-dedup")

	fakeDir := t.TempDir()
	writeFakeGCForUpdater(t, filepath.Join(fakeDir, "gc"), `[]`, `{"total":0,"unread":0}`)
	env := updaterTestEnv(t, guard, fakeDir, "")

	runUpdater(t, env, session, "witness", 1)
	pid := waitForUpdaterPID(t, guard, session, 10*time.Second)
	killUpdaterOnCleanup(t, pid)

	runUpdater(t, env, session, "witness", 1)
	// The second launcher must observe the live daemon and leave it alone.
	// Give a would-be duplicate time to claim the slot, then confirm the
	// original still owns it.
	time.Sleep(3 * time.Second)
	if got := updaterPID(t, guard, session); got != pid {
		t.Fatalf("second launcher replaced the daemon: pid %d -> %d", pid, got)
	}
	if !processAlive(pid) {
		t.Fatalf("original daemon (pid %d) died after relaunch", pid)
	}
}

// TestStatusUpdaterExitsWhenSessionDies proves the sidecar reaps itself
// when its session disappears — no orphan loops after gc stop.
func TestStatusUpdaterExitsWhenSessionDies(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	session := startDetachedSession(t, guard, guard.CityName()+"-reap")

	fakeDir := t.TempDir()
	writeFakeGCForUpdater(t, filepath.Join(fakeDir, "gc"), `[]`, `{"total":0,"unread":0}`)
	env := updaterTestEnv(t, guard, fakeDir, "")

	runUpdater(t, env, session, "witness", 1)
	pid := waitForUpdaterPID(t, guard, session, 10*time.Second)
	killUpdaterOnCleanup(t, pid)

	if out, err := runCommand("", os.Environ(), 10*time.Second,
		"tmux", "-L", guard.SocketName(), "kill-session", "-t", session); err != nil {
		t.Fatalf("killing session: %v\noutput: %s", err, out)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("updater daemon (pid %d) survived its session", pid)
}

// TestStatusUpdaterIdlesWhileDetached proves the sidecar does not produce
// data while no client is attached (beyond the initial seed tick): a
// detached idle town must contribute no recurring bd/dolt load.
func TestStatusUpdaterIdlesWhileDetached(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	session := startDetachedSession(t, guard, guard.CityName()+"-idle")

	fakeDir := t.TempDir()
	fakeLog := filepath.Join(t.TempDir(), "gc-calls.log")
	writeFakeGCForUpdater(t, filepath.Join(fakeDir, "gc"), `[]`, `{"total":0,"unread":0}`)
	env := updaterTestEnv(t, guard, fakeDir, fakeLog)

	runUpdater(t, env, session, "witness", 1)
	pid := waitForUpdaterPID(t, guard, session, 10*time.Second)
	killUpdaterOnCleanup(t, pid)

	// Allow the seed tick to land, then confirm no further production.
	waitForOptionValue(t, guard, session, "@gc_status_line", "witness", 15*time.Second)
	base := countFakeGCHookCalls(t, fakeLog)
	if base < 1 {
		t.Fatalf("expected at least the seed tick, got %d hook calls", base)
	}
	time.Sleep(4 * time.Second)
	if got := countFakeGCHookCalls(t, fakeLog); got != base {
		t.Fatalf("detached session kept producing: hook calls %d -> %d", base, got)
	}
}

// TestStatusUpdaterRefreshesWhileAttached proves the acceptance freshness
// bound: with a client attached, a hook-state change reaches
// @gc_status_line within a few update intervals.
func TestStatusUpdaterRefreshesWhileAttached(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	session := startDetachedSession(t, guard, guard.CityName()+"-attach")

	fakeDir := t.TempDir()
	fakeGC := filepath.Join(fakeDir, "gc")
	writeFakeGCForUpdater(t, fakeGC, `[]`, `{"total":0,"unread":0}`)
	env := updaterTestEnv(t, guard, fakeDir, "")

	runUpdater(t, env, session, "witness", 1)
	pid := waitForUpdaterPID(t, guard, session, 10*time.Second)
	killUpdaterOnCleanup(t, pid)
	waitForOptionValue(t, guard, session, "@gc_status_line", "witness", 15*time.Second)

	attachControlClient(t, guard, session)

	// The daemon execs the fake `gc` fresh on every tick, so rewriting the
	// script changes what the next tick observes.
	writeFakeGCForUpdater(t, fakeGC, `[{"id":"a"}]`, `{"total":0,"unread":0}`)
	waitForOptionValue(t, guard, session, "@gc_status_line", "witness | 🪝 1", 15*time.Second)
}

// attachControlClient attaches a control-mode tmux client to the session
// (counts toward #{session_attached}) and detaches it on cleanup.
func attachControlClient(t *testing.T, guard *tmuxtest.Guard, session string) {
	t.Helper()

	cmd := exec.Command("tmux", "-C", "-L", guard.SocketName(), "attach-session", "-t", session)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("control client stdin: %v", err)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting control client: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := runCommand("", os.Environ(), 10*time.Second,
			"tmux", "-L", guard.SocketName(), "display-message", "-p", "-t", session, "#{session_attached}")
		if err == nil && strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("control client never registered as attached")
}

// TestThemeRenderPathHasNoShellouts proves the render path is O(1): after
// theming, status-right reads the @gc_status_line option and contains no
// #() command substitution, and the option is seeded with the agent name
// so the bar is never blank before the first updater tick.
func TestThemeRenderPathHasNoShellouts(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	session := startDetachedSession(t, guard, guard.CityName()+"-theme")

	env := updaterTestEnv(t, guard, t.TempDir(), "")
	out, err := runCommand("", env, 30*time.Second, "sh", tmuxThemeScriptPath(t),
		session, "witness", gastownPackDir(t))
	if err != nil {
		t.Fatalf("tmux-theme.sh failed: %v\noutput: %s", err, out)
	}

	right, err := runCommand("", os.Environ(), 10*time.Second,
		"tmux", "-L", guard.SocketName(), "show-options", "-t", session, "-v", "status-right")
	if err != nil {
		t.Fatalf("reading status-right: %v\noutput: %s", err, right)
	}
	if strings.Contains(right, "#(") {
		t.Fatalf("status-right still shells out per render: %q", right)
	}
	if !strings.Contains(right, "#{@gc_status_line}") {
		t.Fatalf("status-right does not read @gc_status_line: %q", right)
	}
	if got := sessionOption(t, guard, session, "@gc_status_line"); got != "witness" {
		t.Fatalf("@gc_status_line seed = %q, want %q", got, "witness")
	}

	// Re-theming must not clobber updater-produced content.
	if out, err := runCommand("", os.Environ(), 10*time.Second,
		"tmux", "-L", guard.SocketName(), "set-option", "-t", session,
		"@gc_status_line", "witness | 🪝 1"); err != nil {
		t.Fatalf("setting @gc_status_line: %v\noutput: %s", err, out)
	}
	out, err = runCommand("", env, 30*time.Second, "sh", tmuxThemeScriptPath(t),
		session, "witness", gastownPackDir(t))
	if err != nil {
		t.Fatalf("re-running tmux-theme.sh: %v\noutput: %s", err, out)
	}
	if got := sessionOption(t, guard, session, "@gc_status_line"); got != "witness | 🪝 1" {
		t.Fatalf("re-theming clobbered @gc_status_line: %q", got)
	}
}

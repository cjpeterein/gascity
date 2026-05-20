package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// snapshotPreexistingDoltPIDs returns the set of PIDs already running as
// `dolt sql-server` when the test binary starts. Paired with
// reapLeakedTestDoltProcesses at TestMain teardown: PIDs in the snapshot
// are excluded from the reap so the suite never touches a production
// dolt server (for example the dev environment's city dolt) that happens
// to be running on the host.
//
// A /proc walk failure degrades to an empty set — no information means
// the reaper will treat every test-path dolt it sees post-run as a leak
// (fail-safe for detection) rather than masking leaks with a partial
// snapshot.
func snapshotPreexistingDoltPIDs() map[int]bool {
	procs, err := discoverDoltProcesses()
	if err != nil || procs == nil {
		return map[int]bool{}
	}
	out := make(map[int]bool, len(procs))
	for _, p := range procs {
		out[p.PID] = true
	}
	return out
}

// reapLeakedTestDoltProcesses scans for `dolt sql-server` processes whose
// --config path is on the test-config-path allowlist (test TempDirs and
// known Gas City unit-test prefixes) and whose PID was not present in
// the pre-snapshot. Each leaked process is SIGTERM'd, then SIGKILL'd
// after a brief grace period. Returns the count of reaped PIDs so
// TestMain can fail the suite when tests leak.
func reapLeakedTestDoltProcesses(preexisting map[int]bool, w io.Writer) int {
	return reapLeakedTestDoltProcessesWith(
		preexisting,
		discoverDoltProcesses,
		killProcess,
		time.Sleep,
		w,
	)
}

// reapLeakedTestDoltProcessesWith is the injectable form of
// reapLeakedTestDoltProcesses. Tests pass scripted enumerate/kill/sleep
// callbacks so the reaper's decision path can be exercised without
// spawning or signaling real processes.
func reapLeakedTestDoltProcessesWith(
	preexisting map[int]bool,
	enumerate func() ([]DoltProcInfo, error),
	kill func(int, syscall.Signal) error,
	sleep func(time.Duration),
	w io.Writer,
) int {
	procs, err := enumerate()
	if err != nil {
		fmt.Fprintf(w, "TestMain dolt leak reaper: discoverDoltProcesses: %v\n", err) //nolint:errcheck // best-effort stderr
		return 0
	}
	homeDir, _ := os.UserHomeDir()
	tempDir := os.TempDir()

	var leaked []DoltProcInfo
	for _, p := range procs {
		if preexisting[p.PID] {
			continue
		}
		cfg := extractConfigPath(p.Argv)
		if !isTestConfigPath(cfg, homeDir, tempDir) {
			continue
		}
		leaked = append(leaked, p)
	}
	if len(leaked) == 0 {
		return 0
	}
	sort.Slice(leaked, func(i, j int) bool {
		return leaked[i].PID < leaked[j].PID
	})

	for _, p := range leaked {
		if err := kill(p.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			fmt.Fprintf(w, "TestMain dolt leak reaper: SIGTERM pid=%d: %v\n", p.PID, err) //nolint:errcheck // best-effort stderr
		}
	}
	sleep(500 * time.Millisecond)
	for _, p := range leaked {
		if err := kill(p.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			fmt.Fprintf(w, "TestMain dolt leak reaper: SIGKILL pid=%d: %v\n", p.PID, err) //nolint:errcheck // best-effort stderr
		}
	}
	fmt.Fprintf(w, "TestMain dolt leak reaper: reaped %d leaked dolt sql-server process(es):\n", len(leaked)) //nolint:errcheck // best-effort stderr
	for _, p := range leaked {
		fmt.Fprintf(w, "  pid=%d argv=%q\n", p.PID, strings.Join(p.Argv, " ")) //nolint:errcheck // best-effort stderr
	}
	return len(leaked)
}

// doltReapingTestingM wraps *testing.M so the TestMain wrapper can
// snapshot pre-existing dolt PIDs before m.Run and reap any new
// test-owned dolt processes after m.Run returns. Exit-code policy: if
// tests leaked a dolt process, force a non-zero exit even when every
// individual test passed — a silent leak would re-expand to the 9 GiB
// OOM incident the per-test helper was written to catch (gc-ucmw).
type doltReapingTestingM struct {
	inner TestingMRunner
	w     io.Writer
}

// TestingMRunner matches the single Run() int method on *testing.M and
// testscript.TestingM. Wrapping through this interface keeps the reaper
// orthogonal to whichever runner is in use.
type TestingMRunner interface {
	Run() int
}

// Run snapshots pre-existing dolt PIDs, delegates to the underlying
// TestingM, then reaps any new test-owned dolt processes. A leak
// forces a non-zero exit so CI will catch regressions instead of
// silently growing RSS on the host.
func (d doltReapingTestingM) Run() int {
	pre := snapshotPreexistingDoltPIDs()
	code := d.inner.Run()
	leaked := reapLeakedTestDoltProcesses(pre, d.w)
	if leaked > 0 && code == 0 {
		code = 1
	}
	return code
}

// fakeTestingM is a TestingMRunner whose Run() invokes a scripted
// function. Used by the TestMain-wrapper unit tests.
type fakeTestingM struct {
	run func() int
}

func (f fakeTestingM) Run() int {
	if f.run == nil {
		return 0
	}
	return f.run()
}

func TestSnapshotPreexistingDoltPIDs_EmptyOnNoProcs(t *testing.T) {
	// On hosts where /proc lacks any dolt sql-server, the snapshot must be
	// empty so the reaper flags every post-run dolt as a leak.
	got := snapshotPreexistingDoltPIDs()
	if got == nil {
		t.Fatal("snapshotPreexistingDoltPIDs returned nil map; expected empty map")
	}
}

func TestReapLeakedTestDoltProcesses_NoLeaksReturnsZero(t *testing.T) {
	buf := &bytes.Buffer{}
	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{},
		func() ([]DoltProcInfo, error) { return nil, nil },
		func(int, syscall.Signal) error {
			t.Fatal("kill must not run when no leaks are found")
			return nil
		},
		func(time.Duration) {},
		buf,
	)
	if got != 0 {
		t.Fatalf("reap count = %d, want 0", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestReapLeakedTestDoltProcesses_PreexistingPIDsNotKilled(t *testing.T) {
	pid := 4242
	proc := doltTestProc(pid)
	buf := &bytes.Buffer{}
	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{pid: true},
		func() ([]DoltProcInfo, error) { return []DoltProcInfo{proc}, nil },
		func(int, syscall.Signal) error {
			t.Fatalf("kill called for pre-existing pid %d; must be skipped", pid)
			return nil
		},
		func(time.Duration) {},
		buf,
	)
	if got != 0 {
		t.Fatalf("reap count = %d, want 0 (pre-existing PID must be excluded)", got)
	}
}

func TestReapLeakedTestDoltProcesses_NewTestPIDKilledWithTERMThenKILL(t *testing.T) {
	leaked := doltTestProc(7777)
	var killOrder []string
	slept := false
	buf := &bytes.Buffer{}

	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{},
		func() ([]DoltProcInfo, error) { return []DoltProcInfo{leaked}, nil },
		func(pid int, sig syscall.Signal) error {
			killOrder = append(killOrder, fmt.Sprintf("%d:%d", pid, sig))
			return nil
		},
		func(time.Duration) { slept = true },
		buf,
	)
	if got != 1 {
		t.Fatalf("reap count = %d, want 1", got)
	}
	wantOrder := []string{
		fmt.Sprintf("7777:%d", syscall.SIGTERM),
		fmt.Sprintf("7777:%d", syscall.SIGKILL),
	}
	if len(killOrder) != len(wantOrder) {
		t.Fatalf("killOrder = %v, want %v", killOrder, wantOrder)
	}
	for i, k := range killOrder {
		if k != wantOrder[i] {
			t.Fatalf("kill[%d] = %q, want %q", i, k, wantOrder[i])
		}
	}
	if !slept {
		t.Fatal("reaper must sleep between SIGTERM and SIGKILL to give the process a chance to exit gracefully")
	}
	if !strings.Contains(buf.String(), "7777") {
		t.Fatalf("report missing leaked PID 7777: %q", buf.String())
	}
}

func TestReapLeakedTestDoltProcesses_NonTestPathIgnored(t *testing.T) {
	unrelated := DoltProcInfo{
		PID: 9001,
		Argv: []string{
			"dolt",
			"sql-server",
			"--config=/data/projects/production-city/.gc/runtime/packs/dolt/dolt-config.yaml",
		},
	}
	buf := &bytes.Buffer{}
	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{},
		func() ([]DoltProcInfo, error) { return []DoltProcInfo{unrelated}, nil },
		func(int, syscall.Signal) error {
			t.Fatalf("kill called for unrelated production dolt pid %d; must be skipped", unrelated.PID)
			return nil
		},
		func(time.Duration) {},
		buf,
	)
	if got != 0 {
		t.Fatalf("reap count = %d, want 0 (non-test config path must be excluded)", got)
	}
}

func TestReapLeakedTestDoltProcesses_ReportsLeakedPIDsSortedAscending(t *testing.T) {
	hi := doltTestProc(22222, "--port=3400")
	lo := doltTestProc(11111, "--port=3399")
	buf := &bytes.Buffer{}

	var sigTermOrder []int
	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{},
		func() ([]DoltProcInfo, error) { return []DoltProcInfo{hi, lo}, nil },
		func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				sigTermOrder = append(sigTermOrder, pid)
			}
			return nil
		},
		func(time.Duration) {},
		buf,
	)
	if got != 2 {
		t.Fatalf("reap count = %d, want 2", got)
	}
	if len(sigTermOrder) != 2 || sigTermOrder[0] != 11111 || sigTermOrder[1] != 22222 {
		t.Fatalf("sigTermOrder = %v, want [11111 22222] (ascending)", sigTermOrder)
	}
}

func TestReapLeakedTestDoltProcesses_EnumerationErrorLogged(t *testing.T) {
	boom := errors.New("enumerate boom")
	buf := &bytes.Buffer{}
	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{},
		func() ([]DoltProcInfo, error) { return nil, boom },
		func(int, syscall.Signal) error {
			t.Fatal("kill must not run when enumeration fails")
			return nil
		},
		func(time.Duration) {},
		buf,
	)
	if got != 0 {
		t.Fatalf("reap count = %d, want 0 on enumeration error", got)
	}
	if !strings.Contains(buf.String(), boom.Error()) {
		t.Fatalf("error not reported in output: %q", buf.String())
	}
}

func TestReapLeakedTestDoltProcesses_SigTermESRCHNotLogged(t *testing.T) {
	// A race where the process exits between enumeration and SIGTERM
	// returns ESRCH; that is the expected "already gone" case and must
	// not be logged as an error. SIGKILL is still attempted for symmetry;
	// its ESRCH is also benign.
	leaked := doltTestProc(33333)
	buf := &bytes.Buffer{}

	got := reapLeakedTestDoltProcessesWith(
		map[int]bool{},
		func() ([]DoltProcInfo, error) { return []DoltProcInfo{leaked}, nil },
		func(int, syscall.Signal) error { return syscall.ESRCH },
		func(time.Duration) {},
		buf,
	)
	if got != 1 {
		t.Fatalf("reap count = %d, want 1 (process still counts as leaked even if already gone)", got)
	}
	if strings.Contains(buf.String(), "SIGTERM") && strings.Contains(buf.String(), "no such process") {
		t.Fatalf("ESRCH from SIGTERM must not be logged as error: %q", buf.String())
	}
}

func TestDoltReapingTestingM_DelegatesExitCodeWhenNoLeaks(t *testing.T) {
	called := atomic.Int32{}
	inner := fakeTestingM{
		run: func() int {
			called.Add(1)
			return 42
		},
	}
	d := doltReapingTestingM{inner: inner, w: io.Discard}
	got := d.Run()
	if got != 42 {
		t.Fatalf("Run() = %d, want 42 (inner exit code preserved when no leak)", got)
	}
	if called.Load() != 1 {
		t.Fatalf("inner.Run called %d times, want 1", called.Load())
	}
}

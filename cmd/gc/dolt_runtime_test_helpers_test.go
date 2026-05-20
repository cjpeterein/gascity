package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// managedDoltLayoutEnvOverrides lists the env vars that
// resolveManagedDoltRuntimeLayout honors. Tests that derive a layout from
// t.TempDir() and then write fixtures under layout.DataDir must scrub these
// first so an ambient override (e.g., set by `gc prime` in the calling shell)
// cannot redirect the writes into the live data dir.
var managedDoltLayoutEnvOverrides = []string{
	"GC_DOLT_DATA_DIR",
	"GC_DOLT_LOG_FILE",
	"GC_DOLT_STATE_FILE",
	"GC_DOLT_PID_FILE",
	"GC_DOLT_LOCK_FILE",
	"GC_DOLT_CONFIG_FILE",
	"GC_PACK_STATE_DIR",
	"GC_CITY_RUNTIME_DIR",
}

// scrubManagedDoltLayoutEnv unsets every env var that
// resolveManagedDoltRuntimeLayout honors, scoped to the test via t.Setenv.
// Use it in any test that derives a layout from a t.TempDir() cityPath and
// then writes under the resulting layout — without the scrub, an ambient
// override would redirect those writes into a non-test data dir.
func scrubManagedDoltLayoutEnv(t *testing.T) {
	t.Helper()
	for _, key := range managedDoltLayoutEnvOverrides {
		t.Setenv(key, "")
	}
}

// resolveManagedDoltRuntimeLayoutForTest scrubs the env-var overrides via
// scrubManagedDoltLayoutEnv and then resolves the layout from cityPath.
// Use it instead of resolveManagedDoltRuntimeLayout in any test that follows
// up with writes under layout.DataDir.
func resolveManagedDoltRuntimeLayoutForTest(t *testing.T, cityPath string) managedDoltRuntimeLayout {
	t.Helper()
	scrubManagedDoltLayoutEnv(t)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	return layout
}

func writeReachableManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(city .beads): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}
	return port
}

func writeReachableProviderManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(city .beads/dolt): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write provider Dolt state: %v", err)
	}
	return port
}

func occupyManagedDoltPort(t *testing.T, port int) {
	t.Helper()

	cmd := exec.Command("python3", "-c", `
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.time() + 10.0
sock = None
while time.time() < deadline:
    candidate = socket.socket()
    candidate.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        candidate.bind(("127.0.0.1", port))
        candidate.listen(5)
        sock = candidate
        break
    except OSError:
        candidate.close()
        time.sleep(0.05)

if sock is None:
    raise SystemExit(3)

def _stop(*_args):
    raise SystemExit(0)

signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    time.sleep(1)
`, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start managed port blocker: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("managed port blocker for %d exited early", port)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("managed port blocker on %d did not become ready", port)
}

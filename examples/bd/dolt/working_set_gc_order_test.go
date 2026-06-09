package dolt_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// The dolt working-set GC order wires the bare-GC mode of `gc dolt compact`
// (GC_DOLT_COMPACT_BARE_GC=1) onto a short cooldown. It closes the gap left by
// PR #1918, which removed the recurring dolt-gc-nudge order on the assumption
// that compaction performs scheduled full-GC cleanup. Compaction is gated on
// GC_DOLT_COMPACT_THRESHOLD_COMMITS (default 2000), so databases below that
// commit count — gc, hq, pgp on a typical city — never receive any DOLT_GC and
// accumulate working-set/NBS-journal orphan chunks driven by write churn rather
// than commit-graph length (gc-59o). PR #2687 added the bare-GC mechanism but
// explicitly deferred this scheduling step.
func TestWorkingSetGCOrderRunsBareGCOnShortCooldown(t *testing.T) {
	root := repoRoot(t)
	orderPath := filepath.Join(root, "orders", "dolt-working-set-gc.toml")
	data, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatalf("read working-set GC order: %v", err)
	}

	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parse working-set GC order: %v", err)
	}
	order.Name = "dolt-working-set-gc"
	if err := orders.Validate(order); err != nil {
		t.Fatalf("validate working-set GC order: %v", err)
	}

	if !order.IsExec() {
		t.Fatalf("working-set GC order must be an exec order, got formula=%q", order.Formula)
	}
	if order.Exec != "gc dolt compact" {
		t.Fatalf("working-set GC order exec = %q, want %q", order.Exec, "gc dolt compact")
	}
	if order.Trigger != "cooldown" {
		t.Fatalf("working-set GC order trigger = %q, want cooldown", order.Trigger)
	}

	if got := order.Env["GC_DOLT_COMPACT_BARE_GC"]; got != "1" {
		t.Fatalf("working-set GC order must set GC_DOLT_COMPACT_BARE_GC=1 to skip the flatten threshold, got %q", got)
	}
	// The bare-GC env key must not be a controller-owned key, or dispatch
	// rejects the [order.env] override before the order ever runs.
	if orders.IsReservedExecEnvKey("GC_DOLT_COMPACT_BARE_GC") {
		t.Fatal("GC_DOLT_COMPACT_BARE_GC is reserved; [order.env] override would be rejected at dispatch")
	}

	// The working-set GC must run far more often than the 24h flatten so the
	// NBS journal / working-set orphans stay bounded between runs (PR #1196
	// measured 1h as both necessary and cheap). Require an interval at least as
	// tight as 1h, and tighter than the flatten cadence.
	interval, err := time.ParseDuration(order.Interval)
	if err != nil {
		t.Fatalf("working-set GC order interval %q is not a valid duration: %v", order.Interval, err)
	}
	if interval <= 0 || interval > time.Hour {
		t.Fatalf("working-set GC order interval = %s, want a positive cadence no longer than 1h", interval)
	}

	// A working-set CALL DOLT_GC() can be slow on a large store; the timeout
	// must comfortably exceed the default per-call bound (1800s) so a single
	// slow database does not get its GC cut off mid-run.
	if got := order.TimeoutOrDefault(); got < 1800*time.Second {
		t.Fatalf("working-set GC order timeout = %s, want at least 1800s to cover a slow DOLT_GC", got)
	}
}

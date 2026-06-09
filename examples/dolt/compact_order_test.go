package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCompactorOrderCadenceTracksCommitThreshold guards the compactor wake
// cadence against the bug observed in deacon patrol em-wisp-qp2 (gascity
// gc-1axb5): a 24h cooldown let busy databases accumulate a full day of
// commits before the threshold-gated compactor ever ran, so idle-town commit
// volume (~2k commits/hour background) overshot the per-database compaction
// threshold many times over between runs.
//
// The fix keeps cost control where it already lives — the per-database
// commit-count gate inside `gc dolt compact`
// (GC_DOLT_COMPACT_THRESHOLD_COMMITS, default 2000) — and only shortens the
// order's wake cadence so the cheap threshold probe runs often enough to catch
// a database shortly after it crosses the threshold instead of up to 24h
// later. The interval bounds the per-database commit overshoot; at the
// observed background rate a database crosses the 2000-commit threshold within
// a few hours, so the cooldown must stay at or below that horizon.
func TestCompactorOrderCadenceTracksCommitThreshold(t *testing.T) {
	root := repoRoot(t)
	orderPath := filepath.Join(root, "orders", "mol-dog-compactor.toml")
	data, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatalf("read compactor order: %v", err)
	}
	text := string(data)

	interval := orderInterval(t, text)
	const maxInterval = 6 * time.Hour
	if interval > maxInterval {
		t.Fatalf("compactor cooldown %s exceeds %s: at the observed background commit rate a database crosses the per-db compaction threshold within a few hours, so a longer cooldown lets commit volume overshoot the threshold before the compactor wakes (gc-1axb5)\n%s",
			interval, maxInterval, text)
	}

	// The wake cadence is only safe to shorten because the expensive
	// flatten/full-GC path is gated per database on commit count inside the
	// exec'd command. Assert the order still routes through that gate rather
	// than some unconditional compaction, so a future edit can't turn the
	// shorter cadence into a full GC on every wake.
	if !strings.Contains(text, `exec = "gc dolt compact"`) {
		t.Fatalf("compactor order must exec the threshold-gated `gc dolt compact` so the shorter cooldown only triggers cheap per-db commit-count probes, not unconditional compaction:\n%s", text)
	}
}

// orderInterval extracts the `interval = "..."` value from an order TOML body
// and parses it as a Go duration.
func orderInterval(t *testing.T, text string) time.Duration {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "interval") {
			continue
		}
		_, rhs, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value := strings.Trim(strings.TrimSpace(rhs), `"`)
		d, err := time.ParseDuration(value)
		if err != nil {
			t.Fatalf("parse compactor interval %q: %v", value, err)
		}
		return d
	}
	t.Fatalf("compactor order has no interval field:\n%s", text)
	return 0
}

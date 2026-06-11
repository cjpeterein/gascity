package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func hookBeadAt(id, assignee string, created time.Time) beads.Bead {
	return beads.Bead{ID: id, Title: id, Status: "open", Type: "task", Assignee: assignee, CreatedAt: created}
}

// TestEvaluateStandardHookWorkTierOrder proves the three-tier priority of the
// standard work query: assigned in_progress (crash recovery) beats assigned
// ready, which beats routed pool demand.
func TestEvaluateStandardHookWorkTierOrder(t *testing.T) {
	now := time.Now().UTC()
	spec := hookCacheSpec{
		Identities: []string{"sess-id", "sess-name", "alias"},
		Origin:     "",
		Routes:     []string{"myrig/worker"},
	}
	inProgress := beads.Bead{ID: "gc-ip", Status: "in_progress", Assignee: "sess-name", CreatedAt: now}
	assignedReady := hookBeadAt("gc-ready", "alias", now)
	routed := hookBeadAt("gc-routed", "", now)
	routed.Metadata = map[string]string{"gc.routed_to": "myrig/worker"}

	got := evaluateStandardHookWork(hookCacheCandidates{
		Ready:      []beads.Bead{assignedReady, routed},
		InProgress: []beads.Bead{inProgress},
	}, spec)
	if len(got) != 1 || got[0].ID != "gc-ip" {
		t.Fatalf("tier order: got %+v, want [gc-ip]", got)
	}

	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{routed, assignedReady},
	}, spec)
	if len(got) != 1 || got[0].ID != "gc-ready" {
		t.Fatalf("tier order without in_progress: got %+v, want [gc-ready]", got)
	}

	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{routed},
	}, spec)
	if len(got) != 1 || got[0].ID != "gc-routed" {
		t.Fatalf("routed tier: got %+v, want [gc-routed]", got)
	}

	got = evaluateStandardHookWork(hookCacheCandidates{}, spec)
	if len(got) != 0 {
		t.Fatalf("no candidates: got %+v, want empty", got)
	}
}

// TestEvaluateStandardHookWorkIdentityOrder proves identities resolve in
// GC_SESSION_ID > GC_SESSION_NAME > GC_ALIAS order and empty identities are
// skipped, matching the script's for-loop.
func TestEvaluateStandardHookWorkIdentityOrder(t *testing.T) {
	now := time.Now().UTC()
	byName := beads.Bead{ID: "gc-by-name", Status: "in_progress", Assignee: "sess-name", CreatedAt: now}
	byAlias := beads.Bead{ID: "gc-by-alias", Status: "in_progress", Assignee: "alias", CreatedAt: now}

	got := evaluateStandardHookWork(hookCacheCandidates{
		InProgress: []beads.Bead{byAlias, byName},
	}, hookCacheSpec{Identities: []string{"", "sess-name", "alias"}})
	if len(got) != 1 || got[0].ID != "gc-by-name" {
		t.Fatalf("identity order: got %+v, want [gc-by-name]", got)
	}

	got = evaluateStandardHookWork(hookCacheCandidates{
		InProgress: []beads.Bead{byAlias, byName},
	}, hookCacheSpec{Identities: []string{"", "", "alias"}})
	if len(got) != 1 || got[0].ID != "gc-by-alias" {
		t.Fatalf("identity skip: got %+v, want [gc-by-alias]", got)
	}
}

// TestEvaluateStandardHookWorkFindsInProgressWisp covers the working-agent
// case the status line renders most often: the assigned molecule wisp is
// ephemeral and must surface from the in_progress candidates.
func TestEvaluateStandardHookWorkFindsInProgressWisp(t *testing.T) {
	wisp := beads.Bead{ID: "mol-1", Status: "in_progress", Assignee: "sess-name", Ephemeral: true, CreatedAt: time.Now().UTC()}
	got := evaluateStandardHookWork(hookCacheCandidates{
		InProgress: []beads.Bead{wisp},
	}, hookCacheSpec{Identities: []string{"", "sess-name", ""}})
	if len(got) != 1 || got[0].ID != "mol-1" {
		t.Fatalf("wisp tier 1: got %+v, want [mol-1]", got)
	}
}

// TestEvaluateStandardHookWorkAssignedReadyKeepsEpics pins the gc-udx
// contract: the assigned ready tier does NOT exclude epics (a patrol agent
// resuming its own epic wisp), while the routed tier does.
func TestEvaluateStandardHookWorkAssignedReadyKeepsEpics(t *testing.T) {
	now := time.Now().UTC()
	assignedEpic := beads.Bead{ID: "gc-epic", Status: "open", Type: "epic", Assignee: "alias", CreatedAt: now}
	got := evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{assignedEpic},
	}, hookCacheSpec{Identities: []string{"", "", "alias"}})
	if len(got) != 1 || got[0].ID != "gc-epic" {
		t.Fatalf("assigned epic: got %+v, want [gc-epic]", got)
	}

	routedEpic := beads.Bead{
		ID: "gc-epic-routed", Status: "open", Type: "epic", CreatedAt: now,
		Metadata: map[string]string{"gc.routed_to": "myrig/worker"},
	}
	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{routedEpic},
	}, hookCacheSpec{Routes: []string{"myrig/worker"}})
	if len(got) != 0 {
		t.Fatalf("routed epic must be excluded: got %+v, want empty", got)
	}
}

// TestEvaluateStandardHookWorkOriginGate proves the routed tier only fires
// for ephemeral (pool) or origin-less invocations, matching the script's
// GC_SESSION_ORIGIN case gate.
func TestEvaluateStandardHookWorkOriginGate(t *testing.T) {
	routed := hookBeadAt("gc-routed", "", time.Now().UTC())
	routed.Metadata = map[string]string{"gc.routed_to": "myrig/worker"}
	candidates := hookCacheCandidates{Ready: []beads.Bead{routed}}

	for _, origin := range []string{"", "ephemeral"} {
		got := evaluateStandardHookWork(candidates, hookCacheSpec{Origin: origin, Routes: []string{"myrig/worker"}})
		if len(got) != 1 {
			t.Fatalf("origin %q: got %+v, want routed bead", origin, got)
		}
	}
	for _, origin := range []string{"named", "crew"} {
		got := evaluateStandardHookWork(candidates, hookCacheSpec{Origin: origin, Routes: []string{"myrig/worker"}})
		if len(got) != 0 {
			t.Fatalf("origin %q: got %+v, want empty (gate must block routed tier)", origin, got)
		}
	}
}

// TestEvaluateStandardHookWorkRoutedTier covers the routed tier predicates:
// canonical gc.routed_to first (oldest wins), the run_target+kind=workflow
// migration shape only when gc.routed_to is empty, and assigned beads never
// matching pool demand.
func TestEvaluateStandardHookWorkRoutedTier(t *testing.T) {
	now := time.Now().UTC()
	older := hookBeadAt("gc-old", "", now.Add(-time.Hour))
	older.Metadata = map[string]string{"gc.routed_to": "myrig/worker"}
	newer := hookBeadAt("gc-new", "", now)
	newer.Metadata = map[string]string{"gc.routed_to": "myrig/worker"}

	got := evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{newer, older},
	}, hookCacheSpec{Routes: []string{"myrig/worker"}})
	if len(got) != 1 || got[0].ID != "gc-old" {
		t.Fatalf("canonical oldest: got %+v, want [gc-old]", got)
	}

	legacy := hookBeadAt("gc-legacy", "", now)
	legacy.Metadata = map[string]string{"gc.run_target": "myrig/worker", "gc.kind": "workflow"}
	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{legacy},
	}, hookCacheSpec{Routes: []string{"myrig/worker"}})
	if len(got) != 1 || got[0].ID != "gc-legacy" {
		t.Fatalf("legacy migration shape: got %+v, want [gc-legacy]", got)
	}

	migrated := hookBeadAt("gc-migrated", "", now)
	migrated.Metadata = map[string]string{
		"gc.run_target": "myrig/worker", "gc.kind": "workflow", "gc.routed_to": "other/route",
	}
	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{migrated},
	}, hookCacheSpec{Routes: []string{"myrig/worker"}})
	if len(got) != 0 {
		t.Fatalf("already-routed legacy bead must not match run_target: got %+v", got)
	}

	legacyNonWorkflow := hookBeadAt("gc-non-workflow", "", now)
	legacyNonWorkflow.Metadata = map[string]string{"gc.run_target": "myrig/worker"}
	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{legacyNonWorkflow},
	}, hookCacheSpec{Routes: []string{"myrig/worker"}})
	if len(got) != 0 {
		t.Fatalf("run_target without gc.kind=workflow must not match: got %+v", got)
	}

	assigned := hookBeadAt("gc-assigned", "someone-else", now)
	assigned.Metadata = map[string]string{"gc.routed_to": "myrig/worker"}
	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{assigned},
	}, hookCacheSpec{Routes: []string{"myrig/worker"}})
	if len(got) != 0 {
		t.Fatalf("assigned bead must not match pool demand: got %+v", got)
	}
}

// TestEvaluateStandardHookWorkControlDispatcherAliases proves the legacy
// control-dispatcher identity and route aliases stay reachable: an identity
// ending in control-dispatcher also matches its workflow-control alias, and
// multiple routes are probed in order.
func TestEvaluateStandardHookWorkControlDispatcherAliases(t *testing.T) {
	now := time.Now().UTC()
	legacyAssigned := beads.Bead{
		ID: "gc-legacy-assigned", Status: "in_progress",
		Assignee: "town/workflow-control", CreatedAt: now,
	}
	got := evaluateStandardHookWork(hookCacheCandidates{
		InProgress: []beads.Bead{legacyAssigned},
	}, hookCacheSpec{Identities: []string{"", "town/control-dispatcher", ""}})
	if len(got) != 1 || got[0].ID != "gc-legacy-assigned" {
		t.Fatalf("legacy control identity alias: got %+v, want [gc-legacy-assigned]", got)
	}

	legacyRouted := hookBeadAt("gc-legacy-routed", "", now)
	legacyRouted.Metadata = map[string]string{"gc.routed_to": "town/workflow-control"}
	got = evaluateStandardHookWork(hookCacheCandidates{
		Ready: []beads.Bead{legacyRouted},
	}, hookCacheSpec{Routes: []string{"town/control-dispatcher", "town/workflow-control"}})
	if len(got) != 1 || got[0].ID != "gc-legacy-routed" {
		t.Fatalf("legacy control route: got %+v, want [gc-legacy-routed]", got)
	}
}

// TestHookCacheResultJSON proves the cached evaluation emits the same
// JSON-array shape the shell work query prints, including the empty-result
// literal doHook expects.
func TestHookCacheResultJSON(t *testing.T) {
	now := time.Now().UTC()
	assigned := beads.Bead{ID: "gc-work", Status: "in_progress", Assignee: "sess-name", CreatedAt: now}
	out, err := hookCacheResultJSON(hookCacheCandidates{
		InProgress: []beads.Bead{assigned},
	}, hookCacheSpec{Identities: []string{"", "sess-name", ""}})
	if err != nil {
		t.Fatalf("hookCacheResultJSON: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output %q is not a JSON array: %v", out, err)
	}
	if len(decoded) != 1 || decoded[0]["id"] != "gc-work" {
		t.Fatalf("decoded = %+v, want one bead gc-work", decoded)
	}

	out, err = hookCacheResultJSON(hookCacheCandidates{}, hookCacheSpec{Identities: []string{"sess-id", "", ""}})
	if err != nil {
		t.Fatalf("hookCacheResultJSON(empty): %v", err)
	}
	if out != "[]" {
		t.Fatalf("empty output = %q, want []", out)
	}
}

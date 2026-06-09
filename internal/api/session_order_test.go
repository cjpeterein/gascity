package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// orderTestCfg builds a city config whose rig-startup order is
// beads, gascity, petereinc-gascity-pack (declaration order), with a
// crew member per rig so crew-vs-pool classification is exercised.
func orderTestCfg() *config.City {
	return &config.City{
		Rigs: []config.Rig{
			{Name: "beads"},
			{Name: "gascity"},
			{Name: "petereinc-gascity-pack"},
		},
		Agents: []config.Agent{
			{Name: "lando", Dir: "gascity/crew"},
			{Name: "jasper", Dir: "beads/crew"},
			{Name: "lupin", Dir: "petereinc-gascity-pack/crew"},
		},
	}
}

// templatesInOrder extracts the Template field from a sorted response slice
// for compact assertions.
func templatesInOrder(items []sessionResponse) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].Template
	}
	return out
}

func TestSortSessionResponsesDeterministicOrder(t *testing.T) {
	cfg := orderTestCfg()

	// Shuffled input mixing city agents and three rigs' roles.
	in := []sessionResponse{
		{Template: "gascity/polecat", Alias: "gascity/slit", AgentKind: "pool"},
		{Template: "petereinc-gascity-pack/gastown.witness", AgentKind: "role"},
		{Template: "gastown.deacon", AgentKind: "role"},
		{Template: "beads/refinery", AgentKind: "role"},
		{Template: "gascity/refinery", AgentKind: "role"},
		{Template: "control-dispatcher", AgentKind: "role"},
		{Template: "gascity/witness", AgentKind: "role"},
		{Template: "gastown.mayor", AgentKind: "role"},
		{Template: "gascity/lando", AgentKind: "crew"},
		{Template: "beads/jasper", AgentKind: "crew"},
		{Template: "gastown.boot", AgentKind: "role"},
		{Template: "petereinc-gascity-pack/lupin", AgentKind: "crew"},
		{Template: "beads/witness", AgentKind: "role"},
	}

	sortSessionResponses(in, cfg)

	want := []string{
		// City agents first: mayor, deacon, then other city roles by name.
		"gastown.mayor",
		"gastown.deacon",
		"control-dispatcher",
		"gastown.boot",
		// Rig groups in startup order: beads, gascity, petereinc-gascity-pack.
		// Within each: witness < refinery < crew < polecat.
		"beads/witness",
		"beads/refinery",
		"beads/jasper",
		"gascity/witness",
		"gascity/refinery",
		"gascity/lando",
		"gascity/polecat",
		"petereinc-gascity-pack/gastown.witness",
		"petereinc-gascity-pack/lupin",
	}

	got := templatesInOrder(in)
	if len(got) != len(want) {
		t.Fatalf("sorted length = %d, want %d\n got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch at %d:\n got=%v\nwant=%v", i, got, want)
		}
	}
}

func TestSortSessionResponsesUnknownRigSortsLast(t *testing.T) {
	cfg := orderTestCfg()
	in := []sessionResponse{
		{Template: "zzz-unknown-rig/polecat", AgentKind: "pool"},
		{Template: "gascity/witness", AgentKind: "role"},
		{Template: "gastown.mayor", AgentKind: "role"},
	}
	sortSessionResponses(in, cfg)
	got := templatesInOrder(in)
	want := []string{"gastown.mayor", "gascity/witness", "zzz-unknown-rig/polecat"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unknown-rig order mismatch:\n got=%v\nwant=%v", got, want)
		}
	}
}

func TestSortSessionResponsesUnknownRolesSortByName(t *testing.T) {
	cfg := orderTestCfg()
	// Two unknown roles in the same rig sort by role name (alpha < zeta).
	in := []sessionResponse{
		{Template: "gascity/zeta", AgentKind: ""},
		{Template: "gascity/alpha", AgentKind: ""},
	}
	sortSessionResponses(in, cfg)
	got := templatesInOrder(in)
	want := []string{"gascity/alpha", "gascity/zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unknown-role-by-name order mismatch:\n got=%v\nwant=%v", got, want)
		}
	}
}

func TestSortSessionInfosMatchesResponseOrder(t *testing.T) {
	cfg := orderTestCfg()
	in := []session.Info{
		{Template: "gascity/polecat"},
		{Template: "beads/witness"},
		{Template: "gastown.mayor"},
		{Template: "gascity/witness"},
	}
	SortSessionInfos(in, cfg)
	got := make([]string, len(in))
	for i := range in {
		got[i] = in[i].Template
	}
	want := []string{"gastown.mayor", "beads/witness", "gascity/witness", "gascity/polecat"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SortSessionInfos order mismatch:\n got=%v\nwant=%v", got, want)
		}
	}
}

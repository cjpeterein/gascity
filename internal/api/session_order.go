package api

import (
	"math"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Session list ordering.
//
// `gc session list` (table and --json) and the dashboard all read the same
// session set. Without a deterministic order operators have to hunt for the
// mayor, the city agents, and a given rig's group. This file defines the
// single comparator that makes the order stable and meaningful:
//
//  1. City-level agents first (no rig prefix), in a fixed role order:
//     mayor, deacon, then other city/HQ agents by name.
//  2. Then one group per rig, rigs ordered by rig-startup order (the order
//     they are declared in city.toml, which is the same order the daemon
//     brings them up — see cmd/gc/cmd_start.go ranging cfg.Rigs).
//  3. Within a rig group, a fixed role order: witness, refinery, crew, then
//     polecats/ephemeral; unknown roles sort last, stable by name.
//
// The comparator is applied at the API layer so JSON consumers and the CLI
// table (which renders the server-provided order verbatim) agree. The
// controller-down fallback in cmd/gc reuses SortSessionInfos so both paths
// produce the same order — one comparator, one source of truth.

// Role ranks within a group. Lower sorts first. Unknown roles use
// roleRankUnknown so they fall to the end of their group, tie-broken by name.
const (
	cityRoleMayor = iota
	cityRoleDeacon
	cityRoleOther
)

const (
	rigRoleWitness = iota
	rigRoleRefinery
	rigRoleCrew
	rigRolePolecat
	rigRoleUnknown
)

// sessionOrderKey is the precomputed sort position for one session row.
// Comparison is lexicographic over (group, rigIndex, roleRank, name).
type sessionOrderKey struct {
	// cityFirst is 0 for city-level agents and 1 for rig-scoped sessions, so
	// all city agents sort ahead of every rig group.
	cityFirst int
	// rigIndex is the rig's position in startup order; math.MaxInt for
	// sessions whose rig is not found in the config (sort last among rigs).
	rigIndex int
	// roleRank orders roles within the city group or within a rig group.
	roleRank int
	// name is the final stable tie-breaker (the role suffix or template).
	name string
}

func (k sessionOrderKey) less(o sessionOrderKey) bool {
	if k.cityFirst != o.cityFirst {
		return k.cityFirst < o.cityFirst
	}
	if k.rigIndex != o.rigIndex {
		return k.rigIndex < o.rigIndex
	}
	if k.roleRank != o.roleRank {
		return k.roleRank < o.roleRank
	}
	return k.name < o.name
}

// rigStartupOrder maps each rig name to its declaration index in city.toml,
// which is the order the daemon brings rigs up.
func rigStartupOrder(cfg *config.City) map[string]int {
	order := map[string]int{}
	if cfg == nil {
		return order
	}
	for i := range cfg.Rigs {
		name := strings.TrimSpace(cfg.Rigs[i].Name)
		if name == "" {
			continue
		}
		if _, seen := order[name]; !seen {
			order[name] = i
		}
	}
	return order
}

// roleSuffix extracts the bare role name from a qualified template's name
// part. The name may itself be pack-qualified with a dot (e.g.
// "gastown.witness" -> "witness"); the segment after the last dot is the role.
func roleSuffix(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// cityRoleRank ranks a city-level (rig-less) role: mayor, deacon, then others.
func cityRoleRank(role string) int {
	switch role {
	case "mayor":
		return cityRoleMayor
	case "deacon":
		return cityRoleDeacon
	default:
		return cityRoleOther
	}
}

// rigRoleRank ranks a role within a rig group. agentKind ("crew"/"pool"/
// "role"/"") disambiguates crew members, whose themed names (e.g. "lando")
// are not literally "crew". Crew sorts between refinery and polecat; pool
// members (polecats/ephemeral) sort after crew; unrecognized roles sort last.
func rigRoleRank(role, agentKind string) int {
	switch role {
	case "witness":
		return rigRoleWitness
	case "refinery":
		return rigRoleRefinery
	case "polecat":
		return rigRolePolecat
	}
	switch agentKind {
	case "crew":
		return rigRoleCrew
	case "pool":
		return rigRolePolecat
	}
	return rigRoleUnknown
}

// orderKeyForTemplate builds the sort key for a session from its template,
// the resolved agentKind, and the rig-startup order. Templates without a
// rig prefix (e.g. "gastown.mayor", "control-dispatcher") are city agents.
func orderKeyForTemplate(template, agentKind string, rigOrder map[string]int) sessionOrderKey {
	rig, name := config.ParseQualifiedName(template)
	role := roleSuffix(name)
	if rig == "" {
		return sessionOrderKey{
			cityFirst: 0,
			rigIndex:  0,
			roleRank:  cityRoleRank(role),
			name:      name,
		}
	}
	idx, ok := rigOrder[rig]
	if !ok {
		idx = math.MaxInt
	}
	return sessionOrderKey{
		cityFirst: 1,
		rigIndex:  idx,
		roleRank:  rigRoleRank(role, agentKind),
		name:      name,
	}
}

// stableSortByKeys reorders items in place so that they follow the order of
// the parallel keys slice, tie-broken by original index for stability.
// keys[i] must be the precomputed key for items[i]; the permutation is
// applied to both so they stay aligned. T is constrained to the row shapes
// this file sorts.
func stableSortByKeys[T any](items []T, keys []sessionOrderKey) {
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return keys[idx[a]].less(keys[idx[b]])
	})
	sorted := make([]T, len(items))
	for newPos, oldPos := range idx {
		sorted[newPos] = items[oldPos]
	}
	copy(items, sorted)
}

// sortSessionResponses orders API session rows in place using the
// deterministic city-first, rig-startup, role-within-group comparator. The
// sort is stable so equal keys preserve input order. agentKind is read from
// each row's already-derived AgentKind field.
func sortSessionResponses(items []sessionResponse, cfg *config.City) {
	rigOrder := rigStartupOrder(cfg)
	keys := make([]sessionOrderKey, len(items))
	for i := range items {
		keys[i] = orderKeyForTemplate(items[i].Template, items[i].AgentKind, rigOrder)
	}
	stableSortByKeys(items, keys)
}

// SortSessionInfos orders raw session.Info rows in place using the same
// comparator as sortSessionResponses, for the controller-down fallback path
// in cmd/gc. Crew classification is derived from cfg via findAgent so the
// fallback matches the API order.
func SortSessionInfos(items []session.Info, cfg *config.City) {
	rigOrder := rigStartupOrder(cfg)
	keys := make([]sessionOrderKey, len(items))
	for i := range items {
		kind := ""
		if cfg != nil {
			if a, ok := findAgent(cfg, items[i].Template); ok {
				kind = classifyAgentKind(a)
			}
		}
		keys[i] = orderKeyForTemplate(items[i].Template, kind, rigOrder)
	}
	stableSortByKeys(items, keys)
}

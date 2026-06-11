package main

import (
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The supervisor-cache hook path (gc-lqt). The standard work query is a
// shell script that spawns 4-6 bd subprocesses per evaluation — and the
// explicit-target flow spawns more for session-name resolution — which the
// tmux status line multiplies into hundreds of data-plane queries per
// second on an otherwise idle town. When a supervisor API endpoint is
// reachable and the agent uses the standard (built-in) work query, the
// hook instead reads the supervisor's cached session-bead, ready, and
// in_progress projections over HTTP and evaluates the same three-tier
// semantics in process. Any API failure falls through to the original
// shell path, so availability never regresses.

// hookCacheCandidates carries the supervisor-cache projections the standard
// work query tiers select from: the cross-rig ready set and the cross-rig
// in_progress set (both spanning persistent and wisp tiers on policy-wrapped
// stores, matching the script's bd + ephemeral probe union).
type hookCacheCandidates struct {
	Ready      []beads.Bead
	InProgress []beads.Bead
}

// hookCacheSpec carries the identity and routing context the standard work
// query script reads from its environment.
type hookCacheSpec struct {
	// Identities in resolution order: GC_SESSION_ID, GC_SESSION_NAME,
	// GC_ALIAS. Empty entries are skipped, matching the script's id loop.
	Identities []string
	// Origin mirrors GC_SESSION_ORIGIN: the routed pool tier only fires for
	// "ephemeral" or empty origins.
	Origin string
	// Routes are the gc.routed_to targets of the routed tier, in probe
	// order (Agent.RouteTargets: pool demand target plus the legacy
	// workflow-control alias for the control dispatcher).
	Routes []string
}

// evaluateStandardHookWork applies the standard three-tier work query
// (internal/config standardAssignedWorkQueryScript + routed pool probe) to
// supervisor-cache candidates. Tier priority: assigned in_progress (crash
// recovery) > assigned ready > routed unassigned pool demand. Returns at
// most one bead, mirroring the script's --limit=1 first-match contract.
func evaluateStandardHookWork(c hookCacheCandidates, spec hookCacheSpec) []beads.Bead {
	identities := expandHookIdentities(spec.Identities)

	for _, id := range identities {
		if match, ok := newestAssignedBead(c.InProgress, id); ok {
			return []beads.Bead{match}
		}
	}

	for _, id := range identities {
		for _, b := range c.Ready {
			if b.Assignee == id {
				return []beads.Bead{b}
			}
		}
	}

	switch spec.Origin {
	case "", "ephemeral":
	default:
		return nil
	}
	for _, route := range spec.Routes {
		route = strings.TrimSpace(route)
		if route == "" {
			continue
		}
		if match, ok := oldestRoutedBead(c.Ready, route, canonicalRoutedMatch); ok {
			return []beads.Bead{match}
		}
		if match, ok := oldestRoutedBead(c.Ready, route, legacyRoutedMatch); ok {
			return []beads.Bead{match}
		}
	}
	return nil
}

// expandHookIdentities drops empty identities and appends the legacy
// workflow-control alias for control-dispatcher identities, preserving
// order and uniqueness. Mirrors the script's per-identity legacy case and
// upstream's workflowServeControlReadyAssignees expansion.
func expandHookIdentities(identities []string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, id := range identities {
		add(id)
		id = strings.TrimSpace(id)
		if strings.HasSuffix(id, config.ControlDispatcherAgentName) {
			add(strings.TrimSuffix(id, config.ControlDispatcherAgentName) + "workflow-control")
		}
	}
	return out
}

// newestAssignedBead picks the newest in_progress bead assigned to id,
// preferring persistent beads over wisps the way the script probes bd list
// before the ephemeral query.
func newestAssignedBead(candidates []beads.Bead, id string) (beads.Bead, bool) {
	matches := make([]beads.Bead, 0, 1)
	for _, b := range candidates {
		if b.Assignee == id {
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return beads.Bead{}, false
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Ephemeral != matches[j].Ephemeral {
			return !matches[i].Ephemeral
		}
		if !matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		}
		return matches[i].ID < matches[j].ID
	})
	return matches[0], true
}

// canonicalRoutedMatch is the routed tier's canonical predicate:
// bd ready --metadata-field gc.routed_to=$target --unassigned --exclude-type=epic.
func canonicalRoutedMatch(b beads.Bead, route string) bool {
	return b.Metadata["gc.routed_to"] == route
}

// legacyRoutedMatch is the routed tier's migration predicate: a workflow
// root still carrying the pre-routing gc.run_target/gc.kind shape and not
// yet claimed by the canonical router.
func legacyRoutedMatch(b beads.Bead, route string) bool {
	return b.Metadata["gc.routed_to"] == "" &&
		b.Metadata["gc.run_target"] == route &&
		b.Metadata["gc.kind"] == "workflow"
}

// oldestRoutedBead picks the oldest unassigned non-epic ready bead matching
// the routed predicate, mirroring bd ready --sort oldest --limit=1.
func oldestRoutedBead(ready []beads.Bead, route string, match func(beads.Bead, string) bool) (beads.Bead, bool) {
	var best beads.Bead
	found := false
	for _, b := range ready {
		if strings.TrimSpace(b.Assignee) != "" || b.Type == "epic" {
			continue
		}
		if !match(b, route) {
			continue
		}
		if !found || b.CreatedAt.Before(best.CreatedAt) ||
			(b.CreatedAt.Equal(best.CreatedAt) && b.ID < best.ID) {
			best = b
			found = true
		}
	}
	return best, found
}

// hookCacheResultJSON evaluates the standard work query over supervisor
// cache candidates and encodes the result in the same JSON-array shape the
// shell work query prints, so doHook's normalization and exit-code logic
// apply unchanged.
func hookCacheResultJSON(c hookCacheCandidates, spec hookCacheSpec) (string, error) {
	result := evaluateStandardHookWork(c, spec)
	if result == nil {
		result = []beads.Bead{}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// hookCacheIdentitiesForAgent derives the work-query identity candidates for
// an explicit hook target from the supervisor's cached session beads: the
// session bead ID and runtime session name of every open session bead
// labeled for the agent (newest first, the list endpoint's canonical order),
// then the deterministic default session name and the qualified name itself.
// This replaces cliSessionName's store-backed (bd subprocess) resolution on
// the cached path. The session-bead label read costs one cached HTTP call;
// the enriched /sessions endpoint is deliberately avoided because its
// per-session runtime enrichment is ~100x slower than a cache-served bead
// list.
func hookCacheIdentitiesForAgent(client *api.Client, cityName, sessionTemplate string, a *config.Agent) ([]string, error) {
	sessions, err := client.ListBeads(api.ListBeadsOpts{
		Label:  sessionBeadLabel,
		Cached: true,
		Limit:  1000,
	})
	if err != nil {
		return nil, err
	}
	qualified := a.QualifiedName()
	var ids []string
	for _, b := range sessions.Body {
		if !sessionBeadMatchesHookAgent(b, qualified) {
			continue
		}
		ids = append(ids, b.ID, b.Metadata["session_name"])
	}
	ids = append(ids, sessionName(nil, cityName, qualified, sessionTemplate), qualified)
	return ids, nil
}

// sessionBeadMatchesHookAgent reports whether a session bead belongs to the
// agent, matching the canonical agent label first and falling back to the
// identity metadata lookupSessionName reads.
func sessionBeadMatchesHookAgent(b beads.Bead, qualified string) bool {
	for _, label := range b.Labels {
		if label == "agent:"+qualified {
			return true
		}
	}
	return b.Metadata["agent_name"] == qualified || b.Metadata["alias"] == qualified
}

// tryServeHookFromSupervisorCache attempts to answer a read-only standard
// work query hook from the supervisor cache. Returns (exitCode, true) when
// the hook was served; (0, false) means the caller must continue on the
// shell work-query path (no API endpoint, or any API read failed).
func tryServeHookFromSupervisorCache(cityPath, cityName, sessionTemplate string, a *config.Agent, sessionTemplateContext bool, stdout, stderr io.Writer) (int, bool) {
	client := supervisorCacheReadClient(cityPath)
	if client == nil {
		return 0, false
	}

	spec := hookCacheSpec{Routes: a.RouteTargets()}
	if sessionTemplateContext {
		spec.Identities = []string{
			os.Getenv("GC_SESSION_ID"),
			os.Getenv("GC_SESSION_NAME"),
			os.Getenv("GC_ALIAS"),
		}
		spec.Origin = os.Getenv("GC_SESSION_ORIGIN")
	} else {
		identities, err := hookCacheIdentitiesForAgent(client, cityName, sessionTemplate, a)
		if err != nil {
			logRoute(stderr, "hook", "fallback", api.FallbackReason(err))
			return 0, false
		}
		spec.Identities = identities
	}

	candidates, err := fetchHookCacheCandidates(client)
	if err != nil {
		logRoute(stderr, "hook", "fallback", api.FallbackReason(err))
		return 0, false
	}
	output, err := hookCacheResultJSON(candidates, spec)
	if err != nil {
		logRoute(stderr, "hook", "fallback", "encode-error")
		return 0, false
	}
	logRoute(stderr, "hook", "api", "")
	return doHook("", "", false, func(string, string) (string, error) {
		return output, nil
	}, stdout, stderr), true
}

// fetchHookCacheCandidates reads the two cached projections one hook
// evaluation needs: the cross-rig ready set and the cross-rig in_progress
// list.
func fetchHookCacheCandidates(client *api.Client) (hookCacheCandidates, error) {
	ready, err := client.ListReadyBeadsCached()
	if err != nil {
		return hookCacheCandidates{}, err
	}
	inProgress, err := client.ListBeads(api.ListBeadsOpts{
		Status: "in_progress",
		Cached: true,
		Limit:  1000,
	})
	if err != nil {
		return hookCacheCandidates{}, err
	}
	return hookCacheCandidates{Ready: ready.Body, InProgress: inProgress.Body}, nil
}

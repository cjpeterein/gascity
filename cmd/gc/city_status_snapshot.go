package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type cityStatusSnapshot struct {
	CityName      string
	CityPath      string
	Controller    ControllerJSON
	Suspended     bool
	Agents        []cityStatusAgentRow
	Rigs          []StatusRigJSON
	NamedSessions []cityStatusNamedSession
	Summary       StatusSummaryJSON
}

type cityStatusAgentRow struct {
	Agent       StatusAgentJSON
	SessionName string
	GroupName   string
	ScaleLabel  string
	Expanded    bool
	Draining    bool
}

type cityStatusNamedSession struct {
	Identity string
	Status   string
	Mode     string
}

type rigStatusCounts struct {
	Total     int
	Suspended int
}

func openCityStatusStore(cityPath string, stderr io.Writer) (beads.Store, int) {
	if cityPath == "" {
		return nil, 0
	}
	opened, err := openCityStoreAtForStatus(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc status: opening bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return opened, 0
}

func collectCityStatusSnapshot(sp runtime.Provider, cfg *config.City, cityPath string, store beads.Store, stderr io.Writer) cityStatusSnapshot {
	return collectCityStatusSnapshotFromStoreSnapshot(sp, nil, cfg, cityPath, store, loadStatusSessionSnapshot(store), stderr)
}

// agentObservationTask describes a single agent row that needs a live
// observation. Building these up front lets the observation subprocess
// fan-out run concurrently instead of serially per agent.
type agentObservationTask struct {
	agent         config.Agent
	scope         string
	suspendedBase bool
	expanded      bool
	scaleLabel    string
	displayName   string
	qualifiedName string
	groupName     string
	target        statusObservationTarget
}

func collectCityStatusSnapshotFromStoreSnapshot(
	sp runtime.Provider,
	dops drainOps,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stderr io.Writer,
) cityStatusSnapshot {
	suspended := os.Getenv("GC_SUSPENDED") == "1"
	if cfg != nil {
		suspended = citySuspended(cfg)
	}
	snapshot := cityStatusSnapshot{
		CityPath:   cityPath,
		Controller: controllerStatusForCity(cityPath),
		Suspended:  suspended,
	}
	snapshot.CityName = loadedCityName(cfg, cityPath)
	registerStatusProviderACPRoutes(sp, statusSnapshot, snapshot.CityName, cfg)
	if cfg == nil {
		return snapshot
	}

	suspendedRigs := make(map[string]bool, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigs[r.Name] = true
		}
	}

	rigCounts := make(map[string]*rigStatusCounts, len(cfg.Rigs))
	addRigCount := func(rigName string, rowSuspended bool) {
		if rigName == "" {
			return
		}
		tally := rigCounts[rigName]
		if tally == nil {
			tally = &rigStatusCounts{}
			rigCounts[rigName] = tally
		}
		tally.Total++
		if rowSuspended {
			tally.Suspended++
		}
	}

	// Phase 1: build the flat list of observation tasks. This runs the
	// pool discovery (which is lightweight) but defers the per-session
	// runtime observations so they can be parallelized below.
	tasks := make([]agentObservationTask, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		suspendedBase := a.Suspended || (a.Dir != "" && suspendedRigs[a.Dir])
		sp0 := scaleParamsFor(&a)
		scope := "city"
		if a.Dir != "" {
			scope = "rig"
		}

		if a.SupportsInstanceExpansion() {
			maxDisplay := fmt.Sprintf("max=%d", sp0.Max)
			if sp0.Max < 0 {
				maxDisplay = "max=unlimited"
			}
			scaleLabel := fmt.Sprintf("scaled (min=%d, %s)", sp0.Min, maxDisplay)
			headerShown := false
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, snapshot.CityName, cfg.Workspace.SessionTemplate, sp) {
				target := statusObservationTargetForIdentity(statusSnapshot, snapshot.CityName, qualifiedInstance, cfg.Workspace.SessionTemplate)
				_, instanceName := config.ParseQualifiedName(qualifiedInstance)
				task := agentObservationTask{
					agent:         a,
					scope:         scope,
					suspendedBase: suspendedBase,
					expanded:      true,
					displayName:   instanceName,
					qualifiedName: qualifiedInstance,
					groupName:     a.QualifiedName(),
					target:        target,
				}
				if !headerShown {
					task.scaleLabel = scaleLabel
					headerShown = true
				}
				tasks = append(tasks, task)
			}
			continue
		}

		target := statusObservationTargetForIdentity(statusSnapshot, snapshot.CityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		tasks = append(tasks, agentObservationTask{
			agent:         a,
			scope:         scope,
			suspendedBase: suspendedBase,
			expanded:      false,
			displayName:   a.Name,
			qualifiedName: a.QualifiedName(),
			groupName:     a.QualifiedName(),
			target:        target,
		})
	}

	// Phase 2: run all observations (and, when available, drain checks)
	// concurrently. The per-session tmux subprocess fan-out dominated the
	// wall-clock cost of `gc status` before this change; running them in
	// parallel keeps CPU and fork overhead but collapses the serial chain.
	type observationResult struct {
		obs      worker.LiveObservation
		draining bool
	}
	results := make([]observationResult, len(tasks))
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			obs := observeSessionTargetWithWarning("gc status", cityPath, store, sp, cfg, tasks[idx].target, stderr)
			draining := false
			if dops != nil && obs.Running && tasks[idx].target.runtimeSessionName != "" {
				if d, err := dops.isDraining(tasks[idx].target.runtimeSessionName); err == nil {
					draining = d
				}
			}
			results[idx] = observationResult{obs: obs, draining: draining}
		}(i)
	}
	wg.Wait()

	// Phase 3: walk the task list in order and assemble the snapshot so
	// the output order matches the config declaration order exactly as it
	// did when the loop was serial.
	for i, task := range tasks {
		res := results[i]
		row := cityStatusAgentRow{
			Agent: StatusAgentJSON{
				Name:          task.displayName,
				QualifiedName: task.qualifiedName,
				Scope:         task.scope,
				Running:       res.obs.Running,
				Suspended:     task.suspendedBase || res.obs.Suspended,
			},
			SessionName: task.target.runtimeSessionName,
			GroupName:   task.groupName,
			ScaleLabel:  task.scaleLabel,
			Expanded:    task.expanded,
			Draining:    res.draining,
		}
		snapshot.Agents = append(snapshot.Agents, row)
		snapshot.Summary.TotalAgents++
		if res.obs.Running {
			snapshot.Summary.RunningAgents++
		}
		addRigCount(task.agent.Dir, task.suspendedBase || res.obs.Suspended)
	}

	for _, r := range cfg.Rigs {
		suspended := r.Suspended
		if !suspended {
			if tally := rigCounts[r.Name]; tally != nil && tally.Total > 0 && tally.Total == tally.Suspended {
				suspended = true
			}
		}
		snapshot.Rigs = append(snapshot.Rigs, StatusRigJSON{
			Name:      r.Name,
			Path:      r.Path,
			Suspended: suspended,
		})
	}

	for _, ns := range cfg.NamedSessions {
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := namedSessionStatusForCity(cityPath, cfg, store, snapshot.CityName, identity, mode, suspendedRigs)
		snapshot.NamedSessions = append(snapshot.NamedSessions, cityStatusNamedSession{
			Identity: identity,
			Status:   status,
			Mode:     mode,
		})
	}

	return snapshot
}

func namedSessionStatusForCity(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	cityName string,
	identity string,
	mode string,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
		if mode == "always" && namedSessionBlockedBySuspension(cfg, spec.Agent, suspendedRigs) {
			status = "degraded blocked"
		}
	}
	if store == nil {
		return status
	}

	id, err := resolveSessionIDWithConfig(cityPath, cfg, store, identity)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return status
		}
		return "lookup error: " + err.Error()
	}

	bead, err := store.Get(id)
	if err != nil {
		return "lookup error: " + err.Error()
	}
	if state := strings.TrimSpace(bead.Metadata["state"]); state != "" {
		return state
	}
	return "materialized"
}

func collectCitySessionCounts(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (StatusSummaryJSON, error) {
	summary := StatusSummaryJSON{}
	if store == nil {
		return summary, nil
	}
	if cityPath != "" {
		if _, err := os.Stat(cityPath); err != nil {
			return summary, nil
		}
	}
	if store == nil {
		return summary, nil
	}
	catalog, err := workerSessionCatalogWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return summary, err
	}
	sessions, err := catalog.List("", "")
	if err != nil {
		return summary, err
	}
	for _, s := range sessions {
		switch s.State {
		case session.StateActive:
			summary.ActiveSessions++
		case session.StateSuspended:
			summary.SuspendedSessions++
		}
	}
	return summary, nil
}

func cityStatusJSONFromSnapshot(snapshot cityStatusSnapshot, summary StatusSummaryJSON) StatusJSON {
	var agents []StatusAgentJSON
	for _, row := range snapshot.Agents {
		agents = append(agents, row.Agent)
	}
	return StatusJSON{
		CityName:   snapshot.CityName,
		CityPath:   snapshot.CityPath,
		Controller: snapshot.Controller,
		Suspended:  snapshot.Suspended,
		Agents:     agents,
		Rigs:       snapshot.Rigs,
		Summary:    summary,
	}
}

func renderCityStatusText(snapshot cityStatusSnapshot, stdout io.Writer) {
	fmt.Fprintf(stdout, "%s  %s\n", snapshot.CityName, snapshot.CityPath)                //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Controller: %s\n", controllerStatusLine(snapshot.Controller)) //nolint:errcheck // best-effort stdout
	for _, line := range controllerStatusGuidance(snapshot.Controller, snapshot.CityPath) {
		fmt.Fprintf(stdout, "  %s\n", line) //nolint:errcheck // best-effort stdout
	}

	if snapshot.Suspended {
		fmt.Fprintf(stdout, "  Suspended:  yes\n") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "  Suspended:  no\n") //nolint:errcheck // best-effort stdout
	}

	if len(snapshot.Agents) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Agents:")
		for _, row := range snapshot.Agents {
			if row.ScaleLabel != "" {
				fmt.Fprintf(stdout, "  %-24s%s\n", row.GroupName, row.ScaleLabel) //nolint:errcheck // best-effort stdout
			}
			status := agentStatusLineFromRow(row)
			if row.Expanded {
				fmt.Fprintf(stdout, "    %-22s%s\n", row.Agent.QualifiedName, status) //nolint:errcheck // best-effort stdout
			} else {
				fmt.Fprintf(stdout, "  %-24s%s\n", row.Agent.QualifiedName, status) //nolint:errcheck // best-effort stdout
			}
		}
		fmt.Fprintln(stdout)                                                                                        //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "%d/%d agents running\n", snapshot.Summary.RunningAgents, snapshot.Summary.TotalAgents) //nolint:errcheck // best-effort stdout
	}

	if len(snapshot.NamedSessions) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Named sessions:")
		for _, named := range snapshot.NamedSessions {
			fmt.Fprintf(stdout, "  %-24s%s (%s)\n", named.Identity, named.Status, named.Mode) //nolint:errcheck // best-effort stdout
		}
	}

	if len(snapshot.Rigs) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Rigs:")
		for _, r := range snapshot.Rigs {
			annotation := ""
			if r.Suspended {
				annotation = "  (suspended)"
			}
			fmt.Fprintf(stdout, "  %-24s%s%s\n", r.Name, r.Path, annotation) //nolint:errcheck // best-effort stdout
		}
	}
}

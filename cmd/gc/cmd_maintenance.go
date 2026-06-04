package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/githooks"
)

// newMaintenanceCmd constructs the `gc maintenance` parent command. It
// owns three subcommands today: `status` and `dolt-gc` route through the
// supervisor API to drive the weekly Dolt store maintenance loop (see
// docs/adr/0002-dolt-store-maintenance-runbook.md), while
// `install-hooks` writes the city's shared git-hook infrastructure (see
// gc-6o0m). Status and dolt-gc have no local fallback because the
// scheduler ring buffer lives in the supervisor; when the controller is
// down they exit with code 2.
func newMaintenanceCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "City maintenance (Dolt store + git hooks)",
		Long: `Manage city-wide maintenance tasks.

Subcommands:
  status         Show Dolt store maintenance status (supervisor API).
  dolt-gc        Trigger a Dolt store maintenance run (supervisor API).
  install-hooks  Install or refresh the city's prepare-commit-msg hook
                 and the per-rig stub block that delegates to it.

The weekly Dolt loop runs inside the supervisor process when
[maintenance.dolt] enabled=true in city.toml. The hook installer is
filesystem-only and does not require the controller to be up.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc maintenance: missing subcommand (status|dolt-gc|install-hooks)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc maintenance: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newMaintenanceStatusCmd(stdout, stderr))
	cmd.AddCommand(newMaintenanceDoltGCCmd(stdout, stderr))
	cmd.AddCommand(newMaintenanceInstallHooksCmd(stdout, stderr))
	return cmd
}

func newMaintenanceStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show Dolt store maintenance status",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdMaintenanceStatus(jsonOut, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

func newMaintenanceDoltGCCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		wait    bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:          "dolt-gc",
		Short:        "Trigger a Dolt store maintenance run",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdMaintenanceDoltGC(wait, jsonOut, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run completes (exit 1 on failure)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

func cmdMaintenanceStatus(jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	c, reason := maintenanceAPIClient(cityPath)
	return routeMaintenanceStatus(c, reason, jsonOut, stdout, stderr)
}

func cmdMaintenanceDoltGC(wait, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 2
	}
	c, reason := maintenanceAPIClient(cityPath)
	return routeMaintenanceDoltGC(c, reason, wait, jsonOut, stdout, stderr)
}

// maintenanceAPIClient resolves the supervisor API client for the
// maintenance subcommands, or returns (nil, reason) when routing isn't
// available. Indirected through a var so tests inject a client pointed at
// httptest.Server or force a specific fallback reason without spinning up
// a real controller.
var maintenanceAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeMaintenanceStatus dispatches `gc maintenance status` to the
// supervisor API. There is no local fallback (the in-memory ring buffer
// lives in the supervisor), so a nil client exits with code 2 and a
// route=fallback reason=<code> log line documents the reason.
func routeMaintenanceStatus(c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	const cmdName = "maintenance status"
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		fmt.Fprintf(stderr, "gc maintenance status: supervisor not running (%s)\n", nilReason) //nolint:errcheck // best-effort stderr
		return 2
	}
	cr, err := c.GetMaintenanceStatus()
	if err == nil {
		logRoute(stderr, cmdName, "api", "")
		return renderMaintenanceStatus(cr, jsonOut, stdout)
	}
	if api.IsMaintenanceDisabled(err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintln(stderr, "gc maintenance status: "+err.Error()) //nolint:errcheck // best-effort stderr
		return 2
	}
	if !api.ShouldFallbackForRead(err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc maintenance status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	fmt.Fprintf(stderr, "gc maintenance status: supervisor unavailable (%s)\n", api.FallbackReason(err)) //nolint:errcheck // best-effort stderr
	return 2
}

// routeMaintenanceDoltGC dispatches `gc maintenance dolt-gc` to the
// supervisor API. Exit codes match the bead spec: 0 on success/accepted,
// 1 on --wait failure, 2 when the supervisor is unreachable, 3 on 409
// (run already in progress).
func routeMaintenanceDoltGC(c *api.Client, nilReason string, wait, jsonOut bool, stdout, stderr io.Writer) int {
	const cmdName = "maintenance dolt-gc"
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: supervisor not running (%s)\n", nilReason) //nolint:errcheck // best-effort stderr
		return 2
	}
	view, err := c.TriggerMaintenanceDoltGC(wait)
	if err == nil {
		logRoute(stderr, cmdName, "api", "")
		return renderMaintenanceTrigger(view, wait, jsonOut, stdout)
	}
	if api.IsMaintenanceInProgress(err) {
		logRoute(stderr, cmdName, "api", "in-progress")
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 3
	}
	if api.IsMaintenanceDisabled(err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintln(stderr, "gc maintenance dolt-gc: "+err.Error()) //nolint:errcheck // best-effort stderr
		return 2
	}
	if !api.ShouldFallback(err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc maintenance dolt-gc: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	fmt.Fprintf(stderr, "gc maintenance dolt-gc: supervisor unavailable (%s)\n", api.FallbackReason(err)) //nolint:errcheck // best-effort stderr
	return 2
}

// renderMaintenanceStatus prints the status view as human-readable text or
// JSON, appending a stale-read banner when the API cache age exceeds the
// shared threshold. Always returns 0 — status is purely informational.
func renderMaintenanceStatus(cr api.CachedRead[api.MaintenanceStatusView], jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		envelope := map[string]any{
			"status":       cr.Body,
			"_cache_age_s": cr.AgeSeconds,
		}
		_ = writeMaintenanceJSON(stdout, envelope)
		return 0
	}
	v := cr.Body
	enabled := "no"
	if v.Enabled {
		enabled = "yes"
	}
	fmt.Fprintf(stdout, "Maintenance: enabled=%s interval=%s\n", enabled, formatDurationSeconds(v.IntervalSec)) //nolint:errcheck
	if v.InFlight {
		fmt.Fprintf(stdout, "In-flight run started at %s\n", v.InFlightStart) //nolint:errcheck
	}
	if v.LastRun != nil {
		fmt.Fprintf(stdout, "Last run: stage=%s at %s (%.1fs)\n", v.LastRun.Stage, v.LastRun.StartedAt, v.LastRun.DurationSeconds) //nolint:errcheck
		if v.LastRun.Err != "" {
			fmt.Fprintf(stdout, "  error: %s\n", v.LastRun.Err) //nolint:errcheck
		}
	} else {
		fmt.Fprintln(stdout, "Last run: none") //nolint:errcheck
	}
	if v.NextScheduled != "" {
		fmt.Fprintf(stdout, "Next scheduled: %s\n", v.NextScheduled) //nolint:errcheck
	}
	if len(v.History) > 0 {
		fmt.Fprintf(stdout, "History (%d):\n", len(v.History)) //nolint:errcheck
		for _, r := range v.History {
			line := fmt.Sprintf("  %s  stage=%s  duration=%.1fs", r.StartedAt, r.Stage, r.DurationSeconds)
			if r.Err != "" {
				line += "  err=" + truncateMaintenance(r.Err, 80)
			}
			fmt.Fprintln(stdout, line) //nolint:errcheck
		}
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

// renderMaintenanceTrigger prints the trigger outcome and decides the exit
// code. Sync (--wait) returns 1 when the Run's stage names a failing
// phase (anything other than "done"); async always returns 0 when the
// supervisor accepted the request.
func renderMaintenanceTrigger(view api.MaintenanceTriggerView, wait, jsonOut bool, stdout io.Writer) int {
	if jsonOut {
		_ = writeMaintenanceJSON(stdout, view)
	} else {
		if view.Run != nil {
			fmt.Fprintf(stdout, "Maintenance run: stage=%s started=%s duration=%.1fs\n", view.Run.Stage, view.Run.StartedAt, view.Run.DurationSeconds) //nolint:errcheck
			if view.Run.Err != "" {
				fmt.Fprintf(stdout, "  error: %s\n", view.Run.Err) //nolint:errcheck
			}
			if view.Run.SnapshotPath != "" {
				fmt.Fprintf(stdout, "  snapshot: %s\n", view.Run.SnapshotPath) //nolint:errcheck
			}
		} else {
			fmt.Fprintf(stdout, "Maintenance accepted: started_at=%s\n", view.StartedAt) //nolint:errcheck
		}
	}
	if wait && view.Run != nil && view.Run.Stage != "done" {
		return 1
	}
	return 0
}

func formatDurationSeconds(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	d := time.Duration(sec) * time.Second
	return d.String()
}

func writeMaintenanceJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncateMaintenance(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}

func newMaintenanceInstallHooksCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-hooks",
		Short: "Install or refresh the city's git hook infrastructure",
		Long: `Install or refresh the city's shared prepare-commit-msg hook and
the per-rig stub block that delegates to it.

Writes the city hook to <city>/.gc/hooks/prepare-commit-msg and updates
the marker-delimited GASCITY FOOTER block in each rig's active hooks
directory (core.hooksPath if set, else .githooks/). Idempotent — safe to
re-run.

The hook verifies that agent-authored commits include a Claude
co-authorship footer. Behavior is controlled at runtime by
GC_HOOK_FOOTER_MODE (warn|strict|off, default warn).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdMaintenanceInstallHooks(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

// cmdMaintenanceInstallHooks runs the install for the resolved city.
//
// It writes the city hook, then iterates rigs from city.toml (plus the
// HQ city itself, since HQ is also a rig that takes commits) and installs
// or refreshes the per-rig stub.
func cmdMaintenanceInstallHooks(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance install-hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doMaintenanceInstallHooks(githooks.Config{}, cityPath, stdout, stderr)
}

// doMaintenanceInstallHooks is the pure logic, with HooksConfig injected
// for testability. The filesystem is always the OS — config reads and
// hook writes both live on disk in production.
func doMaintenanceInstallHooks(cfg githooks.HooksConfig, cityPath string, stdout, stderr io.Writer) int {
	fs := fsys.OSFS{}
	wrote, err := githooks.WriteCityHook(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance install-hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	hookPath := githooks.CityHookPath(cityPath)
	if wrote {
		fmt.Fprintf(stdout, "wrote city hook %s\n", hookPath) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "city hook up to date %s\n", hookPath) //nolint:errcheck // best-effort stdout
	}

	cityCfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc maintenance install-hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolveRigPaths(cityPath, cityCfg.Rigs)

	// Install in HQ city (it also takes commits).
	rigs := []rigForHooks{{name: cityCfg.EffectiveCityName(), path: cityPath, isHQ: true}}
	for i := range cityCfg.Rigs {
		r := cityCfg.Rigs[i]
		if strings.TrimSpace(r.Path) == "" {
			fmt.Fprintf(stderr, "skipping rig %q: no path\n", r.Name) //nolint:errcheck
			continue
		}
		rigs = append(rigs, rigForHooks{name: r.Name, path: r.Path})
	}

	failures := 0
	for _, rig := range rigs {
		if !isGitRepo(rig.path) {
			fmt.Fprintf(stdout, "  rig %s: skipped (not a git repository)\n", rig.name) //nolint:errcheck
			continue
		}
		res, installErr := githooks.InstallStub(cfg, rig.path, cityPath)
		if installErr != nil {
			tag := ""
			if rig.isHQ {
				tag = " (HQ)"
			}
			fmt.Fprintf(stderr, "  rig %s%s: %v\n", rig.name, tag, installErr) //nolint:errcheck
			failures++
			continue
		}
		fmt.Fprintf(stdout, "  rig %s: %s\n", rig.name, summarizeResult(res)) //nolint:errcheck
	}
	if failures > 0 {
		fmt.Fprintf(stderr, "gc maintenance install-hooks: %d rig(s) failed\n", failures) //nolint:errcheck
		return 1
	}
	return 0
}

// isGitRepo reports whether dir is the root of a git repo or worktree.
// Probes for a `.git` entry — handles both a normal repo (`.git/` dir)
// and a worktree (`.git` file).
func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

type rigForHooks struct {
	name string
	path string
	isHQ bool
}

func summarizeResult(res githooks.InstallStubResult) string {
	switch {
	case !res.WroteFile:
		return "up to date"
	case res.BlockAppended:
		return "stub appended (" + res.HooksDir + ")"
	case res.BlockUpdated:
		return "stub updated (" + res.HooksDir + ")"
	default:
		return "wrote hook (" + res.HooksDir + ")"
	}
}

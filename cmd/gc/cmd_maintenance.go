package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/githooks"
	"github.com/spf13/cobra"
)

func newMaintenanceCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "City maintenance operations (hooks, etc.)",
		Long: `Maintenance commands for keeping a city's rigs in sync with
city-managed infrastructure.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc maintenance: missing subcommand (install-hooks)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc maintenance: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newMaintenanceInstallHooksCmd(stdout, stderr))
	return cmd
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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// crossRigV1Subcommands is the v1 set of bd subcommands that support
// --cross-rig. The merge logic for these is shape-identical (list-style
// output that can be concatenated). Other subcommands either have
// per-issue routing already (show, history, comments) or aggregate-shaped
// output that needs different merge logic (count, status). See gc-din.
var crossRigV1Subcommands = map[string]struct{}{
	"list":    {},
	"ready":   {},
	"blocked": {},
}

// bdSubcommand returns the subcommand from a forwarded bd argument list.
// Returns "" if no subcommand-shaped argument is found (i.e. all args are
// flags, or the list is empty). The first non-flag argument is treated as
// the subcommand.
func bdSubcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// crossRigSupportsSubcommand reports whether --cross-rig supports the given
// bd subcommand in v1.
func crossRigSupportsSubcommand(sub string) bool {
	_, ok := crossRigV1Subcommands[sub]
	return ok
}

// crossRigSubcommandList returns the v1 subcommand set as a comma-separated
// alphabetized string for error messages.
func crossRigSubcommandList() string {
	names := make([]string, 0, len(crossRigV1Subcommands))
	for name := range crossRigV1Subcommands {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// argsContainJSONFlag reports whether the forwarded bd arguments request
// JSON output.
func argsContainJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			return true
		}
	}
	return false
}

// crossRigScope pairs a name with the resolved exec target, preserving
// declaration order so cross-rig output is stable.
type crossRigScope struct {
	Name   string
	Target execStoreTarget
}

// crossRigScopes returns the city scope plus every bound rig in the order
// they appear in city.toml. Unbound rigs are skipped because there is no
// path to route bd against. The city scope is always first.
func crossRigScopes(cityPath string, cfg *config.City) []crossRigScope {
	resolveRigPaths(cityPath, cfg.Rigs)
	scopes := []crossRigScope{
		{Name: cfg.Workspace.Name, Target: bdCityScopeTarget(cityPath, cfg)},
	}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			continue
		}
		scopes = append(scopes, crossRigScope{
			Name:   cfg.Rigs[i].Name,
			Target: bdRigScopeTarget(cityPath, cfg.Rigs[i]),
		})
	}
	return scopes
}

// crossRigResult captures the per-scope outcome of a cross-rig bd invocation.
type crossRigResult struct {
	scope crossRigScope
	out   []byte
	err   error
}

// runCrossRigBd executes `bd <args>` against every scope in `scopes` and
// merges the output. For human output, each rig's stdout is preceded by a
// `=== <name> (<prefix>) ===` header (suppressed for empty rigs). For
// `--json`, the per-rig arrays are concatenated into a single flat array.
//
// On per-rig failure the error is reported to stderr and the iteration
// continues. The final exit code is 0 if at least one scope succeeded,
// and 1 if every scope failed.
func runCrossRigBd(cityPath string, cfg *config.City, scopes []crossRigScope, bdArgs []string, stdout, stderr io.Writer) int {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		fmt.Fprintln(stderr, "gc bd: bd not found in PATH") //nolint:errcheck // best-effort stderr
		return 1
	}

	jsonMode := argsContainJSONFlag(bdArgs)

	results := make([]crossRigResult, 0, len(scopes))
	for _, scope := range scopes {
		// Skip scopes whose provider does not support the bd contract.
		// Don't fail the whole sweep because of one mismatched rig.
		provider := rawBeadsProviderForScope(scope.Target.ScopeRoot, cityPath)
		if !providerUsesBdStoreContract(provider) {
			fmt.Fprintf(stderr, "gc bd: skipping %s (%s): provider %q is not bd-backed\n", //nolint:errcheck // best-effort stderr
				scope.Name, scope.Target.Prefix, provider)
			continue
		}

		warnExternalBdOverrideDrift(stderr, cityPath, scope.Target)

		var out bytes.Buffer
		cmd := exec.Command(bdPath, bdArgs...)
		cmd.Dir = scope.Target.ScopeRoot
		cmd.Stdin = os.Stdin
		cmd.Stdout = &out
		cmd.Stderr = stderr
		cmd.Env = workQueryEnvForDir(bdCommandEnv(cityPath, cfg, scope.Target), cmd.Dir)

		runErr := cmd.Run()
		results = append(results, crossRigResult{scope: scope, out: out.Bytes(), err: runErr})
	}

	if jsonMode {
		return renderCrossRigJSON(results, stdout, stderr)
	}
	return renderCrossRigHuman(results, stdout, stderr)
}

func renderCrossRigJSON(results []crossRigResult, stdout, stderr io.Writer) int {
	merged := make([]json.RawMessage, 0, 32)
	failures := 0
	successes := 0
	for _, res := range results {
		if res.err != nil {
			failures++
			fmt.Fprintf(stderr, "gc bd: cross-rig: %s (%s) failed: %v\n", //nolint:errcheck // best-effort stderr
				res.scope.Name, res.scope.Target.Prefix, res.err)
			continue
		}
		successes++
		trimmed := bytes.TrimSpace(res.out)
		if len(trimmed) == 0 {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			failures++
			fmt.Fprintf(stderr, "gc bd: cross-rig: %s (%s) emitted non-array JSON: %v\n", //nolint:errcheck // best-effort stderr
				res.scope.Name, res.scope.Target.Prefix, err)
			continue
		}
		merged = append(merged, arr...)
	}

	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(merged); err != nil {
		fmt.Fprintf(stderr, "gc bd: cross-rig: encoding merged output: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if successes == 0 && failures > 0 {
		return 1
	}
	return 0
}

func renderCrossRigHuman(results []crossRigResult, stdout, stderr io.Writer) int {
	failures := 0
	successes := 0
	for _, res := range results {
		if res.err != nil {
			failures++
			fmt.Fprintf(stderr, "gc bd: cross-rig: %s (%s) failed: %v\n", //nolint:errcheck // best-effort stderr
				res.scope.Name, res.scope.Target.Prefix, res.err)
			continue
		}
		successes++
		trimmed := bytes.TrimSpace(res.out)
		if len(trimmed) == 0 {
			continue
		}
		fmt.Fprintf(stdout, "=== %s (%s) ===\n", res.scope.Name, res.scope.Target.Prefix) //nolint:errcheck // best-effort stdout
		if _, err := stdout.Write(res.out); err != nil {
			fmt.Fprintf(stderr, "gc bd: cross-rig: writing %s output: %v\n", res.scope.Name, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if !bytes.HasSuffix(res.out, []byte("\n")) {
			fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		}
	}
	if successes == 0 && failures > 0 {
		return 1
	}
	return 0
}

// bdHintTTYStdout reports whether stdout is connected to a terminal. It is
// a package-level variable so tests can override it without depending on
// the actual stdout file descriptor.
var bdHintTTYStdout = func() bool { return isTerminal(os.Stdout) }

// maybeEmitCrossRigHint emits a one-line stderr hint reminding the user
// that --cross-rig exists. The hint fires when:
//   - the bd subcommand supports cross-rig (list/ready/blocked),
//   - the resolved scope is the city store (HQ),
//   - no --rig was passed (the user did not make an explicit choice),
//   - stdout is a terminal (so the hint reaches a human, not a pipe), and
//   - the GC_NO_CROSS_RIG_HINT env var is unset.
//
// The intent is to satisfy the bead's "make scoping obvious" requirement
// without adding noise for scripts.
func maybeEmitCrossRigHint(stderr io.Writer, target execStoreTarget, rigName string, bdArgs []string) {
	if rigName != "" {
		return
	}
	if target.ScopeKind != "city" {
		return
	}
	if !crossRigSupportsSubcommand(bdSubcommand(bdArgs)) {
		return
	}
	if os.Getenv("GC_NO_CROSS_RIG_HINT") == "1" {
		return
	}
	if !bdHintTTYStdout() {
		return
	}
	fmt.Fprintf(stderr, "(showing only %s beads; use --cross-rig for all rigs)\n", target.Prefix) //nolint:errcheck // best-effort stderr
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBdSubcommandReturnsFirstNonFlagArg(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"all flags", []string{"--json", "--limit=5"}, ""},
		{"plain", []string{"list"}, "list"},
		{"flag then sub", []string{"--json", "ready"}, "ready"},
		{"sub then flag", []string{"blocked", "--status=open"}, "blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdSubcommand(tc.in); got != tc.want {
				t.Errorf("bdSubcommand(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCrossRigSupportsSubcommand(t *testing.T) {
	supported := []string{"list", "ready", "blocked"}
	unsupported := []string{"show", "search", "stale", "count", "status", "history", "comments", ""}
	for _, s := range supported {
		if !crossRigSupportsSubcommand(s) {
			t.Errorf("crossRigSupportsSubcommand(%q) = false, want true", s)
		}
	}
	for _, s := range unsupported {
		if crossRigSupportsSubcommand(s) {
			t.Errorf("crossRigSupportsSubcommand(%q) = true, want false", s)
		}
	}
}

func TestCrossRigSubcommandListIsAlphabetized(t *testing.T) {
	got := crossRigSubcommandList()
	want := "blocked, list, ready"
	if got != want {
		t.Errorf("crossRigSubcommandList() = %q, want %q", got, want)
	}
}

func TestArgsContainJSONFlag(t *testing.T) {
	cases := []struct {
		in   []string
		want bool
	}{
		{nil, false},
		{[]string{"list"}, false},
		{[]string{"list", "--json"}, true},
		{[]string{"list", "--json=true"}, true},
		{[]string{"list", "--jsonish"}, false},
		{[]string{"--json", "ready"}, true},
	}
	for _, tc := range cases {
		if got := argsContainJSONFlag(tc.in); got != tc.want {
			t.Errorf("argsContainJSONFlag(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCrossRigScopesCityFirstThenBoundRigs(t *testing.T) {
	cityDir := t.TempDir()
	rigA := filepath.Join(cityDir, "rigs", "a")
	rigB := filepath.Join(cityDir, "rigs", "b")
	if err := os.MkdirAll(rigA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigB, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "a", Path: filepath.Join("rigs", "a"), Prefix: "rapref"},
			{Name: "unbound", Path: "", Prefix: "ub"},
			{Name: "b", Path: filepath.Join("rigs", "b"), Prefix: "rbpref"},
		},
	}

	got := crossRigScopes(cityDir, cfg)
	if len(got) != 3 {
		t.Fatalf("got %d scopes, want 3 (city + a + b); unbound rig should be skipped", len(got))
	}
	if got[0].Name != "demo" || got[0].Target.ScopeKind != "city" {
		t.Errorf("first scope = %+v, want city scope named 'demo'", got[0])
	}
	if got[1].Name != "a" || got[1].Target.Prefix != "rapref" {
		t.Errorf("second scope = %+v, want rig 'a' (rapref)", got[1])
	}
	if got[2].Name != "b" || got[2].Target.Prefix != "rbpref" {
		t.Errorf("third scope = %+v, want rig 'b' (rbpref)", got[2])
	}
}

func TestRenderCrossRigJSONMergesArrays(t *testing.T) {
	results := []crossRigResult{
		{
			scope: crossRigScope{Name: "demo", Target: execStoreTarget{ScopeKind: "city", Prefix: "ga"}},
			out:   []byte(`[{"id":"ga-1"},{"id":"ga-2"}]`),
		},
		{
			scope: crossRigScope{Name: "a", Target: execStoreTarget{ScopeKind: "rig", Prefix: "rapref"}},
			out:   []byte("[]\n"),
		},
		{
			scope: crossRigScope{Name: "b", Target: execStoreTarget{ScopeKind: "rig", Prefix: "rbpref"}},
			out:   []byte(`[{"id":"rbpref-7"}]`),
		},
	}
	var stdout, stderr bytes.Buffer
	if got := renderCrossRigJSON(results, &stdout, &stderr); got != 0 {
		t.Fatalf("renderCrossRigJSON() = %d, want 0; stderr=%q", got, stderr.String())
	}
	var merged []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &merged); err != nil {
		t.Fatalf("unmarshal: %v; out=%q", err, stdout.String())
	}
	if len(merged) != 3 {
		t.Fatalf("len(merged) = %d, want 3; out=%q", len(merged), stdout.String())
	}
	wantIDs := []string{"ga-1", "ga-2", "rbpref-7"}
	for i, want := range wantIDs {
		if got := merged[i]["id"]; got != want {
			t.Errorf("merged[%d].id = %v, want %q", i, got, want)
		}
	}
}

func TestRenderCrossRigJSONReportsRigFailure(t *testing.T) {
	results := []crossRigResult{
		{
			scope: crossRigScope{Name: "demo", Target: execStoreTarget{ScopeKind: "city", Prefix: "ga"}},
			out:   []byte(`[{"id":"ga-1"}]`),
		},
		{
			scope: crossRigScope{Name: "broken", Target: execStoreTarget{ScopeKind: "rig", Prefix: "br"}},
			err:   errors.New("dolt unreachable"),
		},
	}
	var stdout, stderr bytes.Buffer
	if got := renderCrossRigJSON(results, &stdout, &stderr); got != 0 {
		t.Fatalf("renderCrossRigJSON() = %d, want 0 (one success); stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "broken (br) failed: dolt unreachable") {
		t.Errorf("stderr = %q, want failure note for 'broken'", stderr.String())
	}
	var merged []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &merged); err != nil {
		t.Fatalf("unmarshal: %v; out=%q", err, stdout.String())
	}
	if len(merged) != 1 || merged[0]["id"] != "ga-1" {
		t.Errorf("merged = %v, want single entry [ga-1]", merged)
	}
}

func TestRenderCrossRigJSONFailsWhenAllRigsFail(t *testing.T) {
	results := []crossRigResult{
		{scope: crossRigScope{Name: "a", Target: execStoreTarget{Prefix: "a"}}, err: errors.New("boom")},
		{scope: crossRigScope{Name: "b", Target: execStoreTarget{Prefix: "b"}}, err: errors.New("kaboom")},
	}
	var stdout, stderr bytes.Buffer
	if got := renderCrossRigJSON(results, &stdout, &stderr); got != 1 {
		t.Errorf("renderCrossRigJSON() = %d, want 1", got)
	}
}

func TestRenderCrossRigHumanEmitsHeadersForNonEmptyOnly(t *testing.T) {
	results := []crossRigResult{
		{
			scope: crossRigScope{Name: "demo", Target: execStoreTarget{ScopeKind: "city", Prefix: "ga"}},
			out:   []byte("ga-1  open\nga-2  open\n"),
		},
		{
			scope: crossRigScope{Name: "empty", Target: execStoreTarget{ScopeKind: "rig", Prefix: "ep"}},
			out:   []byte("\n"),
		},
		{
			scope: crossRigScope{Name: "b", Target: execStoreTarget{ScopeKind: "rig", Prefix: "rbpref"}},
			out:   []byte("rbpref-7  open"),
		},
	}
	var stdout, stderr bytes.Buffer
	if got := renderCrossRigHuman(results, &stdout, &stderr); got != 0 {
		t.Fatalf("renderCrossRigHuman() = %d, want 0; stderr=%q", got, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "=== demo (ga) ===") {
		t.Errorf("missing demo header; out=%q", out)
	}
	if !strings.Contains(out, "=== b (rbpref) ===") {
		t.Errorf("missing b header; out=%q", out)
	}
	if strings.Contains(out, "=== empty (ep) ===") {
		t.Errorf("empty rig should be suppressed; out=%q", out)
	}
	// Both populated rigs' bodies should be present.
	if !strings.Contains(out, "ga-1") || !strings.Contains(out, "rbpref-7") {
		t.Errorf("missing per-rig bodies; out=%q", out)
	}
}

func TestRenderCrossRigHumanContinuesOnFailure(t *testing.T) {
	results := []crossRigResult{
		{scope: crossRigScope{Name: "broken", Target: execStoreTarget{Prefix: "br"}}, err: errors.New("boom")},
		{scope: crossRigScope{Name: "ok", Target: execStoreTarget{Prefix: "ok"}}, out: []byte("ok-1\n")},
	}
	var stdout, stderr bytes.Buffer
	if got := renderCrossRigHuman(results, &stdout, &stderr); got != 0 {
		t.Fatalf("renderCrossRigHuman() = %d, want 0; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "broken (br) failed: boom") {
		t.Errorf("stderr = %q, want failure note", stderr.String())
	}
	if !strings.Contains(stdout.String(), "=== ok (ok) ===") {
		t.Errorf("stdout = %q, want ok header", stdout.String())
	}
}

func TestMaybeEmitCrossRigHintFiresForCityScope(t *testing.T) {
	old := bdHintTTYStdout
	defer func() { bdHintTTYStdout = old }()
	bdHintTTYStdout = func() bool { return true }
	t.Setenv("GC_NO_CROSS_RIG_HINT", "")

	var stderr bytes.Buffer
	target := execStoreTarget{ScopeKind: "city", Prefix: "ga"}
	maybeEmitCrossRigHint(&stderr, target, "", []string{"list"})
	if !strings.Contains(stderr.String(), "use --cross-rig for all rigs") {
		t.Errorf("stderr = %q, want hint", stderr.String())
	}
	if !strings.Contains(stderr.String(), "ga") {
		t.Errorf("stderr = %q, want prefix", stderr.String())
	}
}

func TestMaybeEmitCrossRigHintSilentInRigScope(t *testing.T) {
	old := bdHintTTYStdout
	defer func() { bdHintTTYStdout = old }()
	bdHintTTYStdout = func() bool { return true }
	t.Setenv("GC_NO_CROSS_RIG_HINT", "")

	var stderr bytes.Buffer
	target := execStoreTarget{ScopeKind: "rig", Prefix: "rapref"}
	maybeEmitCrossRigHint(&stderr, target, "", []string{"list"})
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for rig scope", stderr.String())
	}
}

func TestMaybeEmitCrossRigHintSilentWhenRigFlagPresent(t *testing.T) {
	old := bdHintTTYStdout
	defer func() { bdHintTTYStdout = old }()
	bdHintTTYStdout = func() bool { return true }
	t.Setenv("GC_NO_CROSS_RIG_HINT", "")

	var stderr bytes.Buffer
	target := execStoreTarget{ScopeKind: "city", Prefix: "ga"}
	maybeEmitCrossRigHint(&stderr, target, "explicit-rig", []string{"list"})
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty when --rig was passed", stderr.String())
	}
}

func TestMaybeEmitCrossRigHintSilentForUnsupportedSubcommand(t *testing.T) {
	old := bdHintTTYStdout
	defer func() { bdHintTTYStdout = old }()
	bdHintTTYStdout = func() bool { return true }
	t.Setenv("GC_NO_CROSS_RIG_HINT", "")

	var stderr bytes.Buffer
	target := execStoreTarget{ScopeKind: "city", Prefix: "ga"}
	maybeEmitCrossRigHint(&stderr, target, "", []string{"show", "ga-1"})
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for show subcommand", stderr.String())
	}
}

func TestMaybeEmitCrossRigHintRespectsEnvSuppression(t *testing.T) {
	old := bdHintTTYStdout
	defer func() { bdHintTTYStdout = old }()
	bdHintTTYStdout = func() bool { return true }
	t.Setenv("GC_NO_CROSS_RIG_HINT", "1")

	var stderr bytes.Buffer
	target := execStoreTarget{ScopeKind: "city", Prefix: "ga"}
	maybeEmitCrossRigHint(&stderr, target, "", []string{"list"})
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty when GC_NO_CROSS_RIG_HINT=1", stderr.String())
	}
}

func TestMaybeEmitCrossRigHintSilentWhenStdoutNotTTY(t *testing.T) {
	old := bdHintTTYStdout
	defer func() { bdHintTTYStdout = old }()
	bdHintTTYStdout = func() bool { return false }
	t.Setenv("GC_NO_CROSS_RIG_HINT", "")

	var stderr bytes.Buffer
	target := execStoreTarget{ScopeKind: "city", Prefix: "ga"}
	maybeEmitCrossRigHint(&stderr, target, "", []string{"list"})
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty when stdout is not a TTY", stderr.String())
	}
}

func TestDoBdCrossRigRejectsUnsupportedSubcommand(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"--city", cityDir, "--cross-rig", "show", "ga-1"}, &stdout, &stderr)
	if got != 2 {
		t.Fatalf("doBd() = %d, want 2 for unsupported subcommand; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `--cross-rig is not supported for "show"`) {
		t.Errorf("stderr = %q, want unsupported-subcommand error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "blocked, list, ready") {
		t.Errorf("stderr = %q, want supported-set listing", stderr.String())
	}
}

func TestDoBdCrossRigRejectsRigFlagCombination(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"--city", cityDir, "--rig", "demo", "--cross-rig", "list"}, &stdout, &stderr)
	if got != 2 {
		t.Fatalf("doBd() = %d, want 2; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--rig and --cross-rig are mutually exclusive") {
		t.Errorf("stderr = %q, want mutual-exclusion error", stderr.String())
	}
}

func TestDoBdCrossRigMergesAcrossRigs(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigA := filepath.Join(cityDir, "alpha")
	rigB := filepath.Join(cityDir, "beta")
	for _, dir := range []string{rigA, rigB} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "alpha"
path = "alpha"
prefix = "alpha"

[[rigs]]
name = "beta"
path = "beta"
prefix = "beta"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
# Mock bd: emit a JSON array whose contents depend on cwd.
case "$PWD" in
  */alpha) printf '[{"id":"alpha-1"}]\n' ;;
  */beta) printf '[{"id":"beta-7"},{"id":"beta-8"}]\n' ;;
  *) printf '[{"id":"ga-1"}]\n' ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"--city", cityDir, "--cross-rig", "list", "--json"}, &stdout, &stderr)
	if got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}
	var merged []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &merged); err != nil {
		t.Fatalf("unmarshal merged JSON: %v; out=%q", err, stdout.String())
	}
	wantIDs := []string{"ga-1", "alpha-1", "beta-7", "beta-8"}
	if len(merged) != len(wantIDs) {
		t.Fatalf("merged length = %d (%v), want %d (%v)", len(merged), merged, len(wantIDs), wantIDs)
	}
	for i, want := range wantIDs {
		if merged[i]["id"] != want {
			t.Errorf("merged[%d].id = %v, want %q", i, merged[i]["id"], want)
		}
	}
}

func TestDoBdCrossRigHumanEmitsHeaders(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigA := filepath.Join(cityDir, "alpha")
	if err := os.MkdirAll(filepath.Join(rigA, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "alpha"
path = "alpha"
prefix = "alpha"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
case "$PWD" in
  */alpha) printf 'alpha-1  open\n' ;;
  *) printf 'ga-1  open\n' ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"--city", cityDir, "--cross-rig", "list"}, &stdout, &stderr)
	if got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"=== demo (", "=== alpha (alpha) ===", "ga-1", "alpha-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; out=%q", want, out)
		}
	}
}

func TestDoBdCrossRigContinuesAfterPerRigFailure(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigBad := filepath.Join(cityDir, "broken")
	if err := os.MkdirAll(filepath.Join(rigBad, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "broken"
path = "broken"
prefix = "broken"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
case "$PWD" in
  */broken)
    echo "dolt unreachable" >&2
    exit 73
    ;;
  *) printf '[{"id":"ga-1"}]\n' ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"--city", cityDir, "--cross-rig", "list", "--json"}, &stdout, &stderr)
	if got != 0 {
		t.Fatalf("doBd() = %d, want 0 (city succeeded); stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "broken (broken) failed") {
		t.Errorf("stderr = %q, want broken-rig failure note", stderr.String())
	}
	var merged []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &merged); err != nil {
		t.Fatalf("unmarshal: %v; out=%q", err, stdout.String())
	}
	if len(merged) != 1 || merged[0]["id"] != "ga-1" {
		t.Errorf("merged = %v, want single [ga-1]", merged)
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	legacyOrderConfigFile = "order.toml"
	packHashManifestFile  = ".gc-pack-hashes.json"
)

// builtinPacks lists all packs embedded in the gc binary. These are
// materialized to .gc/system/packs/ on every gc start and gc init.
var builtinPacks = builtinpacks.All()

var builtinPackRefreshCache sync.Map

type builtinPackRefreshState struct {
	mu          sync.Mutex
	ready       bool
	lastWarning string
}

type builtinPackRefreshResult struct {
	ready   bool
	warning error
	fatal   error
}

type builtinPackFile struct {
	data []byte
	perm os.FileMode
}

// MaterializeBuiltinPacks writes all embedded pack files to
// .gc/system/packs/{name}/ in the city directory. Files whose content and mode
// already match are left in place; changed content or mode is repaired with an
// atomic rename so readers never observe a truncated file. Executable scripts
// get 0755; everything else 0644.
//
// Operator edits are preserved only for non-required packs: a regular,
// correct-mode file in a non-required pack is left untouched even when its
// content differs from the embedded bytes (see gastownhall/gascity#2429).
// Required packs (core, maintenance, and the provider-dependent bd/dolt) are
// always refreshed and validated, so a stale or corrupt required pack on disk
// is repaired rather than silently accepted.
// Idempotent: safe to call on every gc start and gc init.
func MaterializeBuiltinPacks(cityPath string) error {
	required := requiredBuiltinPackSet(cityPath)
	for _, bp := range builtinPacks {
		dst := filepath.Join(cityPath, citylayout.SystemPacksRoot, bp.Name)
		_, isRequired := required[bp.Name]
		desired, err := materializeFS(bp.FS, dst, !isRequired, os.Stderr)
		if err != nil {
			return fmt.Errorf("materializing %s pack: %w", bp.Name, err)
		}
		if err := pruneStaleGeneratedPackFiles(dst, desired); err != nil {
			return fmt.Errorf("pruning stale %s pack files: %w", bp.Name, err)
		}
		if err := pruneLegacyEmbeddedOrders(bp.FS, dst); err != nil {
			return fmt.Errorf("pruning legacy %s order paths: %w", bp.Name, err)
		}
	}
	if err := repairLegacyGcBeadsBdScript(cityPath); err != nil {
		return fmt.Errorf("repairing legacy gc-beads-bd script: %w", err)
	}
	if err := repairStaleDoltDogDoctorScript(cityPath); err != nil {
		return fmt.Errorf("repairing stale dolt dog-doctor script: %w", err)
	}
	return nil
}

func builtinPackIncludesForConfigLoad(fs fsys.FS, tomlPath string, warningWriter io.Writer) ([]string, error) {
	if !usesOSFS(fs) {
		return nil, nil
	}
	cityPath := filepath.Dir(tomlPath)
	if err := ensureBuiltinPacksReadyForConfigLoad(cityPath, warningWriter); err != nil {
		return nil, err
	}
	return builtinPackIncludes(cityPath), nil
}

func usesOSFS(fs fsys.FS) bool {
	switch fs.(type) {
	case fsys.OSFS, *fsys.OSFS:
		return true
	default:
		return false
	}
}

func ensureBuiltinPacksReadyForConfigLoad(cityPath string, warningWriter io.Writer) error {
	key := normalizePathForCompare(cityPath)
	stateAny, _ := builtinPackRefreshCache.LoadOrStore(key, &builtinPackRefreshState{})
	state := stateAny.(*builtinPackRefreshState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ready {
		if len(unusableRequiredBuiltinPackNames(cityPath)) == 0 {
			return nil
		}
		state.ready = false
	}
	result := materializeBuiltinPacksForConfigLoad(cityPath)
	if result.fatal != nil {
		state.lastWarning = ""
		return result.fatal
	}
	if result.warning != nil {
		const warningKey = "builtin-pack-refresh-incomplete"
		if state.lastWarning != warningKey {
			emitBuiltinPackRefreshWarning(warningWriter, result.warning)
			state.lastWarning = warningKey
		}
		return nil
	}
	if result.ready {
		state.ready = true
		state.lastWarning = ""
	}
	return nil
}

func materializeBuiltinPacksForConfigLoad(cityPath string) builtinPackRefreshResult {
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		if missing := unusableRequiredBuiltinPackNames(cityPath); len(missing) > 0 {
			return builtinPackRefreshResult{
				fatal: fmt.Errorf("materializing builtin packs: required builtin packs remain unusable (%s): %w", strings.Join(missing, ", "), err),
			}
		}
		return builtinPackRefreshResult{
			warning: fmt.Errorf("builtin pack refresh incomplete; using existing materialized packs: %w", err),
		}
	}
	return builtinPackRefreshResult{ready: true}
}

func unusableRequiredBuiltinPackNames(cityPath string) []string {
	systemRoot := filepath.Join(cityPath, citylayout.SystemPacksRoot)
	var missing []string
	for _, name := range requiredBuiltinPackNames(cityPath) {
		bp, ok := builtinPackByName(name)
		if !ok || !packContainsEmbeddedState(bp.FS, filepath.Join(systemRoot, name)) {
			missing = append(missing, name)
		}
	}
	return missing
}

func builtinPackByName(name string) (builtinpacks.Pack, bool) {
	for _, bp := range builtinPacks {
		if bp.Name == name {
			return bp, true
		}
	}
	return builtinpacks.Pack{}, false
}

func packContainsEmbeddedState(embedded fs.FS, dstDir string) bool {
	manifest, err := embeddedPackManifest(embedded)
	if err != nil {
		return false
	}
	return packContainsEmbeddedManifest(manifest, dstDir)
}

func packContainsEmbeddedManifest(manifest map[string]builtinPackFile, dstDir string) bool {
	fi, err := os.Stat(dstDir)
	if err != nil || !fi.IsDir() {
		return false
	}
	for rel, want := range manifest {
		dstPath := filepath.Join(dstDir, filepath.FromSlash(rel))
		info, err := os.Lstat(dstPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != want.perm {
			return false
		}
		got, err := os.ReadFile(dstPath)
		if err != nil || !bytes.Equal(got, want.data) {
			return false
		}
	}
	return true
}

func embeddedPackManifest(embedded fs.FS) (map[string]builtinPackFile, error) {
	manifest := make(map[string]builtinPackFile)
	err := fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}
		manifest[filepath.ToSlash(path)] = builtinPackFile{
			data: data,
			perm: builtinpacks.MaterializedFileMode(path),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// requiredBuiltinPackSet returns the set of builtin pack names that must stay
// in lockstep with the embedded bytes for the city at cityPath. Required packs
// are refreshed and validated on every materialize; operator edits to them are
// not preserved. Derived from requiredBuiltinPackNames so the set tracks the
// provider-dependent membership (bd/dolt) exactly.
func requiredBuiltinPackSet(cityPath string) map[string]struct{} {
	names := requiredBuiltinPackNames(cityPath)
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

func requiredBuiltinPackNames(cityPath string) []string {
	required := []string{"core", "maintenance"}

	provider := strings.TrimSpace(configuredBeadsProviderValue(cityPath))
	normalizedProvider := normalizeRawBeadsProvider(cityPath, provider)
	if providerUsesBdStoreContract(normalizedProvider) {
		required = append(required, "bd")
	}
	usesDirectExecLifecycle := strings.HasPrefix(provider, "exec:") &&
		execProviderBase(provider) == "gc-beads-bd" &&
		normalizedProvider != "bd"
	if usesDirectExecLifecycle {
		required = append(required, "dolt")
	}
	return required
}

func emitBuiltinPackRefreshWarning(w io.Writer, err error) {
	if w == nil || err == nil {
		return
	}
	fmt.Fprintf(w, "warning: %v\n", err) //nolint:errcheck // best-effort warning emission
}

// builtinPackIncludes returns the system pack paths that should be
// auto-included in config loading. These are appended as extraIncludes
// to LoadWithIncludes so they go through normal pack expansion
// (ExpandCityPacks) with dedup/fallback resolution.
//
// Core and maintenance are always included. Core ships the role prompts
// referenced by implicit agents and the overlay/per-provider hook files,
// so its content must reach PackOverlayDirs even when the user has never
// run `gc init` (and therefore has no implicit-import.toml written to
// $GC_HOME). When the beads provider is "bd" (the default), include bd
// and let its own pack includes pull in dolt transitively. Gastown is
// never auto-included — it requires an explicit workspace.includes entry.
func builtinPackIncludes(cityPath string) []string {
	systemRoot := filepath.Join(cityPath, citylayout.SystemPacksRoot)

	var includes []string
	for _, name := range requiredBuiltinPackNames(cityPath) {
		packPath := filepath.Join(systemRoot, name)
		if packExists(packPath) {
			includes = append(includes, packPath)
		}
	}

	return includes
}

// packExists checks if a pack.toml exists in the given directory.
func packExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pack.toml"))
	return err == nil
}

// peekBeadsProvider reads just the beads.provider field from a city.toml
// without doing full config parsing. Returns "" if not set or on error.
func peekBeadsProvider(tomlPath string) string {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var peek struct {
		Beads struct {
			Provider string `toml:"provider"`
			Backend  string `toml:"backend"`
		} `toml:"beads"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Beads.Provider
}

func peekBeadsBackend(tomlPath string) string {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var peek struct {
		Beads struct {
			Backend string `toml:"backend"`
		} `toml:"beads"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Beads.Backend
}

// peekEventsProvider reads just the events.provider field from a city.toml
// without doing full config parsing. Returns "" if not set or on error.
//
// Used by gc event emit (called from bd hooks on every bead write) to avoid
// the full loadCityConfig path, which resolves [imports] and runs
// `git status --porcelain --ignored` against every cached pack-source repo
// — slow on hosts where a pack source is a large monorepo, and fan-out
// concurrent across a bd-write burst (see gastownhall/gascity#2099).
//
// Trade-off: include/import/pack-provided overrides of [events].provider are
// not honored on this hook fast path. Operators that need this path to bypass
// city.toml should use the GC_EVENTS env var.
func peekEventsProvider(tomlPath string) string {
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	var peek struct {
		Events struct {
			Provider string `toml:"provider"`
		} `toml:"events"`
	}
	if _, err := toml.Decode(string(data), &peek); err != nil {
		return ""
	}
	return peek.Events.Provider
}

// materializeFS walks an embed.FS, writes all files to dstDir, and returns the
// relative file paths that belong in the generated directory.
//
// When preserveOperatorEdits is true, a per-pack hash manifest
// (.gc-pack-hashes.json) distinguishes stale embedded content from operator
// edits. A file whose on-disk hash matches the last binary-written hash is
// stale and refreshed silently. A file whose on-disk hash differs from the
// manifest entry has been operator-edited and is preserved with a warning. A
// file with no manifest entry is conservatively preserved without a warning
// (migration path for cities without a prior manifest).
//
// When preserveOperatorEdits is false (required packs), every file is refreshed
// and validated against the embedded bytes regardless of the manifest.
//
// The manifest is written after a successful walk even when the merged map is
// empty; write failures are non-fatal and surface through w. The manifest file
// itself is not included in the returned desired set.
//
// The remaining repair semantics are independent of the flag: missing files are
// written (initial scaffolding), wrong-mode files are rewritten (e.g., script
// that lost its +x bit), and non-regular files (symlinks, etc.) are replaced
// with the embedded content.
func materializeFS(embedded fs.FS, dstDir string, preserveOperatorEdits bool, w io.Writer) (map[string]struct{}, error) {
	existingManifest := readPackHashManifest(dstDir)
	pendingManifest := make(map[string]string)
	desired := make(map[string]struct{})

	walkErr := fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		dst := filepath.Join(dstDir, path)

		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}

		rel := filepath.ToSlash(path)
		desired[rel] = struct{}{}

		perm := builtinpacks.MaterializedFileMode(path)

		// For non-required packs, use the hash manifest to distinguish stale
		// embedded content from operator edits. Mode comparison uses
		// fsys.ComparableMode (perm + setuid/setgid/sticky) so it agrees with
		// the WriteFileIfContentOrModeChangedAtomic repair path below.
		if preserveOperatorEdits {
			if info, statErr := os.Lstat(dst); statErr == nil {
				if info.Mode().IsRegular() && fsys.ComparableMode(info.Mode()) == fsys.ComparableMode(perm) {
					if knownHash, ok := existingManifest[rel]; ok {
						onDiskData, readErr := os.ReadFile(dst)
						if readErr != nil {
							return fmt.Errorf("reading %s for hash comparison: %w", dst, readErr)
						}
						if sha256Hex(onDiskData) != knownHash {
							// On-disk content differs from last binary-written hash: operator edit.
							emitBuiltinPackRefreshWarning(w, fmt.Errorf("file %s has local edits; newer version available in the binary", rel))
							pendingManifest[rel] = knownHash
							return nil
						}
						// On-disk hash matches manifest: stale embed, fall through to refresh.
					} else {
						// No manifest entry: a pre-manifest deployment is
						// indistinguishable from an operator edit, except when
						// the on-disk bytes equal the embedded bytes. Adopting
						// that provably-unedited file into the manifest lets
						// future embedded updates refresh it instead of
						// freezing it at the pre-manifest content (gc-3p4).
						// Differing content is conservatively preserved
						// without a warning.
						onDiskData, readErr := os.ReadFile(dst)
						if readErr != nil {
							return fmt.Errorf("reading %s for embedded comparison: %w", dst, readErr)
						}
						embeddedData, embErr := fs.ReadFile(embedded, path)
						if embErr != nil {
							return fmt.Errorf("reading embedded %s: %w", path, embErr)
						}
						if bytes.Equal(onDiskData, embeddedData) {
							pendingManifest[rel] = sha256Hex(embeddedData)
						}
						return nil
					}
				}
				// Wrong mode or non-regular: fall through to repair.
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("stat %s: %w", dst, statErr)
			}
		}

		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}

		pendingManifest[rel] = sha256Hex(data)

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, dst, data, perm)
	})

	if walkErr != nil {
		return nil, walkErr
	}

	if writeErr := writePackHashManifest(dstDir, pendingManifest); writeErr != nil {
		emitBuiltinPackRefreshWarning(w, fmt.Errorf("could not write pack hash manifest: %w", writeErr))
	}

	return desired, nil
}

// sha256Hex returns the hex-encoded SHA-256 digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readPackHashManifest reads the pack hash manifest from dstDir. Returns an
// empty map when the manifest is absent or contains invalid JSON.
func readPackHashManifest(dstDir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dstDir, packHashManifestFile))
	if err != nil {
		return map[string]string{}
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return map[string]string{}
	}
	if manifest == nil {
		return map[string]string{}
	}
	return manifest
}

// writePackHashManifest writes manifest to dstDir/.gc-pack-hashes.json
// atomically. The caller is responsible for treating write errors as non-fatal.
func writePackHashManifest(dstDir string, manifest map[string]string) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshaling pack hash manifest: %w", err)
	}
	dst := filepath.Join(dstDir, packHashManifestFile)
	return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, dst, data, 0o644)
}

func repairLegacyGcBeadsBdScript(cityPath string) error {
	path := legacyGcBeadsBdScriptPath(cityPath)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !looksLikeGeneratedGcBeadsBdScript(data) {
		return nil
	}
	return fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, path, legacyGcBeadsBdShim(), 0o755)
}

func looksLikeGeneratedGcBeadsBdScript(data []byte) bool {
	text := string(data)
	return strings.Contains(text, "gc-beads-bd") && strings.Contains(text, "exec: beads provider")
}

func legacyGcBeadsBdShim() []byte {
	return []byte(`#!/bin/sh
set -eu

script_dir=$(dirname "$0")
city_root=$(cd "$script_dir/../.." && pwd)

exec "$city_root/.gc/system/packs/bd/assets/scripts/gc-beads-bd.sh" "$@"
`)
}

// doltDogDoctorScriptRel is the dolt pack-relative path of the dog-doctor
// health-check script targeted by repairStaleDoltDogDoctorScript.
const doltDogDoctorScriptRel = "assets/scripts/mol-dog-doctor.sh"

// preManifestDoltDogDoctorSHA256s holds the SHA-256 digest of every embedded
// revision of the dolt pack's mol-dog-doctor.sh that shipped before the pack
// hash manifest existed (#3173). A materialized copy with no manifest entry
// whose digest matches one of these is provably stale binary-generated
// content, not an operator edit, so it is safe to refresh. Pre-manifest
// cities were otherwise frozen on a dog-doctor without the backup-eligibility
// gate and mailed false "backup missing" advisories every patrol cycle
// (gc-3p4). The set is closed: revisions shipped after the manifest feature
// always have manifest entries, so do not add new digests.
var preManifestDoltDogDoctorSHA256s = map[string]struct{}{
	"24fd8da6eecc33d17bac81bbcc6201f11ca65bbfd690d8f1ed1b5676f7fdcf76": {}, // 2026-05-09 edce24c70
	"d52394fb1f6e9c981b605bb7a70a0b693ebeb20acde7fc876af0988e0625718e": {}, // 2026-05-10 f94c244a1
	"a2efcdeb712072a443de70904d6541988b50c10333cd4634378b7c8b9dafeefc": {}, // 2026-05-17 6e25d7b3f
	"68d00ca6e05cfa63c575042c7b6ca0530d062125ce0b7e8a6dff14c2371bcdda": {}, // 2026-05-20 8f268621f
	"ffdeb759892e7e577b7171a14b5f7f9cb87f27e8f2daa05f1f1e3bb4539311ff": {}, // 2026-05-25 af092a478
	"8e21f76e827168b088635f1045bea15c2c26c2faf1cc713bb842d8ec7232d94c": {}, // 2026-06-01 4fb0c6f52
}

// repairStaleDoltDogDoctorScript refreshes a materialized dolt dog-doctor
// script that a pre-manifest binary wrote and the manifest migration path
// then froze (no manifest entry means materializeFS preserves the file as a
// potential operator edit). Only content whose digest matches a known
// pre-manifest embedded revision is rewritten; anything else — including
// genuine operator edits — is left untouched. The refreshed file gets a
// manifest entry so the normal refresh flow owns it from then on.
func repairStaleDoltDogDoctorScript(cityPath string) error {
	packDir := filepath.Join(cityPath, citylayout.SystemPacksRoot, "dolt")
	dst := filepath.Join(packDir, filepath.FromSlash(doltDogDoctorScriptRel))
	info, err := os.Lstat(dst)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	manifest := readPackHashManifest(packDir)
	if _, tracked := manifest[doltDogDoctorScriptRel]; tracked {
		return nil
	}
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	if _, stale := preManifestDoltDogDoctorSHA256s[sha256Hex(onDisk)]; !stale {
		return nil
	}
	bp, ok := builtinPackByName("dolt")
	if !ok {
		return fmt.Errorf("builtin dolt pack missing from registry")
	}
	data, err := fs.ReadFile(bp.FS, doltDogDoctorScriptRel)
	if err != nil {
		return fmt.Errorf("reading embedded dolt %s: %w", doltDogDoctorScriptRel, err)
	}
	perm := builtinpacks.MaterializedFileMode(doltDogDoctorScriptRel)
	if err := fsys.WriteFileIfContentOrModeChangedAtomic(fsys.OSFS{}, dst, data, perm); err != nil {
		return err
	}
	manifest[doltDogDoctorScriptRel] = sha256Hex(data)
	return writePackHashManifest(packDir, manifest)
}

// pruneLegacyEmbeddedOrders removes deprecated order directory layouts when the
// embedded pack already provides the flat orders/<name>.toml form.
func pruneLegacyEmbeddedOrders(embedded fs.FS, dstDir string) error {
	entries, err := fs.ReadDir(embedded, "orders")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		orderName, ok := orders.TrimFlatOrderFilename(name)
		if !ok {
			continue
		}
		for _, legacyPath := range []string{
			filepath.Join(dstDir, "orders", orderName, legacyOrderConfigFile),
			filepath.Join(dstDir, "formulas", "orders", orderName, legacyOrderConfigFile),
		} {
			if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			pruneEmptyDirs(filepath.Dir(legacyPath), dstDir)
		}
	}
	return nil
}

// pruneStaleGeneratedPackFiles treats the current binary's embedded pack tree
// as the source of truth for generated files. Concurrent older/newer binaries
// can briefly prune each other's obsolete generated-only files, but the next
// successful materialization self-heals the directory to the active binary.
func pruneStaleGeneratedPackFiles(dstDir string, desired map[string]struct{}) error {
	if _, err := os.Stat(dstDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	dirsToPrune := make(map[string]struct{})
	if err := filepath.WalkDir(dstDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dstDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := desired[rel]; ok {
			return nil
		}
		// Ignore in-flight atomic temp files so concurrent refreshes do not
		// delete each other's rename targets mid-write.
		if isGeneratedPackAtomicTempRel(rel, func(path string) bool {
			_, ok := desired[path]
			return ok
		}) {
			return nil
		}
		// Preserve the pack hash manifest and its atomic temp siblings — they
		// are runtime metadata produced by materializeFS, not embedded content.
		if rel == packHashManifestFile || strings.HasPrefix(rel, packHashManifestFile+".tmp.") {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		dirsToPrune[filepath.Dir(path)] = struct{}{}
		return nil
	}); err != nil {
		return err
	}

	pruneDirs := make([]string, 0, len(dirsToPrune))
	for dir := range dirsToPrune {
		pruneDirs = append(pruneDirs, dir)
	}
	sort.Slice(pruneDirs, func(i, j int) bool {
		left := filepath.Clean(pruneDirs[i])
		right := filepath.Clean(pruneDirs[j])
		leftDepth := strings.Count(left, string(filepath.Separator))
		rightDepth := strings.Count(right, string(filepath.Separator))
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return left > right
	})
	for _, dir := range pruneDirs {
		pruneEmptyDirs(dir, dstDir)
	}
	return nil
}

func isGeneratedPackAtomicTempRel(rel string, hasDesired func(string) bool) bool {
	idx := strings.LastIndex(rel, ".tmp.")
	return idx > 0 && hasDesired(rel[:idx])
}

func pruneEmptyDirs(dir, stop string) {
	stop = filepath.Clean(stop)
	for {
		cleanDir := filepath.Clean(dir)
		if cleanDir == stop || cleanDir == "." || cleanDir == string(filepath.Separator) {
			return
		}
		entries, err := os.ReadDir(cleanDir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(cleanDir); err != nil {
			return
		}
		dir = filepath.Dir(cleanDir)
	}
}

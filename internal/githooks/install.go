package githooks

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// HooksConfig abstracts git's `core.hooksPath` lookup and write so the
// installer can be tested without shelling out. Production callers use
// [GitHooksConfig], which delegates to the `git` binary.
type HooksConfig interface {
	// GetHooksPath returns the value of `core.hooksPath` for the rig at
	// rigPath. If the key is unset, returns ("", false, nil).
	GetHooksPath(rigPath string) (value string, set bool, err error)
	// SetHooksPath sets `core.hooksPath` for the rig at rigPath.
	SetHooksPath(rigPath, value string) error
}

// CityHookPath returns the absolute path to the city's shared hook file
// for cityPath.
func CityHookPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "hooks", "prepare-commit-msg")
}

// WriteCityHook writes (or refreshes) the embedded city hook script at
// `<cityPath>/.gc/hooks/prepare-commit-msg`. The file is marked
// executable. Idempotent — a no-op if the on-disk content already
// matches the embedded script.
//
// Returns true if the file was written or updated.
func WriteCityHook(cityPath string) (bool, error) {
	if cityPath == "" {
		return false, errors.New("WriteCityHook: empty cityPath")
	}
	dst := CityHookPath(cityPath)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", dir, err)
	}
	desired := CityHookScript()
	existing, readErr := os.ReadFile(dst)
	if readErr == nil && bytes.Equal(existing, desired) {
		// Even when content is up-to-date, ensure the executable bit is set.
		if err := os.Chmod(dst, 0o755); err != nil {
			return false, fmt.Errorf("chmod %s: %w", dst, err)
		}
		return false, nil
	}
	if err := atomicWrite(dst, desired, 0o755); err != nil {
		return false, fmt.Errorf("writing %s: %w", dst, err)
	}
	return true, nil
}

// InstallStubResult describes what `InstallStub` did.
type InstallStubResult struct {
	HooksDir      string // resolved active hooks dir for the rig
	HookFile      string // absolute path to prepare-commit-msg
	CreatedDir    bool   // true if HooksDir had to be created
	SetHooksPath  bool   // true if core.hooksPath had to be set
	WroteFile     bool   // true if HookFile content changed
	BlockUpdated  bool   // true if the marker block was replaced
	BlockAppended bool   // true if the marker block was newly appended
}

// InstallStub installs (or updates) the per-rig stub block in the rig's
// active `prepare-commit-msg`.
//
// Behavior:
//   - Resolves the active hooks dir: `git config --get core.hooksPath` if
//     set, else `<rigPath>/.githooks`. If the resolved dir does not exist,
//     it is created and `core.hooksPath` is set to `.githooks` (relative,
//     matching gascity's convention).
//   - Reads any existing `prepare-commit-msg` and finds the GASCITY
//     marker block. Replaces it in place if present; appends it
//     otherwise. Preserves all other content (including bd's marker
//     block).
//   - Ensures the file starts with a `#!/usr/bin/env sh` shebang.
//   - Ensures the file is executable.
//
// Idempotent: re-running with the same cityPath produces no further
// changes.
func InstallStub(cfg HooksConfig, rigPath, cityPath string) (InstallStubResult, error) {
	var res InstallStubResult
	if rigPath == "" {
		return res, errors.New("InstallStub: empty rigPath")
	}
	if cityPath == "" {
		return res, errors.New("InstallStub: empty cityPath")
	}

	// 1) Resolve active hooks dir.
	hooksDir, createdDir, setPath, err := resolveHooksDir(cfg, rigPath)
	if err != nil {
		return res, err
	}
	res.HooksDir = hooksDir
	res.CreatedDir = createdDir
	res.SetHooksPath = setPath

	// 2) Read existing hook, if any.
	hookFile := filepath.Join(hooksDir, "prepare-commit-msg")
	res.HookFile = hookFile
	existing, readErr := os.ReadFile(hookFile)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return res, fmt.Errorf("reading %s: %w", hookFile, readErr)
	}

	// 3) Compose new content with stub block updated/appended.
	stub := stubBlock(cityPath)
	updated, appended := upsertMarkerBlock(existing, stub)

	// 4) Ensure shebang.
	updated = ensureShebang(updated)

	if bytes.Equal(updated, existing) {
		// Even when content is unchanged, ensure executable.
		if err := os.Chmod(hookFile, 0o755); err != nil {
			return res, fmt.Errorf("chmod %s: %w", hookFile, err)
		}
		return res, nil
	}

	if err := atomicWrite(hookFile, updated, 0o755); err != nil {
		return res, fmt.Errorf("writing %s: %w", hookFile, err)
	}
	res.WroteFile = true
	res.BlockUpdated = !appended && len(existing) > 0 && hasGascityBlock(existing)
	res.BlockAppended = appended
	return res, nil
}

// stubBlock returns the marker-delimited stub content (BEGIN line through
// END line, terminated by newline) tailored for the given cityPath.
func stubBlock(cityPath string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# --- BEGIN GASCITY FOOTER %s ---\n", MarkerVersion)
	b.WriteString("# This section is managed by gc. Do not remove these markers.\n")
	fmt.Fprintf(&b, "gc_city_hook=\"${GC_CITY:-%s}/.gc/hooks/prepare-commit-msg\"\n", cityPath)
	b.WriteString("if [ -x \"$gc_city_hook\" ]; then\n")
	b.WriteString("    \"$gc_city_hook\" \"$@\"\n")
	b.WriteString("    _gc_exit=$?\n")
	b.WriteString("    if [ $_gc_exit -ne 0 ]; then exit $_gc_exit; fi\n")
	b.WriteString("fi\n")
	fmt.Fprintf(&b, "# --- END GASCITY FOOTER %s ---\n", MarkerVersion)
	return b.Bytes()
}

// upsertMarkerBlock inserts or replaces the GASCITY FOOTER block in src.
//
// If src contains an existing block (any version), it is replaced in
// place with stub. Otherwise stub is appended at end-of-file (preserving
// trailing whitespace minimally — adds a single newline before the block
// if src does not already end in one).
//
// Returns (updated, appended) where appended is true only when the block
// was newly added rather than replaced.
func upsertMarkerBlock(src, stub []byte) (updated []byte, appended bool) {
	if len(src) == 0 {
		// Caller will prepend shebang via ensureShebang.
		return append([]byte{}, stub...), true
	}

	beginIdx, endIdx, ok := findGascityBlock(src)
	if ok {
		var out bytes.Buffer
		out.Write(src[:beginIdx])
		out.Write(stub)
		// endIdx points at the newline after END marker line; copy from there.
		out.Write(src[endIdx:])
		return out.Bytes(), false
	}

	// Append. Ensure src ends with a newline so stub starts on its own line.
	var out bytes.Buffer
	out.Write(src)
	if !bytes.HasSuffix(src, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.Write(stub)
	return out.Bytes(), true
}

// findGascityBlock returns the byte indices of the BEGIN line's start
// and the byte just past the END line's terminating newline. Returns
// ok=false if no block is found.
func findGascityBlock(src []byte) (beginStart, endPast int, ok bool) {
	scanner := bufio.NewScanner(bytes.NewReader(src))
	// Allow long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	pos := 0
	beginLineStart := -1
	for scanner.Scan() {
		line := scanner.Bytes()
		// scanner strips the newline; account for it in pos tracking.
		lineLen := len(line) + 1
		if beginLineStart < 0 {
			if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte(MarkerBegin)) {
				beginLineStart = pos
			}
		} else {
			if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte(MarkerEnd)) {
				return beginLineStart, pos + lineLen, true
			}
		}
		pos += lineLen
	}
	return 0, 0, false
}

// hasGascityBlock reports whether src contains a complete BEGIN..END block.
func hasGascityBlock(src []byte) bool {
	_, _, ok := findGascityBlock(src)
	return ok
}

// ensureShebang ensures src begins with a `#!/usr/bin/env sh` shebang.
// If src already starts with any `#!` line, it is left intact.
func ensureShebang(src []byte) []byte {
	if bytes.HasPrefix(src, []byte("#!")) {
		return src
	}
	const sh = "#!/usr/bin/env sh\n"
	out := make([]byte, 0, len(sh)+len(src))
	out = append(out, sh...)
	out = append(out, src...)
	return out
}

// resolveHooksDir determines the active hooks directory for the rig.
//
// If `core.hooksPath` is set (relative paths resolved against rigPath),
// returns that. Otherwise, defaults to `<rigPath>/.githooks`. If the
// resolved directory does not exist, creates it and (when defaulting)
// sets `core.hooksPath` to `.githooks` so the directory becomes active.
func resolveHooksDir(cfg HooksConfig, rigPath string) (dir string, createdDir, setPath bool, err error) {
	configured, set, err := cfg.GetHooksPath(rigPath)
	if err != nil {
		return "", false, false, fmt.Errorf("reading core.hooksPath in %s: %w", rigPath, err)
	}
	if set {
		dir = configured
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(rigPath, dir)
		}
	} else {
		dir = filepath.Join(rigPath, ".githooks")
	}

	if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
		return dir, false, false, nil
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", false, false, fmt.Errorf("stat %s: %w", dir, statErr)
	}

	// Need to create the directory.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, false, fmt.Errorf("creating %s: %w", dir, err)
	}
	createdDir = true

	if !set {
		// We defaulted to .githooks; activate it.
		if err := cfg.SetHooksPath(rigPath, ".githooks"); err != nil {
			return "", false, false, fmt.Errorf("setting core.hooksPath in %s: %w", rigPath, err)
		}
		setPath = true
	}
	return dir, createdDir, setPath, nil
}

// atomicWrite writes data to dst via a temp file in the same directory
// followed by rename. Sets mode after writing.
func atomicWrite(dst string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*-prepare-commit-msg")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() {
		// Best-effort cleanup if rename failed.
		_ = os.Remove(tmpName)
	}()
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return nil
}

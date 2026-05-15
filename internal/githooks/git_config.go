package githooks

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Config is a [HooksConfig] that shells out to the `git` binary.
// It honors the rig path by setting `cmd.Dir` rather than passing
// `-C <path>` so behavior matches what would happen during a normal
// commit inside the rig.
type Config struct{}

// GetHooksPath returns the value of `core.hooksPath` for the rig, or
// (set=false) when the key is unset. Errors only on unexpected git
// failures; an unset key is not an error.
func (Config) GetHooksPath(rigPath string) (string, bool, error) {
	cmd := exec.Command("git", "config", "--get", "core.hooksPath")
	cmd.Dir = rigPath
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(out.String()), true, nil
	}
	// `git config --get` exits 1 when the key is unset. Distinguish that
	// from real failures by checking the exit code.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", false, nil
	}
	return "", false, fmt.Errorf("git config --get core.hooksPath: %w", err)
}

// SetHooksPath sets `core.hooksPath` for the rig to the given value.
func (Config) SetHooksPath(rigPath, value string) error {
	cmd := exec.Command("git", "config", "core.hooksPath", value)
	cmd.Dir = rigPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git config core.hooksPath %q in %s: %w (stderr: %s)", value, rigPath, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

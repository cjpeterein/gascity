// Package githooks installs the city's shared `prepare-commit-msg` hook
// and the per-rig stub that delegates to it.
//
// The city hook (embedded as scripts/prepare-commit-msg.sh) verifies that
// agent-authored commits carry a Claude co-authorship footer. The per-rig
// stub is a marker-delimited block that lives in each rig's active
// `prepare-commit-msg` (alongside any bd-managed block) and exec's the
// city hook.
//
// See bead gc-6o0m for the design rationale (three-layer enforcement:
// agent-side wrapper writes the footer, this hook verifies it, CI
// backstops both).
package githooks

import _ "embed"

// MarkerVersion is the version embedded in the BEGIN/END markers of the
// per-rig stub block. Bumping this triggers an in-place replacement on
// the next install run; older marker versions are recognized and
// upgraded.
const MarkerVersion = "v1.0"

// MarkerBegin is the prefix of the BEGIN marker line. Combined with
// MarkerVersion to detect any version of the stub block.
const MarkerBegin = "# --- BEGIN GASCITY FOOTER"

// MarkerEnd is the prefix of the END marker line.
const MarkerEnd = "# --- END GASCITY FOOTER"

//go:embed scripts/prepare-commit-msg.sh
var cityHookScript []byte

// CityHookScript returns the embedded city `prepare-commit-msg` content.
// Exported for tests and for callers that want to inspect what would be
// written without touching the filesystem.
func CityHookScript() []byte {
	out := make([]byte, len(cityHookScript))
	copy(out, cityHookScript)
	return out
}

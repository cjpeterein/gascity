#!/bin/sh
# status-line.sh — status text producer for Gas City agents.
# Usage: status-line.sh <agent-name>
# Called by the status-updater.sh sidecar at a bounded cadence; the text is
# published to the @gc_status_line session option that status-right reads
# (gc-46q: data production stays off the tmux render path).
# Always exits 0 — callers must never see errors.

agent="$1"
[ -z "$agent" ] && exit 0

# Trace gc hook / gc mail check invocations to $GC_BD_TRACE when set.
# The helper lives in the maintenance pack scripts dir; if it's not
# reachable, status-line continues without tracing.
__bd_trace_helper=""
for __cand in \
    "${GC_CITY_PATH:-}/.gc/system/packs/maintenance/assets/scripts/_bd_trace.sh" \
    "${GC_CITY:-}/.gc/system/packs/maintenance/assets/scripts/_bd_trace.sh"; do
    if [ -n "$__cand" ] && [ -f "$__cand" ]; then
        __bd_trace_helper="$__cand"
        break
    fi
done
if [ -n "$__bd_trace_helper" ]; then
    # shellcheck disable=SC1090
    . "$__bd_trace_helper" "status-line:$agent"
fi

# Count pending work items. `gc hook` prints a JSON array, so the count is
# the array length, not the line count. An empty hook is the literal `[]`,
# which is one non-empty line; counting lines reported 1 for no work. jq is
# a standard dependency of the gastown pack scripts that parse gc/bd JSON.
w=$(gc hook "$agent" 2>/dev/null | jq 'length' 2>/dev/null || true)

# Count unread mail via the count-only endpoint (cheaper than mail check).
m=$(gc mail count "$agent" --json 2>/dev/null | jq -r '.unread // 0' 2>/dev/null || echo 0)

# Format: agent | hook-icon N | mail-icon N  (omit segments that are 0)
printf '%s' "$agent"
[ "${w:-0}" -gt 0 ] && printf ' | 🪝 %d' "$w"
[ "${m:-0}" -gt 0 ] && printf ' | 📬 %d' "$m"

# Honor the always-exit-0 contract: the final `[ ]` short-circuits to a
# non-zero status whenever its segment is omitted, and tmux must never see
# an error from a #(command) helper.
exit 0

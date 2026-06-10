#!/usr/bin/env bash
# nudge-on-route — wake the target session the moment a bead is routed to it.
#
# `gc sling` does not nudge warm-idle workers (issue #1129, closed by
# design: cities that reuse warm workers were told to "introduce orders
# that trigger on new beads being created and manually nudge the workers
# in the warm set"). Without that nudge, a bead whose metadata.gc.routed_to
# is newly set or changed sits unclaimed against any worker that is not
# currently in an active turn cycle.
#
# This order is exactly that workaround, shipped. It subscribes to
# bead.updated events; whenever a bead carries a gc.routed_to target with an
# ACTIVE session it runs `gc session nudge <session> "<message>"`. Targets
# with no active session are skipped — queued nudges at dormant targets can
# never deliver and strand until TTL (gc-i4s); demand-driven wake owns them.
# Idempotent: a given (bead, routed_to) pair is nudged at most once. Dedup
# state lives in $GC_PACK_STATE_DIR/nudge-on-route-state.json, so it is both
# city- and pack-scoped — multi-city installs never cross-pollinate.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "nudge-on-route"

# jq is a hard dependency: without it the event stream cannot be decoded
# and every nudge would be silently skipped. Fail loud at startup.
if ! command -v jq >/dev/null 2>&1; then
    echo "nudge-on-route: jq is required but not found in PATH" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# Event lookback window. Must exceed the controller's event-trigger eval
# cadence so no routing event is missed between runs.
LOOKBACK="${GC_NUDGE_ON_ROUTE_LOOKBACK:-2m}"
# Dedup entries older than this are pruned so the state file stays bounded.
# Must comfortably exceed LOOKBACK so an active routing — which keeps
# re-emitting bead.updated as the reconciler refreshes it — is never
# pruned and re-nudged. Accepts a simple Ns / Nm / Nh duration.
RETENTION="${GC_NUDGE_ON_ROUTE_RETENTION:-1h}"
NUDGE_MESSAGE="${GC_NUDGE_ON_ROUTE_MESSAGE:-check for assigned work}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance}"
STATE_FILE="$PACK_STATE_DIR/nudge-on-route-state.json"
mkdir -p "$PACK_STATE_DIR"

# Convert a simple Go-style duration (Ns/Nm/Nh) to whole seconds.
duration_to_seconds() {
    case "$1" in
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

# Snapshot the active session list once per run. Each routed target is
# matched against this snapshot instead of shelling out per target, and a
# fetch failure (API down) degrades to "no active sessions" — every target
# is skipped this run and retried on the next event evaluation.
ACTIVE_SESSIONS="$(gc session list --json --state active 2>/dev/null \
    | jq -c '(.sessions // []) | map({name: (.name // .id), template: (.template // ""), alias: (.alias // "")})' 2>/dev/null)" \
    || ACTIVE_SESSIONS="[]"
[ -n "$ACTIVE_SESSIONS" ] || ACTIVE_SESSIONS="[]"

# Nudge the active session(s) named by a routed_to target. A multi-session
# pool routes to the pool BASE (NormalizePoolRouteTarget collapses slot ->
# base), which is the members' `template`, not a session name — `gc session
# nudge` resolves a single session and cannot target a pool base. So match
# the target against active templates first (pool fan-out; single-session
# agents also carry their own name as template), then against active session
# names/aliases (explicit slot or agent targets).
#
# A target with no active session is SKIPPED, never nudged: `gc session
# nudge` at a dormant target queues a session-fenced nudge that can never
# deliver and strands until its 24h TTL, polluting every ready projection
# on the way (gc-i4s). Waking dormant targets is the controller's job —
# routed demand wakes pools via scale_check counts and named sessions via
# demand checks, and a freshly spawned session reads its hook at startup.
# Returns 0 if at least one nudge succeeded, non-zero otherwise.
nudge_routed_target() {
    _target="$1"
    _members="$(printf '%s' "$ACTIVE_SESSIONS" \
        | jq -r --arg t "$_target" '.[] | select(.template == $t) | .name' 2>/dev/null)" || _members=""
    if [ -z "$_members" ]; then
        _members="$(printf '%s' "$ACTIVE_SESSIONS" \
            | jq -r --arg t "$_target" '.[] | select(.name == $t or .alias == $t) | .name' 2>/dev/null)" || _members=""
    fi
    if [ -z "$_members" ]; then
        # No active session for this target: skip without recording dedup
        # state, so the pair is re-evaluated (and nudged) if the routing is
        # still emitting events once the target has an active session.
        return 1
    fi
    _any=1
    while IFS= read -r _m; do
        [ -n "$_m" ] || continue
        if gc session nudge "$_m" "$NUDGE_MESSAGE" >/dev/null 2>&1; then
            _any=0
        fi
    done <<MEMBERS
$_members
MEMBERS
    return "$_any"
}

# Pull recent bead.updated events. Best-effort: a read failure (API down)
# must not crash the controller's order loop.
EVENTS="$(gc events --type bead.updated --since "$LOOKBACK" 2>/dev/null)" || exit 0
[ -n "$EVENTS" ] || exit 0

# Reduce to unique "<bead_id>\t<routed_to>" pairs. Only events that actually
# carry a non-empty gc.routed_to target are considered.
PAIRS="$(printf '%s\n' "$EVENTS" \
    | jq -r 'select(.payload.bead.metadata."gc.routed_to" != null
                    and .payload.bead.metadata."gc.routed_to" != "")
             | [.payload.bead.id, .payload.bead.metadata."gc.routed_to"] | @tsv' 2>/dev/null \
    | sort -u)" || PAIRS=""
[ -n "$PAIRS" ] || exit 0

# Load dedup state (object mapping "<bead>|<routed_to>" -> ISO timestamp).
# A missing or corrupt file resets to an empty object rather than failing.
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'

NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NUDGED=0
SKIPPED=0
while IFS="$(printf '\t')" read -r bead_id routed_to; do
    if [ -z "$bead_id" ] || [ -z "$routed_to" ]; then continue; fi
    key="$bead_id|$routed_to"
    if echo "$STATE" | jq -e --arg k "$key" 'has($k)' >/dev/null 2>&1; then
        # Already nudged. Refresh last-seen so this still-active routing is
        # not pruned and re-nudged while it keeps re-emitting.
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        continue
    fi
    if nudge_routed_target "$routed_to"; then
        STATE="$(echo "$STATE" | jq --arg k "$key" --arg now "$NOW" '.[$k] = $now')"
        NUDGED=$((NUDGED + 1))
    else
        SKIPPED=$((SKIPPED + 1))
    fi
done <<EOF
$PAIRS
EOF

# Prune entries older than RETENTION so the state file stays bounded.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
STATE="$(echo "$STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value | fromdateiso8601)) <= $keep))')" || true

# Atomic write: temp file in the same dir, then rename.
TMP="$(mktemp "$PACK_STATE_DIR/.nudge-on-route-state.XXXXXX")"
printf '%s\n' "$STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

if [ "$NUDGED" -gt 0 ]; then
    echo "nudge-on-route: nudged $NUDGED newly-routed bead(s)"
fi
if [ "$SKIPPED" -gt 0 ]; then
    echo "nudge-on-route: skipped $SKIPPED routed bead(s) — target has no active session (or nudge failed); demand wake owns dormant targets"
fi

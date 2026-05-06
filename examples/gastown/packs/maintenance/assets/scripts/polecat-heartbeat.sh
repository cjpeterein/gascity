#!/usr/bin/env bash
# polecat-heartbeat — detect wedged polecat sessions via LastActive.
#
# Scans active polecat-template sessions, compares LastActive (session I/O
# activity from the runtime provider) to a threshold. Sessions with
# in-progress work and no I/O past the threshold get a warrant filed
# against them; the dog pool picks up the warrant and runs
# mol-shutdown-dance (three ALIVE probes before kill).
#
# Deterministic — no LLM judgment. Runs as a controller exec order.
#
# Environment:
#   POLECAT_HEARTBEAT_THRESHOLD_SECS   (default: 600 = 10m)
#   POLECAT_HEARTBEAT_TEMPLATE_PATTERN (default: polecat)  regex on Template
#   POLECAT_HEARTBEAT_DOG_ROUTE        (default: dog)      gc.routed_to value
#   POLECAT_HEARTBEAT_REQUESTER        (default: polecat-heartbeat)
set -euo pipefail

THRESHOLD_SECS="${POLECAT_HEARTBEAT_THRESHOLD_SECS:-600}"
TEMPLATE_PATTERN="${POLECAT_HEARTBEAT_TEMPLATE_PATTERN:-polecat}"
DOG_ROUTE="${POLECAT_HEARTBEAT_DOG_ROUTE:-dog}"
REQUESTER="${POLECAT_HEARTBEAT_REQUESTER:-polecat-heartbeat}"

SESSIONS=$(gc session list --state=all --json 2>/dev/null) || exit 0
if [ -z "$SESSIONS" ] || [ "$SESSIONS" = "null" ]; then
    exit 0
fi

WARRANTS=$(gc bd list --type=warrant --status=open --json --limit=0 2>/dev/null || printf '[]')
if [ -z "$WARRANTS" ] || [ "$WARRANTS" = "null" ]; then
    WARRANTS='[]'
fi

NOW_EPOCH=$(date -u +%s)

# Select active polecat-template sessions with real LastActive older than
# the threshold. jq handles both "Z" suffix and "+hh:mm"/"-hh:mm" tz offsets
# (mac/linux date portability is a nightmare; jq's string math isn't).
WEDGED=$(printf '%s' "$SESSIONS" | jq -c \
    --arg pat "$TEMPLATE_PATTERN" \
    --argjson threshold "$THRESHOLD_SECS" \
    --argjson now "$NOW_EPOCH" '
    def parse_ts:
      if (type == "string") then
        if test("Z$") then fromdateiso8601
        else sub("(?<sign>[+-])(?<h>\\d{2}):(?<m>\\d{2})$"; .sign+.h+.m)
             | strptime("%Y-%m-%dT%H:%M:%S%z") | mktime
        end
      else . end;
    [ .[]
      | select(
          .State == "active"
          and ((.Template // "") | test($pat))
          and ((.LastActive // "") != "")
          and (((.LastActive // "") | startswith("0001-")) | not)
        )
      | . as $s
      | ($s.LastActive | parse_ts) as $t
      | select(($now - $t) > $threshold)
      | {
          session_name: $s.SessionName,
          alias:        ($s.Alias // ""),
          template:     ($s.Template // ""),
          last_active:  $s.LastActive,
          wedge_secs:   ($now - $t),
        }
    ]' 2>/dev/null) || WEDGED='[]'

if [ -z "$WEDGED" ] || [ "$WEDGED" = "[]" ] || [ "$WEDGED" = "null" ]; then
    exit 0
fi

# Collect existing warrant targets (by metadata.target) so we don't file
# duplicates while a prior warrant is still being danced.
EXISTING_TARGETS=$(printf '%s' "$WARRANTS" | jq -r '
    .[] | (.metadata.target // empty)
' 2>/dev/null | awk 'NF' | sort -u)

is_existing_target() {
    local t="$1"
    [ -z "$t" ] && return 1
    if [ -n "$EXISTING_TARGETS" ]; then
        printf '%s\n' "$EXISTING_TARGETS" | grep -Fxq "$t"
    else
        return 1
    fi
}

FILED=0
while IFS=$'\t' read -r SESSION_NAME ALIAS TEMPLATE LAST_ACTIVE WEDGE_SECS; do
    [ -z "$SESSION_NAME" ] && continue
    # Prefer alias (rig-qualified name) for target so shutdown-dance nudges
    # the canonical identity; fall back to SessionName.
    TARGET="$ALIAS"
    if [ -z "$TARGET" ] || [ "$TARGET" = "null" ]; then
        TARGET="$SESSION_NAME"
    fi
    # Skip if a warrant is already open for this target (under either name).
    if is_existing_target "$TARGET" || is_existing_target "$SESSION_NAME"; then
        continue
    fi
    REASON="heartbeat-timeout: ${WEDGE_SECS}s without I/O (threshold ${THRESHOLD_SECS}s, last_active ${LAST_ACTIVE})"
    META=$(jq -nc \
        --arg target "$TARGET" \
        --arg session_name "$SESSION_NAME" \
        --arg reason "$REASON" \
        --arg requester "$REQUESTER" \
        --arg routed_to "$DOG_ROUTE" \
        '{target:$target, session_name:$session_name, reason:$reason, requester:$requester, "gc.routed_to":$routed_to}')
    if gc bd create \
        --type=warrant \
        --title="Stuck polecat: $TARGET" \
        --description="Session $TARGET (template $TEMPLATE) has produced no I/O for ${WEDGE_SECS}s. Filed by polecat-heartbeat. Dog pool: pour mol-shutdown-dance against this target." \
        --metadata "$META" \
        --labels pool:dog \
        >/dev/null 2>&1; then
        FILED=$((FILED + 1))
    fi
done < <(printf '%s' "$WEDGED" | jq -r '.[] | [
    .session_name,
    .alias,
    .template,
    .last_active,
    (.wedge_secs | floor | tostring)
] | @tsv')

if [ "$FILED" -gt 0 ]; then
    echo "polecat-heartbeat: filed $FILED warrants for wedged polecats"
fi

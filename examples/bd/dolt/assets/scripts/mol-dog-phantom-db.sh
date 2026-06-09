#!/usr/bin/env bash
# mol-dog-phantom-db — detect and escalate phantom Dolt database directories.
#
# Read-only scan of the data dir: surfaces any <db>/.dolt/ without a
# noms/manifest, any <db>/.dolt/noms/manifest below the byte-sanity
# threshold (a real manifest is hundreds of bytes; test-fixture leaks have
# been as small as 3), plus any *.replaced-YYYYMMDDTHHMMSSZ leftover, via
# generic escalation mail. Operator decides remediation.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
. "$PACK_DIR/assets/scripts/_notify.sh"

DATA_DIR="${GC_PHANTOM_DATA_DIR:-$DOLT_DATA_DIR}"

# Manifests smaller than this many bytes are treated as unservable fixture
# leaks rather than real databases (gc-wfl4e). 0 disables the size guard
# while leaving the missing-manifest phantom scan intact.
MANIFEST_MIN_BYTES="${GC_PHANTOM_MANIFEST_MIN_BYTES:-32}"

# --- Step 1: Scan for phantom database directories ---

if [ ! -d "$DATA_DIR" ]; then
    echo "phantom-db: data dir $DATA_DIR not found, skipping"
    exit 0
fi

SCANNED=0
UNSERVABLES=""
PHANTOMS=""
RETIRED_REPLACEMENTS=""
UNDERSIZED=""
PHANTOM_COUNT=0
RETIRED_COUNT=0
UNDERSIZED_COUNT=0
UNSERVABLE_COUNT=0
VALID=0

for dir in "$DATA_DIR"/*/; do
    [ -d "$dir" ] || continue
    db_name=$(basename "$dir")
    if [ "$db_name" = ".quarantine" ]; then
        continue
    fi
    SCANNED=$((SCANNED + 1))
    is_unservable=0
    if [ -d "$dir/.dolt" ]; then
        manifest="$dir/.dolt/noms/manifest"
        if [ ! -f "$manifest" ]; then
            PHANTOM_COUNT=$((PHANTOM_COUNT + 1))
            PHANTOMS="$PHANTOMS $db_name"
            is_unservable=1
        elif [ "$MANIFEST_MIN_BYTES" -gt 0 ]; then
            manifest_bytes=$(wc -c < "$manifest" 2>/dev/null | tr -d ' ')
            : "${manifest_bytes:=0}"
            if [ "$manifest_bytes" -lt "$MANIFEST_MIN_BYTES" ]; then
                UNDERSIZED_COUNT=$((UNDERSIZED_COUNT + 1))
                UNDERSIZED="$UNDERSIZED $db_name($manifest_bytes bytes)"
                is_unservable=1
            fi
        fi
        case "$db_name" in
            *.replaced-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z)
                RETIRED_COUNT=$((RETIRED_COUNT + 1))
                RETIRED_REPLACEMENTS="$RETIRED_REPLACEMENTS $db_name"
                is_unservable=1
                ;;
        esac
    fi
    if [ "$is_unservable" -eq 1 ]; then
        UNSERVABLES="$UNSERVABLES $db_name"
        UNSERVABLE_COUNT=$((UNSERVABLE_COUNT + 1))
    else
        VALID=$((VALID + 1))
    fi
done

if [ "$UNSERVABLE_COUNT" -eq 0 ]; then
    SUMMARY="phantom-db — scanned: $SCANNED, phantoms: 0, retired: 0, undersized: 0, valid: $VALID"
    dolt_notify_done "$SUMMARY"
    echo "phantom-db: $SUMMARY"
    exit 0
fi

# --- Step 2: Escalate to operator ---

dolt_escalate \
    "ESCALATION: Unservable Dolt databases detected [HIGH]" \
    "Dolt: detected $UNSERVABLE_COUNT unservable database directories in $DATA_DIR:$UNSERVABLES.

Phantoms missing noms/manifest: $PHANTOM_COUNT${PHANTOMS:- (none)}
Undersized manifests (below sanity threshold): $UNDERSIZED_COUNT${UNDERSIZED:- (none)}
Retired replacement directories: $RETIRED_COUNT${RETIRED_REPLACEMENTS:- (none)}

This order is read-only and did not move or delete any database directory.
Operator remediation is required. Stop the Dolt server before removing
phantom directories manually. For retired replacement directories that Dolt
still lists after restart, verify they are no longer needed and use:

  dolt sql -q 'DROP DATABASE \`<db_name>\`;'

Investigate root cause (incomplete dolt init, interrupted replacement,
manual filesystem edit) before re-creating affected databases." \
    2>/dev/null || true

# --- Step 3: Report ---

SUMMARY="phantom-db — scanned: $SCANNED, phantoms: $PHANTOM_COUNT, retired: $RETIRED_COUNT, undersized: $UNDERSIZED_COUNT, escalated: $UNSERVABLE_COUNT"
dolt_notify_done "$SUMMARY"
echo "phantom-db: $SUMMARY"

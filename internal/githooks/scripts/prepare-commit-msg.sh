#!/usr/bin/env sh
# gc-managed prepare-commit-msg hook (Layer 2: footer verification).
#
# Verifies that agent-authored commits include a Claude co-authorship
# footer. No-op for human commits and merge/squash/amend commit sources.
#
# Behavior is controlled by GC_HOOK_FOOTER_MODE:
#   warn   (default): emit a warning to stderr; never block.
#   strict: block the commit if the footer is missing.
#   off:    skip the check entirely.
#
# This hook runs verification only. The footer itself is written by the
# 'commit' skill (or, when it lands, the 'gc commit' wrapper). See bead
# gc-6o0m for the full three-layer design.

msg_file="$1"
source="$2"

case "$source" in
    merge|squash|commit) exit 0 ;;
esac

case "${CLAUDECODE:-}" in
    1|true) ;;
    *) exit 0 ;;
esac

case "${GC_HOOK_FOOTER_MODE:-warn}" in
    off) exit 0 ;;
esac

if grep -qE 'Co-Authored-By:.*Claude Code' "$msg_file" 2>/dev/null; then
    exit 0
fi

case "${GC_HOOK_FOOTER_MODE:-warn}" in
    strict)
        echo >&2 "gc: commit blocked - agent-authored commit missing Claude co-authorship footer."
        echo >&2 "gc: invoke the 'commit' skill, or generate the footer with:"
        echo >&2 "gc:   bash ~/.claude/skills/commit/get-co-author.sh \"<model>\""
        echo >&2 "gc: set GC_HOOK_FOOTER_MODE=warn or =off to bypass once."
        exit 1
        ;;
    *)
        echo >&2 "gc: warning - agent-authored commit missing Claude co-authorship footer."
        echo >&2 "gc: invoke the 'commit' skill on the next commit so the footer is added."
        exit 0
        ;;
esac

#!/bin/sh
# tmux-theme.sh — Gas Town status bar theme with colors and icons.
# Usage: tmux-theme.sh <session> <agent> <config-dir>
#
# Theme tier is driven by SDK session-name primitives — no role awareness:
#
#   "<rig>--*"     -> rig tier   (witness, refinery, polecat, crew within a rig)
#   "<scope>__*"   -> scope tier (mayor, deacon — city-scoped roles)
#   "<base>-N"     -> pool tier  (dog-1, dog-2, generic pool members)
#   anything else  -> default tier
#
# The "--" and "__" separators correspond to the SDK session-name mapping
# (slash → "--", dot → "__") in internal/agent/session_name.go. Keying on
# the separator rather than role names lets this script work for any pack,
# including custom packs whose role taxonomy differs from gastown's. Same
# pattern as cycle.sh (#1571) and bind-key.sh (#1573).
# $3 (config-dir) is still accepted for caller compatibility but unused:
# status-right no longer references pack scripts (gc-46q).
SESSION="$1" AGENT="$2"

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# Determine theme tier by session-name shape.
case "$SESSION" in
    *--*)       tier="rig" ;;
    *__*)       tier="scope" ;;
    *-[0-9]*)   tier="pool" ;;
    *)          tier="default" ;;
esac

# Tier color theme (bg/fg).
case "$tier" in
    rig)     bg="#1e3a5f" fg="#e0e0e0" ;;  # ocean
    scope)   bg="#2d1f3d" fg="#c0b0d0" ;;  # purple/silver
    pool)    bg="#3d2f1f" fg="#d0c0a0" ;;  # brown/tan
    *)       bg="#4a5568" fg="#e0e0e0" ;;  # slate
esac

# Tier icon.
case "$tier" in
    rig)     icon="⛏" ;;
    scope)   icon="🏛" ;;
    pool)    icon="🌊" ;;
    *)       icon="●" ;;
esac

# Apply theme.
gcmux set-option -t "$SESSION" status-position bottom
gcmux set-option -t "$SESSION" status-style "bg=$bg,fg=$fg"
gcmux set-option -t "$SESSION" status-left-length 25
gcmux set-option -t "$SESSION" status-left "$icon $AGENT "
gcmux set-option -t "$SESSION" status-right-length 80
gcmux set-option -t "$SESSION" status-interval 5

# Render path is a pure in-server option read — no #() shellouts. tmux
# re-renders the status bar on pane activity at an unbounded rate, so any
# command here would run arbitrarily often (gc-46q). The status-updater.sh
# session_live sidecar refreshes @gc_status_line at a bounded cadence.
gcmux set-option -t "$SESSION" status-right "#{@gc_status_line} %H:%M"

# Seed the identity segment so the bar is never blank before the first
# updater tick. Only when unset: re-theming (the reconciler re-applies
# session_live on drift) must not clobber updater-produced content.
cur=$(gcmux show-options -t "$SESSION" -qv @gc_status_line 2>/dev/null)
[ -z "$cur" ] && gcmux set-option -t "$SESSION" @gc_status_line "$AGENT"

# Mouse + clipboard.
gcmux set-option -t "$SESSION" mouse on
gcmux set-option -t "$SESSION" set-clipboard on

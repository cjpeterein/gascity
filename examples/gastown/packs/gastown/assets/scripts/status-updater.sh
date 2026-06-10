#!/bin/sh
# status-updater.sh — sidecar that produces the status-line text OFF the
# tmux render path (gc-46q).
#
# Usage: status-updater.sh <session> <agent> <config-dir> [interval-seconds]
#
# tmux re-renders the status bar on pane activity at an unbounded rate, so
# anything in status-right's #() runs arbitrarily often. This sidecar moves
# the data production out: it refreshes the @gc_status_line session option
# at a bounded cadence and tmux-theme.sh points status-right at the option,
# so a render is a pure in-server variable read — zero process spawns.
#
# Launched from the pack's session_live hooks, which run host-side under a
# setup timeout and are re-applied by the reconciler on drift. The launcher
# therefore daemonizes a --daemon copy of itself and returns immediately,
# and re-invocations detect the live daemon via the @gc_status_updater_pid
# session option (live tmux state, not a PID file) and leave it alone.
#
# The daemon only produces data while a client is attached (plus one seed
# tick at startup): a detached idle town contributes no recurring bd/dolt
# load. It exits when its session or tmux server goes away.

if [ "$1" = "--daemon" ]; then
    daemon=1
    shift
else
    daemon=0
fi

SESSION="$1" AGENT="$2" CONFIGDIR="$3" INTERVAL="${4:-10}"
if [ -z "$SESSION" ] || [ -z "$AGENT" ] || [ -z "$CONFIGDIR" ]; then
    exit 0
fi

# Socket-aware tmux command (uses GC_TMUX_SOCKET when set).
gcmux() { tmux ${GC_TMUX_SOCKET:+-L "$GC_TMUX_SOCKET"} "$@"; }

# updater_pid prints the daemon PID recorded on the session, if any.
updater_pid() {
    gcmux show-options -t "$SESSION" -qv @gc_status_updater_pid 2>/dev/null
}

if [ "$daemon" -eq 0 ]; then
    # Launcher: skip when a live daemon already owns this session. The PID
    # is validated against the process table (a recycled PID whose command
    # line is not this script does not count) so a crashed daemon never
    # blocks a replacement.
    pid=$(updater_pid)
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        case "$(ps -o args= -p "$pid" 2>/dev/null)" in
            *status-updater*) exit 0 ;;
        esac
    fi
    # Subshell-background with all stdio detached: the daemon must outlive
    # the session_live setup timeout and hold no pipes back to the caller.
    ("$0" --daemon "$SESSION" "$AGENT" "$CONFIGDIR" "$INTERVAL" \
        >/dev/null 2>&1 </dev/null &)
    exit 0
fi

# Daemon: claim the session slot, then confirm the claim survived so two
# near-simultaneous launches converge to a single daemon (last writer wins).
gcmux set-option -t "$SESSION" @gc_status_updater_pid "$$" 2>/dev/null || exit 0
sleep 1
[ "$(updater_pid)" = "$$" ] || exit 0

first=1
while :; do
    # One probe per tick doubles as the liveness check: when the session or
    # the tmux server is gone, the daemon's job is over.
    attached=$(gcmux display-message -p -t "$SESSION" '#{session_attached}' 2>/dev/null) || exit 0
    if [ "$first" -eq 1 ] || [ "${attached:-0}" -gt 0 ] 2>/dev/null; then
        text=$("$CONFIGDIR/assets/scripts/status-line.sh" "$AGENT" 2>/dev/null)
        if [ -n "$text" ]; then
            cur=$(gcmux show-options -t "$SESSION" -qv @gc_status_line 2>/dev/null)
            if [ "$text" != "$cur" ]; then
                gcmux set-option -t "$SESSION" @gc_status_line "$text" 2>/dev/null || exit 0
            fi
        fi
    fi
    first=0
    sleep "$INTERVAL"
done

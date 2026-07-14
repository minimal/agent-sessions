#!/bin/sh
# agent-sessions copilot-live marker writer.
#
# Usage: marker.sh <idle|running|waiting|notify|delete> <pid> <pane>
#
# Invoked by the Copilot CLI hooks in hooks.json. It reads the hook's JSON
# payload on stdin, then writes (or deletes) a marker file that agent-sessions
# reads to show live Copilot session state:
#
#   ~/.copilot/live/<session-uuid>.json
#
# <pid> and <pane> are passed in from the hook command, where $PPID resolves to
# the copilot process and $TMUX_PANE to its tmux pane (if any); resolving them
# here would instead see this script's own parent shell.

status="$1"
pid="${2:-0}"
pane="$3"
in=$(cat)

# Extract the session uuid from the flat hook JSON. No jq dependency: a Copilot
# session id is a uuid and a cwd is a filesystem path, neither of which contains
# a double quote, so a simple sed capture is safe.
sid=$(printf '%s' "$in" | sed -n 's/.*"sessionId"[[:space:]]*:[[:space:]]*"\([0-9A-Fa-f-]\{8,\}\)".*/\1/p' | head -n1)
[ -n "$sid" ] || exit 0

dir="${COPILOT_HOME:-$HOME/.copilot}/live"
file="$dir/$sid.json"

if [ "$status" = "delete" ]; then
	rm -f "$file"
	exit 0
fi

# The notification hook fires for several reasons; only a permission or
# elicitation prompt means the agent is blocked waiting on the user.
if [ "$status" = "notify" ]; then
	case "$in" in
	*permission_prompt* | *elicitation_dialog*) status="waiting" ;;
	*) exit 0 ;;
	esac
fi

cwd=$(printf '%s' "$in" | sed -n 's/.*"cwd"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Write atomically (temp file + rename) so agent-sessions never reads a
# half-written marker.
mkdir -p "$dir"
tmp="$file.$$.tmp"
printf '{"sid":"%s","pid":%s,"pane":"%s","cwd":"%s","status":"%s","updated_at":"%s"}\n' \
	"$sid" "$pid" "$pane" "$cwd" "$status" "$now" >"$tmp" && mv "$tmp" "$file"

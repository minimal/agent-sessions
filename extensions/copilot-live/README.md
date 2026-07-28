# copilot-live

A small [GitHub Copilot CLI](https://docs.github.com/copilot/how-tos/copilot-cli)
hook that exposes live session state to `agent-sessions`.

## What it does

The Copilot CLI runs configured hooks on lifecycle events. This hook writes (or
deletes) a JSON marker file on each event:

```
~/.copilot/live/<session-uuid>.json
```

Example marker:

```json
{
  "sid": "935ca4e7-60b5-4632-a109-4d242467eab1",
  "pid": 12345,
  "pane": "%5",
  "cwd": "/home/chris/code/agent-sessions",
  "status": "running",
  "updated_at": "2026-07-14T12:34:56Z"
}
```

`agent-sessions` reads these markers to:

- show Copilot sessions as **live** (with a running/waiting/idle state),
- display the **tmux pane** glyph and jump to the pane on `Enter`,
- sort live Copilot sessions to the top of the index.

Without this hook, Copilot sessions are still browsable and `Enter` can jump to
a pane found by cwd match, but they are shown as offline and have no live state.

## Installation

Copilot loads every JSON file in `~/.copilot/hooks/`. Copy (or symlink) both
files there — the hook config as its own file so it doesn't clobber other tools'
hooks (cmux, supacode, ...), and the marker script alongside it:

```bash
cp extensions/copilot-live/hooks.json  "$HOME/.copilot/hooks/agent-sessions.json"
cp extensions/copilot-live/marker.sh   "$HOME/.copilot/hooks/agent-sessions-marker.sh"
```

Then start a new `copilot` session (hooks are read at launch). If you set
`$COPILOT_HOME`, both the hooks dir and the marker dir move under it
automatically.

## Marker lifecycle

| Copilot hook event                          | marker action | status    |
|---------------------------------------------|---------------|-----------|
| `sessionStart`                              | write         | `idle`    |
| `userPromptSubmitted`                       | write         | `running` |
| `preToolUse` / `postToolUse`                | write         | `running` |
| `notification` (permission / elicitation)   | write         | `waiting` |
| `agentStop`                                 | write         | `idle`    |
| `sessionEnd`                                | delete        | —         |

The `notification` hook fires for several reasons; the marker script only sets
`waiting` when the payload is a `permission_prompt` or `elicitation_dialog`
(the agent is blocked on the user), and ignores the rest.

`$PPID` (the `copilot` process) and `$TMUX_PANE` are captured in the hook
command — where they refer to Copilot itself — and passed to the script, since
resolving them inside the script would instead see its own parent shell. Markers
are written atomically (temp file + rename) so `agent-sessions` never reads a
half-written file.

## Status values

- `running` — Copilot is generating a response or running a tool.
- `waiting` — the agent is blocked on the user (a permission/elicitation prompt).
- `idle` — Copilot is waiting for the next user prompt.

`agent-sessions` maps these to its own `running` / `waiting` / `idle` states.
Stale markers (a dead `pid`, or an orphan with no matching session older than an
hour) are cleaned up by `agent-sessions` on refresh.

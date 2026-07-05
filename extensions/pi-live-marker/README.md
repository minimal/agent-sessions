# agent-sessions-pi-live

A small [pi](https://github.com/earendil-works/pi-mono) extension that exposes
live session state to `agent-sessions`.

## What it does

Whenever a pi session starts, an agent turn starts/ends, or a session shuts
down, this extension writes (or deletes) a JSON marker file:

```
~/.pi/agent/live/<session-uuid>.json
```

Example marker:

```json
{
  "sid": "019f4b0c-...",
  "pid": 12345,
  "pane": "%5",
  "cwd": "/home/chris/code/agent-sessions",
  "status": "running",
  "updated_at": "2026-07-05T12:34:56.789Z"
}
```

`agent-sessions` reads these markers to:

- show pi sessions as **live** (with a running/waiting/idle state),
- display the **tmux pane** glyph and jump to the pane on `Enter`,
- sort live pi sessions to the top of the index.

Without this extension, pi sessions are still browsable and `Enter` can jump to
a pane found by cwd match, but they are shown as offline and have no live
state.

## Installation

Copy or symlink this directory into pi's global extensions directory:

```bash
ln -s "$(pwd)/extensions/pi-live-marker" "$HOME/.pi/agent/extensions/agent-sessions-pi-live"
```

Then restart pi, or run `/reload` inside pi. No build step is required — pi
loads TypeScript extensions directly via jiti.

## Marker lifecycle

| pi event          | marker action | status   |
|-------------------|---------------|----------|
| `session_start`   | write         | `idle`   |
| `agent_start`     | write         | `running`|
| `agent_end`       | write         | `idle`   |
| `session_shutdown`| delete        | —        |

Markers are written atomically (temp file + rename) so `agent-sessions` never
reads a half-written file.

## Status values

- `running` — pi is generating a response for this session.
- `waiting` — reserved for future permission-prompt states.
- `idle` — pi is waiting for the next prompt.

`agent-sessions` maps these to its own `running` / `waiting` / `idle` states.

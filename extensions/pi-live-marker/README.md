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
| `message_end` (assistant) | write | `waiting` |
| `tool_execution_start` | write    | `running` (+ start poll) |
| `tool_execution_end`   | (poll stops; stale waiting flag cleared) | |
| `agent_end`       | write         | `idle`   |
| `session_shutdown`| delete        | —        |

While a tool is executing, the extension polls every 1.5s and writes the
status based on:

1. A sidecar flag file `<liveDir>/<sid>.waiting` (see Cooperation protocol
   below) — if present, `waiting`.
2. Otherwise `ctx.isIdle()` — `true` → `waiting`, `false` → `running`.

Markers are written atomically (temp file + rename) so `agent-sessions` never
reads a half-written file.

## Status values

- `running` — pi is generating a response, or a tool is actively working.
- `waiting` — the agent is blocked on the user (a permission prompt or a
  cooperating extension's UI prompt).
- `idle` — pi is waiting for the next user prompt.

`agent-sessions` maps these to its own `running` / `waiting` / `idle` states.

## Cooperation protocol (for prompting extensions)

The pi extension API has no generic "the user is being prompted" event.
`ctx.isIdle()` stays `false` while a custom tool awaits `ctx.ui.confirm` /
`input` / `select` / `custom` (verified — see [earendil-works/pi#5329]),
so a polling isIdle() heuristic cannot distinguish "tool working" from
"tool blocked on a UI promise" on its own.

To fix that for a specific extension, the extension can create a sidecar
**waiting flag** before showing a prompt and delete it after. The
agent-sessions-pi-live poll will force `waiting` whenever the flag is
present, regardless of isIdle().

In your extension, wrap any `ctx.ui.*` prompt with:

```typescript
import { writeFileSync, unlinkSync } from "node:fs";
import { join } from "node:path";

const sid = ctx.sessionManager.getSessionId();
const sessionFile = ctx.sessionManager.getSessionFile();
if (sid && sessionFile) {
	const liveDir = join(sessionFile, "..", "..", "..", "live");
	const flag = join(liveDir, `${sid}.waiting`);

	// Before showing the prompt:
	writeFileSync(flag, "");

	// Show the prompt (await the user):
	const answer = await ctx.ui.confirm("Title", "Allow this?");

	// After the user responds (always, even on rejection):
	try { unlinkSync(flag); } catch {}
}
```

The path is the marker directory's sibling: `~/.pi/agent/live/`. The flag
filename is the session uuid with a `.waiting` suffix. `agent-sessions-pi-live`
also defensively removes the flag when the last `tool_execution_end` fires,
in case your extension crashes mid-prompt.

This is what we use to make the bundled
[`examples/extensions/question.ts`](https://github.com/earendil-works/pi-mono/tree/main/examples/extensions)
and a cloned permissions extension show `waiting` while the prompt is up.

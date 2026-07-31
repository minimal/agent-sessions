# Multi-Adapter Plan (pi + beyond)

Today the app reads Claude Code logs only, and the Claude-specific knowledge is
spread across the core: `claudeDir`, `transcriptLine`, `parseTranscript`,
`absorb`, `assistantText/contentText/userPrompt`, `liveStates`,
`registrySession`, `registryStates`, and `markLive` all live in `session.go`
alongside the generic `Session` model. This doc plans adding a **pi** adapter
and making the core source-agnostic so other adapters (Copilot CLI, etc.) drop
in without touching core logic.

## What's Claude-specific today

| Concern | Claude-specific code | Generic core that stays |
|---|---|---|
| Transcript location | `claudeDir("projects")` → `~/.claude/projects/*/*.jsonl` | — |
| Transcript schema | `transcriptLine` struct (`aiTitle`,`gitBranch`,`slug`,`isMeta`,`message`) | — |
| Parsing | `parseTranscript`, `absorb`, `assistantText`, `contentText`, `userPrompt`, `scan` | — |
| Live process registry | `liveStates`, `registrySession`, `registryStates`, `markLive` (reads `~/.claude/sessions/<pid>.json`) | — |
| Session model | — | `Session`, `SessionState` (+ `running/waiting/idle/unknown`) |
| Caching | — | `loader.cache` (mtime+size keyed) |
| UI / markers / sort | — | `model`, `marker`, `styles`, sort-by-activity |

The `Session` struct is already nearly generic. The only real Claude-isms on it
are `Slug` (harmless; pi leaves it `""`) and the *semantics* of `Title` (Claude's
`aiTitle`; pi will fill it from the first user prompt or a named session).

## How pi stores sessions (verified on this machine)

- **Location:** `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl`,
  where `<encoded-cwd>` is the cwd with `/` → `-` (a shard key only; **do not
  decode it** — read the real cwd from the `session` line). Overridable via
  `PI_CODING_AGENT_SESSION_DIR` / `--session-dir`; the adapter should honour the env
  var and fall back to `~/.pi/agent/sessions`.
- **Filename:** `<RFC3339 start time>_<session-uuid>.jsonl`. The UUID is the
  session id (matches `session.id` inside the file).
- **Entry types** (across all transcripts): `session`, `message`,
  `model_change`, `thinking_level_change`, `custom`, `custom_message`,
  `compaction`.
  - `session` line: `{type, version, id, timestamp, cwd, name?}` — header, has
    the cwd and optionally a `name` (when started with `pi --name`).
  - `message` line: `{type:"message", id, parentId, timestamp,
    message:{role:"user"|"assistant", content:[...blocks], ...}}`. Blocks are
    `{type:"text"|"thinking"|"toolCall", ...}`.
- **No `aiTitle`, no `gitBranch`, no `slug`.** Subject must be derived from the
  first user `text` block (or `session.name`); branch must be read from
  `<cwd>/.git/HEAD`.
- **Timestamps** are RFC3339 `.Z` — identical to Claude, so `time.RFC3339`
  parsing is shared.
- **No live-process registry.** Unlike Claude's `~/.claude/sessions/<pid>.json`,
  pi writes nothing per-process. The pi process itself is a normal Linux process
  here (a WSL pnpm install running on nix node), visible to host `gopsutil`/tmux
  like any other — so a future pid-walk or cwd->pane match can find its pane. The
  gap is only the missing registry: there's no session-id -> PID/status file to
  read, so without a marker-writing extension (see phase 3) we can't attach live
  state. (A Windows pi install also exists on this machine, but the active one is
  the WSL pnpm install. Note: a sandboxed dev shell — bubblewrap — can't see the
  host's processes, which can mislead during investigation; the shipped TUI binary
  is not sandboxed and sees them fine.)
- **Resume CLI:** `pi --session <path|partial-uuid>`, `pi --session-id <id>`,
  `pi --continue` (previous in cwd), `pi --resume` (interactive picker).

## The Adapter interface (the modularity contract)

Core stops knowing about any source's disk layout. It depends only on the
shared `Session` value and this interface:

```go
// Adapter is one source of agent sessions (Claude Code, pi, Copilot CLI, ...).
// Add a source by implementing this and registering it in main.go — core stays
// untouched.
type Adapter interface {
    // Name is a short id used for per-source config and an optional UI tag,
    // e.g. "claude", "pi".
    Name() string

    // Sessions discovers and parses this source's transcripts, freshest-first
    // by activity, with NO live state attached. Implementations should cache
    // parsed metadata between calls (mtime+size keyed) so periodic refresh is
    // cheap — use transcriptCache (below) to get that for free.
    Sessions() ([]Session, error)

    // Live attaches running-process state (State, PID, Pane) in place,
    // best-effort. Sources with no usable live signal (e.g. pi without a
    // marker-writing extension, which has no per-process registry) implement
    // a no-op; sessions then surface as "offline" but still sort to the
    // top by activity/mtime.
    Live(sessions []Session)

    // TrashPaths returns all paths owned by a session. Core moves them to
    // Trash together; the adapter validates its source-specific disk layout.
    TrashPaths(Session) ([]string, error)
}
```

`Sessions` and `Live` are split because parsing is cacheable and
process/registry state is cheap and changes every refresh — and because some
sources simply have no live support and want a no-op `Live`, not a fake one.

### Shared helpers core provides (adapters opt in)

- **`transcriptCache`** — generalises today's `loader`: an adapter supplies
  `discover() ([]string, error)` (list `.jsonl` paths) and
  `parse(path string, info fs.FileInfo) (Session, error)`; the cache handles
  mtime+size-keyed reuse and dropping deleted files. No per-adapter cache code.
- **`tmuxPaneForCWD(cwd) (string, bool)`** — match a tmux pane by
  `pane_current_path` (not by pid-walk). Works across the WSL boundary because
  the pane's cwd is the Linux launch dir; this is the generic pane strategy any
  adapter can use. The Claude pid-walk stays as a Claude-specific precision path.
- **`gitBranch(cwd) string`** — read `<cwd>/.git/HEAD`. Used by pi and any
  source that doesn't record branch inline.
- **`procStartTime`/`parentPID`** (already in `proc.go`) — stay as utilities;
  the Claude adapter uses them for its pid-walk.

### A small addition to `Session`

```go
type Session struct {
    // ...existing fields...
    Source string // which adapter produced this session ("claude", "pi", ...)
}
```

`Source` drives per-source Enter commands and an optional UI tag, and guards
against id collisions when multiple sources are enabled.

## Refactor: extract Claude behind the interface

1. New `adapter.go` defining `Adapter`, the `transcriptCache` helper,
   `tmuxPaneForCWD`, `gitBranch`, and a registry (`adapters []Adapter` built in
   `main.go` from enabled sources).
2. New `claude.go`: move `claudeDir`, `transcriptLine`, `parseTranscript`,
   `absorb`, `assistantText`, `contentText`, `userPrompt`, `scan`,
   `liveStates`, `registrySession`, `registryStates` into a `claudeAdapter`
   implementing `Adapter`. It uses `transcriptCache` for `Sessions` and keeps
   the registry + pid-walk for `Live`. `Session.Source = "claude"`.
3. `session.go` shrinks to the generic `Session`/`SessionState` model + the
   shared helpers; the old `loader` becomes `transcriptCache`.
4. Core loader (in `ui.go`/`main.go`) becomes: for each enabled adapter, call
   `Sessions()`; concat; tag `Source`; call each adapter's `Live()` on the
   matching slice; sort globally by Activity then Modified (the existing sort
   logic moves up, unchanged). The UI, markers, unread detection, spinner all
   stay generic and work off `Session.State`/`Live()` regardless of source.

This is a pure move for Claude: behaviour is identical, the `claudeAdapter`
just wraps today's functions.

## New pi adapter (`pi.go`)

### `Sessions()`

- Root: `os.Getenv("PI_CODING_AGENT_SESSION_DIR")` or `~/.pi/agent/sessions`.
- Walk each project dir, each `*.jsonl`, via `transcriptCache`.
- `parse`:
  - Read the `session` line (head) for `cwd`, `id`, start `timestamp`, and
    `name` if present.
  - Head-scan for the first user `text` block → `Title` fallback (like Claude's
    `firstPrompt`).
  - Tail-scan for the last assistant `text` block (skip `thinking`/`toolCall`)
    → `LastMsg`; mirrors Claude's `assistantText`.
  - `Activity` = newest timestamp among **`message`** entries only (not
    `model_change`/`thinking_level_change`/`compaction`, which carry timestamps
    but aren't conversation — same spirit as Claude ignoring mode writes).
  - `Branch = gitBranch(cwd)`. `Slug = ""`. `ID = session.id` (the UUID).
  - `Source = "pi"`.
- Uses the same head/tail byte windows as Claude (`headScanBytes`/`tailScanBytes`)
  — pi transcripts can be large.

### `Live()` (best-effort without extension, authoritative with it)

**Shipped:** pane match and optional extension-based live state.

When the `agent-sessions-pi-live` extension is installed, `Live()` reads
`~/.pi/agent/live/<sid>.json` files and sets `PID`, `Pane`, and `State`
(`running`/`waiting`/`idle`) on matching pi sessions. Sessions with a marker
become `Live()` and show the live-state glyph/spinner; Enter uses the marker's
`{pane}`/`{pid}`/`{cwd}`/`{id}`. The extension is in
`extensions/pi-live-marker/`; install it by copying or symlinking it to
`~/.pi/agent/extensions/agent-sessions-pi-live/`. It writes markers on
`session_start`, `agent_start`, `agent_end`, and cleans up on `session_shutdown`.

Without the extension, `Live()` falls back to the cwd-based pane match from the
previous phase: it runs `tmux list-panes -a -F '#{pane_id} #{pane_current_path}'`
and sets `Pane` on any pi session whose cwd equals a pane's current path
(skipping agent-sessions' own `$TMUX_PANE` so a session sharing our cwd doesn't
match our pane). No PID or state is attached, so sessions stay "offline", but
the tmux glyph still marks which sessions are in a pane and Enter jumps to it.

The gotoSession guards were decoupled to support the no-extension case:
`{pane}` is substituted empty when there's none (so a template can
shell-fallback) rather than hard-blocked, while `{pid}` still requires a live
session (a PID of 0 is meaningless). The pi default enter command uses
`{pane}`/`{cwd}`/`{id}` and falls back to resuming in the current terminal when
no pane is found.

Phase 2 (mtime heuristic, optional, not implemented): infer live *state* from
file mtime recency — `State = running` if within ~3s (actively streaming) else
`idle`, live if within ~15s. Deliberately coarse; the extension is the preferred
authoritative source. The cwd pane match above is already shipped; the mtime
heuristic would add only the status word/spinner without an extension.

Phase 3 (authoritative extension): **implemented** as `extensions/pi-live-marker/`.
It follows the `skyfallsin/pi-room` pattern (`~/.pi/room/<pane>.json` with
`{pane,pid,cwd,session,registered}`, using `process.env.TMUX_PANE`) and the
`DxVapor/pi-supacode` pattern (lifecycle hooks -> status), just writing a file
our TUI reads instead of Supacode's socket protocol. Because the active pi is
the WSL pnpm install (Linux node), the extension sees `TMUX_PANE` and writes
files a Linux Go TUI reads, fixing pane and live state authoritatively.

## Config

Add a `[sources]` section (all enabled by default):

```toml
[sources]
claude = true
pi     = true

[sources.pi]
session_dir = ""   # override; empty = PI_CODING_AGENT_DIR env or ~/.pi/agent/sessions
```

Per-source Enter commands, since resume syntax differs (`claude --resume` vs
`pi --session`). Global default + per-source override:

```toml
[commands]
enter      = "cd {cwd} && claude --resume {id}"   # default fallback
background = false

[commands.pi]
enter = "cd {cwd} && pi --session {id}"
```

`gotoSession` picks `commands.<source>.enter` if set, else the global `enter`.
The existing `{pane}`/`{pid}` guards carry over: a pi session has no PID, so a
template referencing `{pid}`/`{pane}` yields the existing "no running process"
notice — which is why the pi default uses only `{cwd}`/`{id}`.

## UI

- Status bar: `---Sessions: 12 (2 running, 1 waiting, 9 idle)---` (drop the
  hardcoded "Claude"). Optional per-source counts behind a config flag.
- An optional **source tag** (a one-letter column or a glyph prefix on the
  subject) to disambiguate when two sources have sessions in the same cwd.
  Default off; cheap to add later.
- Markers/spinner/unread are already generic (keyed off `State`/`Live()`), so
  they work for any adapter that reports live state. Pi phase-1 (no `Live`)
  simply shows those sessions as offline — consistent with Claude's non-live
  sessions today.

## Phasing

1. **Interface + Claude refactor** (no behaviour change). Land the `Adapter`
   contract, `transcriptCache`, shared helpers, move Claude into `claudeAdapter`.
   Existing tests stay green.
2. **pi `Sessions()` + no-op `Live()`** + config + per-source Enter. The TUI
   now browses pi sessions; live shows offline. This alone is the bulk of the
   user-visible win.
3. **pi heuristic `Live()`** (mtime recency + tail inference + cwd→pane) and
   the optional source tag. Quality-of-life; skippable.
4. **Copilot CLI adapter** as the first proof of the "add without changing
   core" claim: one new `copilot.go` + one line in the registry.

## Decisions

- **Package layout.** Recommended: stay in `package main`, one file per adapter
  (`adapter.go`, `claude.go`, `pi.go`, future `copilot.go`) behind the
  `Adapter` interface. Adding a source = one new file + one registry line; core
  logic untouched. This already satisfies "modular, core barely changes" with
  minimal boilerplate for a single-binary personal tool. Alternative: split
  into `internal/core` + `internal/claude` + `internal/pi` packages — stronger
  isolation, but exports a pile of types and adds import-cycle discipline for
  little gain at this size. Pick this only if external contributors arrive.
- **pi live-detection scope.** Recommended: ship phase 1 (no-op `Live`) then
  decide between the heuristic (phase 2, no extension) and the marker-file
  extension (phase 3, authoritative). The extension is now known feasible (the
  active pi is WSL-native, so `TMUX_PANE` and the filesystem are visible to it)
  and has direct prior art (`pi-room`, `pi-supacode`); phase 2 is the zero-setup
  fallback. Phase 3 is the path to real running/waiting/idle markers for pi.

# agent-sessions

A Mutt-style TUI for browsing coding-agent sessions on this machine —
Claude Code, pi, and any other source with an adapter (see `ADAPTERS.md`).

```
go build -o agent-sessions .
./agent-sessions
```

## What it shows

Every session transcript from each enabled source (Claude Code's
`~/.claude/projects/`, pi's `~/.pi/agent/sessions/`, ...), most recently active
first, one line per session: index, state, last-modified time, project
directory, git branch, the tmux pane hosting the session (as
`session:window.pane`, for live sessions found in one), and a subject line
(Claude's AI-generated title, or for pi the first prompt / a `pi --name`
session). The list auto-refreshes every 2 seconds.

Ordering is by the timestamp of each transcript's last real entry, not the
file's modification time. Claude Code rewrites a transcript's mtime for
content-free changes too — a mode or permission-mode toggle, for instance —
which would otherwise float an untouched session to the top; keying off the
last timestamped entry keeps that from happening. Sessions with no timestamped
entries at all (bare mode-only stubs) sort to the bottom.

Each session's last assistant message — the thing Claude last said, e.g. the
`Done!` ending a turn — is shown too. By default it appears as an indented
detail line beneath the session, for the selected session and for the most
recently active ones, so recent answers stay on screen. See `[preview]` under
Configuration to change this to an inline column or turn it off.

Each row opens with a coloured status marker (a Nerd Font glyph by default),
so live sessions stand out at a glance:

- `running` — Claude's turn is in progress; an animated spinner in a bright
  colour
- `waiting` — blocked on the user, e.g. a permission prompt
- `idle` — waiting for the next prompt
- **unread** — a session that finished a turn (went `running` → `idle`) while
  you were watching but that you haven't opened yet, in a bright "attention"
  colour (orange by default). This is what tells apart the session that *just*
  said `Done!` from every other long-idle one. The marker clears when you open
  the session with `Enter`. It's tracked in-memory, so it only covers turns
  that finish while agent-sessions is running, and resets when you quit.

The markers and their colours are configurable — Nerd Font icons, emoji, or
plain dots — see `[status]` and the `[styles.*]` sections under Configuration.

Live sessions running inside a tmux pane — the ones the default `Enter`
command can jump to — are additionally marked with a `⊟` glyph (configurable
via `[tmux]`), so you can see which are attachable without pressing `Enter`.

State comes from `~/.claude/sessions/<pid>.json`, a registry each running
Claude Code instance maintains (status `busy`/`waiting`/`idle` plus the exact
session id). Registry files left behind by crashed processes are ignored by
checking that the pid is alive and started around the registry's `startedAt`
stamp; process inspection goes through gopsutil, so it works on both Linux
and macOS (the macOS path hasn't been smoke-tested yet).

## Sources

The app merges every enabled source's transcripts, freshest first. Each
session is tagged with its source, which drives the per-source `Enter`
command (below) and lets `/pi` or `/claude` filter the list.

- **Claude Code** — `~/.claude/projects/*/*.jsonl`, with live state from
  `~/.claude/sessions/<pid>.json` (a per-process registry with a real PID, so
  live sessions match to their tmux pane).
- **pi** — `~/.pi/agent/sessions/<encoded-cwd>/*.jsonl` (or
  `$PI_CODING_AGENT_SESSION_DIR` / `[sources.pi] session_dir`). pi itself has
  no live-process registry, but installing the optional
  `extensions/pi-live-marker/` pi extension writes
  `~/.pi/agent/live/<sid>.json` with `{pid, pane, cwd, status}` on lifecycle
  hooks; the TUI then shows pi sessions as live with running/waiting/idle
  state and jumps to their tmux pane on `Enter`. Without the extension, pi
  sessions are shown as offline for status — but the freshest still sorts to
  the top by activity, and a cwd-based pane match still lets `Enter` jump to a
  pane whose current path equals the session cwd. The pi process is a normal
  Linux process (here, a WSL pnpm install on nix node). Subject is the first
  prompt (or a `pi --name` session); git branch is read from the repo.

Enable/disable sources in config:

```toml
[sources.claude]
enabled = true
[sources.pi]
enabled = true
# session_dir = ""   # override; empty = $PI_CODING_AGENT_SESSION_DIR or ~/.pi/agent/sessions
```

Adding another source (Copilot CLI, ...) is one new adapter file implementing
the `Adapter` interface plus a `[sources.<name>]` entry — see `ADAPTERS.md`.

## Keys

| Key | Action |
| --- | --- |
| `j` / `k`, arrows | move down / up |
| `Enter` | jump to a live session's tmux pane; resume a dead session |
| `/` | search: filter the list as you type, across all projects |
| `f` | filter the list to one project, chosen via the project picker |
| `Esc` | clear the search and project filters |
| `g` / `G` | first / last session |
| `ctrl+d` / `ctrl+u` | half page down / up |
| `d` | delete the session after a y/n confirmation |
| `r` | refresh |
| `?` | help: list all keys and configured commands |
| `q` | quit |

`d` removes the session's transcript and sidecar directory from
`~/.claude/projects`, which only means `claude --resume` can no longer
offer that session — nothing a running Claude Code depends on. Sessions
with a live claude process are refused.

`/` matches case-insensitively against each session's title, project path,
branch, and session id. `f` filters the list to a single project via the
project picker. Both filters are shown in the status bar, combine with each
other, and stay until `Esc` clears them.

`Enter` runs a configurable shell command (see below). For a live session
the default finds the tmux pane whose process tree contains the session's
`claude` process and jumps there: inside tmux it switches the current
client, outside tmux it attaches. For a dead session it resumes the
conversation with `claude --resume` in a fresh tmux window.

## Configuration

Configuration lives in `$XDG_CONFIG_HOME/agent-sessions/config.toml`
(usually `~/.config/agent-sessions/config.toml`); on first run the shipped
default file — [`config.default.toml`](config.default.toml), embedded in
the binary at build time — is written there. Omitted keys keep their
defaults.

Each UI element is a `[styles.*]` section accepting `fg`/`bg` (ANSI/256
number or `#rrggbb` hex) and `bold`/`faint`/`reverse` booleans. Status
elements: `running`, `waiting`, `idle`, `unread`, `offline`, `dimmed`. Chrome:
`bar`, `selected`, `preview`. Columns: `index`, `time`, `project`, `branch`,
`subject`.

```toml
[styles.running]
fg = "#af87ff"
```

The column defaults use ANSI base colours (`"1"`–`"15"`), which resolve
through your terminal's palette — so a theme like **Catppuccin** colours the
app to match, rather than the app pinning fixed colours that ignore it. Use
`#rrggbb` hex to pin exact colours regardless of theme; the shipped default
config includes a commented Catppuccin Mocha block to copy from.

The `[icons]` section sets Nerd Font glyphs shown before the directory and
branch columns (set either to `""` to hide it, or use a plain char / emoji):

```toml
[icons]
dir    = ""   # nf-fa-folder
branch = ""   # nf-pl-branch
```

The `[display]` section chooses how the directory column is shown — the full
path or just its final segment. Search still matches the full path either way:

```toml
[display]
project = "full"   # or "name" for just the directory name
```

The `[columns]` section bounds the width of each column. The content columns
(`dir`, `branch`, `pane`, `title`) size to the longest visible value, clamped
to a per-column `min`/`max`. The `last` column is config-only — last messages
are always long, so the bounds are the actual column width. Set `max = 0` to
hide a column, and `min = max` to pin it to a fixed width (restoring the
pre-feature behaviour):

```toml
[columns]
dir    = { min = 8,  max = 28 }   # the project column
branch = { min = 8,  max = 24 }   # the git branch column
pane   = { min = 0,  max = 12 }   # the tmux pane; min = 0 lets it collapse
title  = { min = 0,  max = 30 }   # the session title (cap in preview "column" mode)
last   = { min = 0,  max = 100 }  # the last message (preview "column" mode only)
```

Bounds refer to the column's *visual* width (the icon and its 1-space
separator count against `min`/`max` when the icon is on). The defaults
recover a lot of horizontal space on narrow terminals — a session set
where every branch is `main` and every pane is `0` shrinks the row by
about 27 columns compared to the old fixed-width layout, which is the
exact complaint that motivated the feature. Hidden columns drop both the
cell and the 2-space gap before it, so the next column abuts the previous
one cleanly.

`[circleci]` enables a CI column showing the latest CircleCI status of each
session's branch (`pass`, `fail`, `run`, `hold`, or `-` for no pipelines).
Set `token` (or export `$CIRCLECI_TOKEN`/`$CIRCLE_TOKEN`); without a token
the column is hidden. Statuses are fetched in the background for visible
rows and cached for 30 seconds. The CircleCI project slug is derived from
each project's git origin remote (`github.com/org/repo` → `gh/org/repo`);
override it per directory when needed:

```toml
[circleci]
token = ""
[circleci.projects]
"~/Projects/foo" = "gh/acme/foo"
```

The `[sort]` section chooses the index order — a comma-separated list of
dimensions, most significant first. Recency (newest activity, with live
sessions floated up) is always the final tie-breaker. Two dimensions exist:
`active` puts live sessions (a running claude process) ahead of the rest, and
`repo` clusters every session of a git repo — across all its worktrees — into
one block. Their order is what matters:

```toml
[sort]
group = "activity"      # default: newest first, live floated to the top
# group = "repo"        # whole repos together, live-first inside each block
# group = "active,repo" # every live session first, grouped by repo, then the rest
```

Prefer `active,repo` when one busy repo has a large backlog of finished
sessions: with plain `repo` that backlog sits in the repo's block and can push
another repo's live session far down the list, whereas `active,repo` surfaces
every live session first (still grouped by repo) and lets the finished ones sink
behind all of them.

The `[git]` section adds an optional per-repo coloured glyph before the branch
column, giving each repo (shared across its worktrees) a consistent colour —
handy with `[sort] group = "repo"`. The branch name is tinted the same colour,
so the icon and branch read as one colour-coded unit per repo. Empty (default)
hides it. A repo's colour is picked by hashing its path, so it stays put across
runs:

```toml
[git]
icon = ""   # a glyph enables it, e.g. "" (nf-fa-git) or "◆"; "" hides it
# colors = ["2", "3", "4", "5", "6"]   # optional palette; unset = built-in
```

The `[status]` section sets the per-status marker glyphs. Defaults are Nerd
Font icons; swap them for plain dots or emoji if your terminal lacks a Nerd
Font. `running = "spinner"` animates a braille spinner instead of a static
glyph:

```toml
[status]
running = "spinner"    # or a glyph, e.g. "●" / "🟢"
waiting = "●"          # "🟡" or "󰭙"
idle    = "·"          # "⚪"
unread  = "●"          # "🟠" — shown in the [styles.unread] colour
offline = " "          # non-live sessions
words   = true         # set false for a compact, icon-only column
```

The `[preview]` section controls the last-message display:

```toml
[preview]
mode = "row"      # "row" (detail line beneath), "column" (inline), or "off"
recent = 5        # in row mode, always preview this many recent sessions...
within = "20m"    # ...that were modified within this window (a Go duration)
```

The selected session is always previewed. `recent`/`within` only apply in
`row` mode; `column` mode shows every session's message inline (capping the
subject to make room), and `off` hides it.

The `[selection]` section controls how the cursor row is drawn:

```toml
[selection]
colors = false        # true keeps column/status colours; false = reverse video
statuscolor = false   # in reverse mode, keep just the status marker/word coloured
```

By default the cursor row is clean, mutt-style **reverse video**. Reverse maps
the whole row to two colours, so per-column colours can't show — but the
running/waiting **bold is kept** (bold and reverse are independent). Set
`colors = true` to keep each column's colour too, drawing a **background
highlight** instead of reverse. The highlight colour comes from
`[styles.selected]` `bg` (a dim default is used if unset). If you like the
plain reverse bar but still want the status to read at a glance, keep `colors
= false` and set `statuscolor = true`: on the cursor row the status marker and
the running/idle/waiting word keep their own colour (so the spinner stays
coloured and animated) while the rest of the row stays reverse. Those colours
are tuned for the dark rows, so on the pale reversed bar they can look a touch
light; override them just for that row with `[selection.statuscolors]` (unset =
the normal colour — on a light terminal theme, where the reversed bar is dark,
you might pick brighter values instead):

```toml
[selection.statuscolors]
running = "2"     # darker green
unread  = "166"   # darker orange
``` Either way, pressing
`Enter` hides the highlight until the next keystroke or until the window
regains focus, so the row you were reading isn't masked while you look at the
session you opened.

The `[tmux]` section sets the marker shown on tmux-attachable sessions:

```toml
[tmux]
glyph = "⊟"   # set to "" to hide the marker
```

`[commands]` binds keys to shell commands run on the selected session. Any
Bubble Tea key name works — single characters, `enter`, or combos like
`"ctrl+x"` (quoted). The command gets the terminal while it runs, so
interactive commands work. Bindings take precedence over built-in keys;
set one to `""` to unbind it. `?` shows the active bindings.

By default a command **takes over the terminal** while it runs, so
interactive ones (an editor, `claude --resume`) work — but this briefly drops
the alt-screen, so the app appears to close and reopen. Set the top-level
`background = true` to run every command **detached** instead, without
touching the terminal (no flash). That suits the default bindings, which only
orchestrate tmux, but breaks any command that needs the terminal for input or
output. Pressing a command key also hides the cursor highlight until the next
keystroke or when the window regains focus, so the row you were reading isn't
masked while you look at the session you opened.

### Command placeholders

Every placeholder expands to a shell-quoted value, so paths and typed text
survive word-splitting.

| Placeholder | Expands to |
| --- | --- |
| `{id}` | the session id (as used by `claude --resume`) |
| `{cwd}` | the session's working directory |
| `{file}` | the path of the session's transcript (`.jsonl`) |
| `{state}` | `running`/`waiting`/`idle` for live sessions, empty otherwise |
| `{pid}` | the pid of the session's running `claude` process |
| `{pane}` | the tmux pane hosting the session's `claude` process |
| `{ci-build-url}` | the latest CircleCI build's page (needs `[circleci]`) |
| `{project-picker}` | interactive: the project chosen from a selection screen |
| `{text-input}` | interactive: a line of text typed into the status bar |

`{pid}` and `{pane}` only apply to live sessions, and `{pane}` further
requires the process to sit inside a tmux pane — commands using them show a
status-bar notice instead of running when that doesn't hold. Likewise
`{ci-build-url}` needs the session's project to have a known CircleCI slug;
it deep-links to the latest fetched workflow, falling back to the branch's
pipelines page (e.g. `b = "xdg-open {ci-build-url}"`). Appending `?`
makes them **optional**: `{pane?}` and `{pid?}` expand to an empty string
instead, so a single command can branch. That's how the default `enter`
jumps to a live session's pane but resumes a dead one:

```toml
enter = '''
if [ -n {pane?} ]; then
    tmux select-pane -t {pane?} && tmux select-window -t {pane?}
    tmux switch-client -t {pane?} 2>/dev/null || tmux attach-session -t {pane?}
else
    p=$(tmux new-window -P -F "#{pane_id}" -c {cwd})
    tmux send-keys -t "$p" "claude --resume {id}" Enter
fi
'''
```

For anything more elaborate, hand the facts to a script and decide there:
`enter = "open-session {state} {pane?} {cwd} {id}"`.

The two interactive placeholders resolve one after another before the
command runs, and compose freely with the rest. `{project-picker}` lists
every known project (the distinct working directories across all sessions,
most recently used first). `{text-input}` accepts an optional label after a
colon that names the prompt: `{text-input:Prompt}`. `Esc` at any step
cancels the whole command.

```toml
[commands]
enter = "cd {cwd} && claude --resume {id}"
o = "cd {cwd} && $EDITOR ."
t = "less +G {file}"
n = "cd {project-picker} && claude"
```

### Command log

Every command run from a binding is appended, with timestamps, exit status,
duration, and everything it printed, to
`$XDG_STATE_HOME/agent-sessions/commands.log` (usually
`~/.local/state/agent-sessions/commands.log`). When a command fails, the
status-bar notice points there — that's the first place to look when a
binding misbehaves. Commands run with the real terminal as their input, so
they stay fully interactive; only their output is captured.

## Tip: create a new Claude session from the TUI

The shipped default binds `c` to pick a project, ask for the opening
prompt, and start `claude` with it in a fresh tmux window:

```toml
[commands]
c = '''
p=$(tmux new-window -P -F "#{pane_id}" -c {project-picker})
tmux send-keys -t "$p" "claude {text-input:Prompt}" Enter
'''
```

Note the shape: the window is opened with *no* command — so it starts your
normal interactive shell, applying whatever environment setup you use
(rc files, version managers such as asdf, per-directory environments such
as direnv) — and the claude invocation is then typed into it with
`send-keys`. The simpler `tmux new-window -c {project-picker} "claude ..."`
would run claude via a non-interactive shell where none of that setup
applies. The default `enter` uses the same pattern for its dead-session
branch. Use `split-window` instead of `new-window` for a pane in the
current window.

Quoting subtlety: the expanded `{text-input:...}` value is single-quote
escaped, and the double-quote wrapper hands it intact to the window's
shell — a prompt containing a literal `"` is the one thing it can't carry.

### Per-source Enter overrides

A `[sources.<name>] enter` setting overrides the `[commands]` `enter` binding
for that source's sessions — useful because resume syntax differs (`claude
--resume` vs `pi --session`). pi has no live PID (no per-process registry), so
its command uses `{pane}` (found by matching the session cwd to a tmux pane's
current path) plus `{cwd}`/`{id}`, not `{pid}`. With a pane, `Enter` jumps to
pi's tmux pane; without one (or outside tmux) it falls back to resuming pi in
the current terminal:

```toml
[sources.pi]
enter = "tmux select-pane -t {pane} && tmux select-window -t {pane} && tmux switch-client -t {pane} 2>/dev/null || (cd {cwd} && pi --session {id})"
```
```

`{pane}` is substituted empty when a session has no pane, so the template's
`||` fallback runs rather than the command being blocked. (`{pid}`, by
contrast, still requires a live session — a PID of 0 is meaningless.)

By default the command **takes over the terminal** while it runs, so
interactive commands work (`tmux attach`, an editor, `claude --resume`). That
briefly drops the alt-screen, so the app appears to close and reopen. Set
`background = true` to run the command **detached**, without touching the
terminal — no flash — for commands that only switch a tmux client or focus a
pane and need no input or output (e.g. `switch-client` plus a focus keystroke
when you run the app in one split and your sessions in another).

### pi live-state extension

pi has no built-in per-process registry, so by default pi sessions appear
**offline** in the TUI. Install the bundled `extensions/pi-live-marker/`
extension to make them live:

```bash
ln -s "$(pwd)/extensions/pi-live-marker" "$HOME/.pi/agent/extensions/agent-sessions-pi-live"
```

Then restart pi, or run `/reload` inside it. The extension writes
`~/.pi/agent/live/<sid>.json` on `session_start`, `agent_start`, `agent_end`,
and deletes it on `session_shutdown`. The TUI reads these markers to show
`running`/`waiting`/`idle` state, the process pid, and the tmux pane — and
jumps to that pane on `Enter`. Without the extension, `Enter` still jumps to a
pane found by cwd match (best-effort) and falls back to resuming pi in the
current terminal.


## Tip: a tmux key that jumps to agent-sessions

To hop to the TUI from anywhere in tmux (starting it if it isn't running),
save this as an executable script, e.g. `~/.local/bin/agent-sessions-focus`:

```sh
#!/bin/sh
# Jump to the pane running agent-sessions, starting it if absent.
pane=$(tmux list-panes -a -F '#{pane_id} #{pane_current_command}' \
    | awk '$2 == "agent-sessions" {print $1; exit}')
if [ -n "$pane" ]; then
    tmux select-window -t "$pane"
    tmux select-pane -t "$pane"
    tmux switch-client -t "$pane"
else
    # Start via the window's interactive shell so the TUI (and every
    # command it runs) gets the full user environment, not the tmux
    # server's.
    win=$(tmux new-window -P -F '#{pane_id}' -n sessions)
    tmux send-keys -t "$win" 'exec agent-sessions' Enter
fi
```

and bind it in `~/.tmux.conf` (`prefix S`, or use `bind-key -n M-s` for a
prefix-less key):

```tmux
bind-key S run-shell ~/.local/bin/agent-sessions-focus
```

If two copies of the TUI are running, the script picks the first pane it
finds, and the match is on the binary name — adjust the awk pattern if you
install it under a different name.

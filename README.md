# agent-sessions

A Mutt-style TUI for browsing Claude Code sessions on this machine.

```
go build -o agent-sessions .
./agent-sessions
```

## What it shows

Every session transcript under `~/.claude/projects/`, most recently active
first, one line per session: index, state, last-modified time, project
directory, git branch, and a subject line (the session's AI-generated title,
falling back to the first typed prompt). The list auto-refreshes every 2
seconds.

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

## Keys

| Key | Action |
| --- | --- |
| `j` / `k`, arrows | move down / up |
| `Enter` | switch to the session's tmux pane |
| `/` | search: filter the list as you type, across all projects |
| `Esc` | clear the search limit |
| `g` / `G` | first / last session |
| `ctrl+d` / `ctrl+u` | half page down / up |
| `r` | refresh |
| `q` | quit |

`/` matches case-insensitively against each session's title, project path,
branch, and session id. `Enter` keeps the match as a limit (shown in the
status bar) until `Esc` clears it.

`Enter` runs a configurable shell command (see below). The default finds the
tmux pane whose process tree contains the session's `claude` process and
jumps there: inside tmux it switches the current client, outside tmux it
attaches. Sessions the command's placeholders can't apply to (no live
process, not under tmux) get a status-bar notice instead.

## Configuration

Configuration lives in `$XDG_CONFIG_HOME/agent-sessions/config.toml`
(usually `~/.config/agent-sessions/config.toml`); a commented default file
is written on first run. Omitted keys keep their defaults.

Each UI element — `running`, `waiting`, `idle`, `unread`, `offline`,
`dimmed`, `bar`, `selected`, `preview` — is a `[styles.*]` section accepting
`fg`/`bg` (ANSI/256 number or `#rrggbb` hex) and `bold`/`faint`/`reverse`
booleans:

```toml
[styles.running]
fg = "#af87ff"
```

The `[status]` section sets the per-status marker glyphs. Defaults are Nerd
Font icons; swap them for plain dots or emoji if your terminal lacks a Nerd
Font. `running = "spinner"` animates a braille spinner instead of a static
glyph:

```toml
[status]
running = "spinner"   # or a glyph, e.g. "●" / "🟢"
waiting = "●"          # "🟡"
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

The `[tmux]` section sets the marker shown on tmux-attachable sessions:

```toml
[tmux]
glyph = "⊟"   # set to "" to hide the marker
```

`[commands] enter` is the shell command bound to `Enter`. `{id}`, `{pid}`,
`{cwd}`, `{file}` and `{pane}` expand to shell-quoted values ({pane} being
the tmux pane hosting the session's claude process), and the command gets
the terminal while it runs, so interactive commands work:

```toml
[commands]
enter = "cd {cwd} && claude --resume {id}"
```

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
    tmux new-window -n sessions agent-sessions
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

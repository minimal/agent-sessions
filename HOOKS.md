# Claude Code notification hooks (tmux)

Recipes for getting notified when a Claude Code session running in **tmux**
finishes a turn (or needs input) — the same idea Supacode wires up for its own
surface, but for plain tmux panes. They pair well with `agent-sessions`: the
notification tells you *which* pane just finished so you can jump to it.

These are **Claude Code hooks**, configured in `~/.claude/settings.json` under
`hooks`. Each hook event holds an array of objects; you can have several
independent hooks per event. Adding one of these does **not** touch any
Supacode-managed hook — it's a separate entry in the same array.

## The guard

Every recipe starts with the same guard so it fires **only** in a tmux pane and
**never** when running under Supacode (which has its own hooks):

```sh
[ -z "${SUPACODE_SURFACE_ID:-}" ] && [ -n "${TMUX:-}" ] && { ... }
```

`$TMUX_PANE` (e.g. `%26`) identifies the exact pane, so the notification can
name the window and — with `terminal-notifier` — focus it on click.

Trailing `>/dev/null 2>&1 || true` keeps a hook failure from ever blocking the
turn. The `# tmux-stop-notify` comment is just a marker for easy dedup/removal.

## How to add one manually

1. Open `~/.claude/settings.json`.
2. Under `"hooks"`, find (or create) the event array — `"Stop"` for
   "finished a turn", `"Notification"` for "waiting on you".
3. Append the recipe's object to that array (keep any existing objects).
4. Reload: open `/hooks` once in Claude Code, or restart. A malformed
   `settings.json` silently disables **all** settings, so validate after:
   `jq . ~/.claude/settings.json >/dev/null && echo ok`.

The `command` in `settings.json` must be a single JSON string, so the shell has
to be escaped (`"` → `\"`, `\` → `\\`). Each recipe below gives the readable
shell first, then the ready-to-paste JSON object.

---

## Recipe 1 — Stop hook, native macOS notification (no dependencies) — recommended

Uses `/usr/bin/osascript`, always present on macOS, and works with no
notification-permission setup (notifications post as "Script Editor", which is
allowed by default). No click action. This is the reliable default; use
Recipe 2 only if you've authorised `terminal-notifier` (see its note).

Readable shell:

```sh
[ -z "${SUPACODE_SURFACE_ID:-}" ] && [ -n "${TMUX:-}" ] && {
  __dir=$(tmux display-message -p -t "${TMUX_PANE:-}" "#{pane_current_path}" 2>/dev/null); __dir=${__dir:-$PWD}
  __win=$(tmux display-message -p -t "${TMUX_PANE:-}" "#{window_index}:#{window_name}" 2>/dev/null)
  __msg="${__dir##*/}"; __msg=${__msg//\\/}; __msg=${__msg//\"/}
  __sub="tmux ${__win}"; __sub=${__sub//\"/}
  /usr/bin/osascript -e "display notification \"$__msg\" with title \"Claude finished\" subtitle \"$__sub\" sound name \"Glass\""
} >/dev/null 2>&1 || true   # tmux-stop-notify
```

Paste into `hooks.Stop`:

```json
{
  "hooks": [
    {
      "type": "command",
      "command": "[ -z \"${SUPACODE_SURFACE_ID:-}\" ] && [ -n \"${TMUX:-}\" ] && { __dir=$(tmux display-message -p -t \"${TMUX_PANE:-}\" \"#{pane_current_path}\" 2>/dev/null); __dir=${__dir:-$PWD}; __win=$(tmux display-message -p -t \"${TMUX_PANE:-}\" \"#{window_index}:#{window_name}\" 2>/dev/null); __msg=\"${__dir##*/}\"; __msg=${__msg//\\\\/}; __msg=${__msg//\\\"/}; __sub=\"tmux ${__win}\"; __sub=${__sub//\\\"/}; /usr/bin/osascript -e \"display notification \\\"$__msg\\\" with title \\\"Claude finished\\\" subtitle \\\"$__sub\\\" sound name \\\"Glass\\\"\"; } >/dev/null 2>&1 || true # tmux-stop-notify",
      "timeout": 10
    }
  ]
}
```

---

## Recipe 2 — Stop hook, terminal-notifier (clickable → focuses the pane)

`terminal-notifier` gives a nicer notification and, via `-execute`, focuses the
originating tmux pane/window when you click it. Falls back to `osascript` if the
binary is missing. Install with `brew install terminal-notifier`.

Because it posts a real macOS notification, it works the same in **any**
terminal (Ghostty, iTerm, …) — no per-terminal setup. Use the **Homebrew**
build (`/opt/homebrew/bin/terminal-notifier`, properly signed): an
adhoc-signed build (e.g. from the Nix store) registers its permission but
macOS silently drops its banners, so it appears "allowed" yet shows nothing.
`which -a terminal-notifier` — if a Nix/store path shadows the Homebrew one on
your `PATH`, reference the Homebrew path explicitly as below.

Readable shell:

```sh
[ -z "${SUPACODE_SURFACE_ID:-}" ] && [ -n "${TMUX:-}" ] && {
  __p="${TMUX_PANE:-}"
  __dir=$(tmux display-message -p -t "$__p" "#{pane_current_path}" 2>/dev/null); __dir=${__dir:-$PWD}
  __win=$(tmux display-message -p -t "$__p" "#{window_index}:#{window_name}" 2>/dev/null)
  __tn="/opt/homebrew/bin/terminal-notifier"
  if [ -x "$__tn" ]; then
    "$__tn" -title "Claude finished" -subtitle "tmux ${__win}" -message "${__dir##*/}" \
            -sound Glass -group "claude-$__p" \
            -execute "/opt/homebrew/bin/tmux select-pane -t $__p && /opt/homebrew/bin/tmux select-window -t $__p && /opt/homebrew/bin/tmux switch-client -t $__p"
  else
    __m="${__dir##*/}"; __m=${__m//\\/}; __m=${__m//\"/}
    __s="tmux ${__win}"; __s=${__s//\"/}
    /usr/bin/osascript -e "display notification \"$__m\" with title \"Claude finished\" subtitle \"$__s\" sound name \"Glass\""
  fi
} >/dev/null 2>&1 || true   # tmux-stop-notify
```

> **Authorisation required.** `terminal-notifier` posts under its own app
> identity, which macOS drops silently (the command still exits 0) until you
> allow it: System Settings ▸ Notifications ▸ **terminal-notifier** ▸ Allow
> Notifications. It only appears in that list after it has run at least once. If
> it never shows even after enabling (common with unsigned/Nix builds), stick
> with Recipe 1 — `osascript` needs no setup. To open the pane directly:
> `open "x-apple.systempreferences:com.apple.Notifications-Settings.extension"`.

Notes:
- `-group "claude-$__p"` coalesces repeated notifications from the same pane so
  they replace rather than stack.
- `-execute` runs when you click. **Use the absolute path to `tmux`** — the
  click command runs under a minimal `launchd` PATH (`/usr/bin:/bin`), so a bare
  `tmux` (in `/opt/homebrew/bin`) is "command not found" and the click does
  nothing. `switch-client` is included so it focuses even across sessions.
- To also bring the terminal *app* to the front on click, resolve the tmux
  client's terminal and append `; open '<app>'` to the click command. That plus
  the ancestry walk is unwieldy inline — use the helper-script variant below.

Paste into `hooks.Stop`:

```json
{
  "hooks": [
    {
      "type": "command",
      "command": "[ -z \"${SUPACODE_SURFACE_ID:-}\" ] && [ -n \"${TMUX:-}\" ] && { __p=\"${TMUX_PANE:-}\"; __dir=$(tmux display-message -p -t \"$__p\" \"#{pane_current_path}\" 2>/dev/null); __dir=${__dir:-$PWD}; __win=$(tmux display-message -p -t \"$__p\" \"#{window_index}:#{window_name}\" 2>/dev/null); __tn=\"/opt/homebrew/bin/terminal-notifier\"; if [ -x \"$__tn\" ]; then \"$__tn\" -title \"Claude finished\" -subtitle \"tmux ${__win}\" -message \"${__dir##*/}\" -sound Glass -group \"claude-$__p\" -execute \"/opt/homebrew/bin/tmux select-pane -t $__p && /opt/homebrew/bin/tmux select-window -t $__p && /opt/homebrew/bin/tmux switch-client -t $__p\"; else __m=\"${__dir##*/}\"; __m=${__m//\\\\/}; __m=${__m//\\\"/}; __s=\"tmux ${__win}\"; __s=${__s//\\\"/}; /usr/bin/osascript -e \"display notification \\\"$__m\\\" with title \\\"Claude finished\\\" subtitle \\\"$__s\\\" sound name \\\"Glass\\\"\"; fi; } >/dev/null 2>&1 || true # tmux-stop-notify",
      "timeout": 10
    }
  ]
}
```

---

## Recipe 2b — helper script (click focuses the pane *and* raises the terminal)

Once the click action grows (absolute `tmux` path, `switch-client`, and raising
the hosting terminal app), inlining it in `settings.json` is painful to escape —
and every tweak trips the auto-mode gate on editing startup config. Put the
logic in a script and let the hook just call it. Bonus: it foregrounds the
correct terminal (Ghostty, iTerm, …) by walking the tmux client's process
ancestry to the owning `.app`.

Save as `~/.claude/bin/claude-tmux-notify` and `chmod +x` it. It takes three
optional args — `$1` title, `$2` sound, `$3` group tag — so the *same* script
serves both the Stop hook (defaults) and the AskUserQuestion hook (Recipe 4).
The group tag keeps the two in separate coalescing groups, so a later
"finished" alert doesn't replace a pending "needs input" one:

```bash
#!/bin/bash
# Notify about a Claude event in a tmux pane. Clicking focuses the
# pane/window/session and foregrounds its terminal app. Fires only in tmux,
# never under Supacode.
#   Stop hook            -> defaults ("Claude finished", Glass, "stop")
#   AskUserQuestion hook -> "Claude needs input" Ping ask
[ -n "${SUPACODE_SURFACE_ID:-}" ] && exit 0
[ -z "${TMUX:-}" ] && exit 0

title="${1:-Claude finished}"
sound="${2:-Glass}"
tag="${3:-stop}"

TB=/opt/homebrew/bin/tmux
p="${TMUX_PANE:-}"
dir=$("$TB" display-message -p -t "$p" '#{pane_current_path}' 2>/dev/null); dir=${dir:-$PWD}
win=$("$TB" display-message -p -t "$p" '#{window_index}:#{window_name}' 2>/dev/null)

# Terminal .app hosting this tmux client (client_pid -> ancestry -> *.app).
app=""; cpid=$("$TB" display-message -p -t "$p" '#{client_pid}' 2>/dev/null)
while [ -n "$cpid" ] && [ "$cpid" -gt 1 ] 2>/dev/null; do
  comm=$(ps -o comm= -p "$cpid" 2>/dev/null)
  case "$comm" in */*.app/*) app="${comm%%.app/*}.app"; break ;; esac
  cpid=$(ps -o ppid= -p "$cpid" 2>/dev/null | tr -d ' ')
done

focus="$TB select-pane -t $p; $TB select-window -t $p; $TB switch-client -t $p"
[ -n "$app" ] && focus="$focus; open '$app'"

TN=/opt/homebrew/bin/terminal-notifier
if [ -x "$TN" ]; then
  "$TN" -title "$title" -subtitle "tmux ${win}" -message "${dir##*/}" \
        -sound "$sound" -group "claude-$tag-$p" -execute "$focus"
else
  m=${dir##*/}; m=${m//\\/}; m=${m//\"/}; s="tmux ${win}"; s=${s//\"/}; t=${title//\"/}
  /usr/bin/osascript -e "display notification \"$m\" with title \"$t\" subtitle \"$s\" sound name \"$sound\""
fi
exit 0
```

Then the `hooks.Stop` object is trivial (and easy to escape):

```json
{
  "hooks": [
    {
      "type": "command",
      "command": "[ -x \"$HOME/.claude/bin/claude-tmux-notify\" ] && \"$HOME/.claude/bin/claude-tmux-notify\" >/dev/null 2>&1 || true # tmux-stop-notify",
      "timeout": 10
    }
  ]
}
```

---

## Recipe 3 — Notification hook (pinged when Claude is waiting on you)

Same idea on the `Notification` event, which fires when Claude needs input (a
permission prompt or a question). Swap the title so you can tell the two apart.
Add this object to `hooks.Notification` (not `hooks.Stop`):

```json
{
  "hooks": [
    {
      "type": "command",
      "command": "[ -z \"${SUPACODE_SURFACE_ID:-}\" ] && [ -n \"${TMUX:-}\" ] && { __p=\"${TMUX_PANE:-}\"; __dir=$(tmux display-message -p -t \"$__p\" \"#{pane_current_path}\" 2>/dev/null); __dir=${__dir:-$PWD}; __tn=\"/opt/homebrew/bin/terminal-notifier\"; [ -x \"$__tn\" ] && \"$__tn\" -title \"Claude needs input\" -message \"${__dir##*/}\" -sound Ping -group \"claude-$__p\" -execute \"/opt/homebrew/bin/tmux select-pane -t $__p && /opt/homebrew/bin/tmux select-window -t $__p && /opt/homebrew/bin/tmux switch-client -t $__p\" || /usr/bin/osascript -e \"display notification \\\"${__dir##*/}\\\" with title \\\"Claude needs input\\\" sound name \\\"Ping\\\"\"; } >/dev/null 2>&1 || true # tmux-notify-input",
      "timeout": 10
    }
  ]
}
```

---

## Recipe 4 — AskUserQuestion hook (pinged when Claude asks you a question)

The `Notification` event (Recipe 3) covers permission prompts, but Claude's
`AskUserQuestion` tool — the multiple-choice questions it poses mid-task — is a
*tool call*, so it's easy to miss if you only watch `Notification`. Catch it
with a `PreToolUse` hook matching `AskUserQuestion`, which fires exactly when
the question is presented. It reuses the Recipe 2b helper script with a
different title/sound/tag:

```json
{
  "matcher": "AskUserQuestion",
  "hooks": [
    {
      "type": "command",
      "command": "[ -x \"$HOME/.claude/bin/claude-tmux-notify\" ] && \"$HOME/.claude/bin/claude-tmux-notify\" \"Claude needs input\" Ping ask >/dev/null 2>&1 || true # tmux-ask-notify",
      "timeout": 10
    }
  ]
}
```

Append this object to `hooks.PreToolUse` (it's an array; keep any existing
entries, including Supacode's `awaiting_input` one). The `matcher` is a regex
over the tool name — add `ExitPlanMode` (`"AskUserQuestion|ExitPlanMode"`) to
also get pinged when Claude presents a plan for approval. `|| true` keeps the
hook non-blocking, so it never delays or suppresses the question itself.

---

## Removing a hook

Delete its object from the event array (find it by the `# tmux-stop-notify` /
`# tmux-notify-input` / `# tmux-ask-notify` marker), then reload via `/hooks` or
restart.

---

## Bonus: focus-follows-Enter (agent-sessions + Ghostty splits)

Not a Claude Code hook — this is agent-sessions' own `[commands] enter`. If you
run the app in one Ghostty split and your tmux sessions in another, pressing
Enter switches the tmux client to the target pane but leaves keyboard focus on
the app's split; you then have to hit the split-nav key yourself. Ghostty has
no remote-control CLI to focus a split by target, so the workaround is to
simulate its split-focus keybind with an AppleScript keystroke appended to the
Enter command.

In `$XDG_CONFIG_HOME/agent-sessions/config.toml` (macOS:
`~/Library/Application Support/agent-sessions/config.toml`):

```toml
[commands]
# Switch the tmux client to the pane, then move Ghostty focus to the split
# below (app on top, sessions below). key code 125 = down arrow.
enter = "tmux select-pane -t {pane} && tmux select-window -t {pane} && tmux switch-client -t {pane} 2>/dev/null; osascript -e 'tell application \"System Events\" to key code 125 using {command down, option down}'"
```

Requirements and gotchas — all learned the hard way:

- **The app must run *outside* tmux** (a plain shell in its Ghostty split). The
  synthetic keystroke is only permitted when the process macOS holds
  *responsible* is an app you've granted. Run the app under tmux and the chain
  dead-ends at the tmux **server daemon** (not Ghostty), so the keystroke fails
  with `execution error: … not allowed to send keystrokes (1002)` no matter
  what you grant. The app doesn't need tmux — it only drives it via CLI.
- **Grant Ghostty Accessibility**: System Settings ▸ Privacy & Security ▸
  Accessibility ▸ add/enable **Ghostty**. (Adding it is what matters; a restart
  isn't required once it's in the list.)
- **Restart agent-sessions** after editing `enter` — it reads config at startup.
- **Direction = your layout.** `cmd+alt+arrow` is `goto_split:<dir>` on Ghostty
  defaults: down `125`, up `126`, left `123`, right `124` (with
  `{command down, option down}`). Or `cmd+]` = next split: key code `30` with
  `command down` (fine when there are only two splits).
- **Ghostty-specific.** iTerm can select a split precisely via AppleScript;
  kitty (`kitty @ focus-window`) and WezTerm (`wezterm cli`) have proper
  remote-control CLIs. Only fires for live, in-tmux sessions (the app's `{pane}`
  guard skips the rest).

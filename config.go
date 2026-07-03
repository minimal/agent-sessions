package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
)

// defaultConfigTOML is both the shipped default configuration and the file
// written to the user's config directory on first run.
const defaultConfigTOML = `# agent-sessions configuration.
#
# Each style section accepts:
#   fg, bg                 colour: an ANSI/256 number ("0"-"255") or hex ("#rrggbb")
#   bold, faint, reverse   boolean attributes
# Omitted keys keep their defaults.

[styles.running]   # sessions whose turn is in progress
fg = "10"
bold = true

[styles.waiting]   # sessions blocked on the user, e.g. a permission prompt
fg = "3"
bold = true

[styles.idle]      # sessions waiting for the next prompt
fg = "6"

[styles.unread]    # sessions that finished a turn you haven't opened yet
fg = "208"
bold = true

[styles.offline]   # sessions with no running claude process
faint = true

[styles.dimmed]    # sessions with no activity for over a day
faint = true

[styles.bar]       # the help and status bars
reverse = true

[styles.selected]  # the cursor row
reverse = true

[styles.preview]   # the last-message text shown per session
faint = true

[status]
# Marker shown at the start of each row for its status. Defaults are Nerd
# Font icons (need a Nerd Font in your terminal). For a plain terminal use
# dots:   running="●" waiting="●" idle="·" unread="●" offline=" "
# or emoji: running="🟢" waiting="🟡" idle="⚪" unread="🟠" offline=" "
# running = "spinner" animates a braille spinner instead of a static glyph.
running = "spinner"
waiting = "\uf0f3"
idle    = "\uf10c"
unread  = "\uf111"
offline = " "
# Show the state word (running/waiting/idle) next to the glyph. Set false for
# a compact, icon-only column.
words = true

[preview]
# Show each session's last assistant message (e.g. the "Done!" ending a
# turn). mode is "row" (a detail line beneath the session), "column" (an
# extra column on the session's own row), or "off".
mode = "row"
# The selected session is always previewed. In "row" mode the most recent
# sessions are too, so recent answers stay on screen without moving the
# cursor: up to "recent" sessions modified within "within" (a Go duration).
recent = 5
within = "20m"

[tmux]
# Marker shown next to live sessions running inside a tmux pane, i.e. the
# ones the default Enter command can jump to. Set to "" to hide it.
glyph = "⊟"

[commands]
# Shell command run when pressing Enter on a session. {id}, {pid}, {cwd},
# {file} and {pane} expand to shell-quoted values; {pane} is the tmux pane
# hosting the session's claude process, and commands that use it are only
# run for sessions found in a pane. The default jumps to that pane:
# switch-client moves the client when run inside tmux, attach-session
# takes over the terminal when run outside it.
enter = "tmux select-pane -t {pane} && tmux select-window -t {pane} && tmux switch-client -t {pane} 2>/dev/null || tmux attach-session -t {pane}"
`

// Config is the user-tunable configuration.
type Config struct {
	Styles struct {
		Running  StyleConfig `toml:"running"`
		Waiting  StyleConfig `toml:"waiting"`
		Idle     StyleConfig `toml:"idle"`
		Unread   StyleConfig `toml:"unread"`
		Offline  StyleConfig `toml:"offline"`
		Dimmed   StyleConfig `toml:"dimmed"`
		Bar      StyleConfig `toml:"bar"`
		Selected StyleConfig `toml:"selected"`
		Preview  StyleConfig `toml:"preview"`
	} `toml:"styles"`
	Status struct {
		Running string `toml:"running"` // "spinner" animates; else a literal glyph
		Waiting string `toml:"waiting"`
		Idle    string `toml:"idle"`
		Unread  string `toml:"unread"`
		Offline string `toml:"offline"`
		Words   bool   `toml:"words"` // show the state word next to the glyph
	} `toml:"status"`
	Preview struct {
		Mode   string `toml:"mode"`   // "row", "column", or "off"
		Recent int    `toml:"recent"` // max recent sessions to always preview
		Within string `toml:"within"` // recency window, a Go duration string
	} `toml:"preview"`
	Tmux struct {
		Glyph string `toml:"glyph"` // marker on tmux-attachable sessions; "" hides it
	} `toml:"tmux"`
	Commands struct {
		Enter string `toml:"enter"`
	} `toml:"commands"`
}

// PreviewWithin parses the recency window, falling back to 20m if unset or
// malformed so a bad config still shows recent previews rather than none.
func (c Config) PreviewWithin() time.Duration {
	if d, err := time.ParseDuration(c.Preview.Within); err == nil {
		return d
	}
	return 20 * time.Minute
}

// StyleConfig describes one visual element of the UI.
type StyleConfig struct {
	Fg      string `toml:"fg"`
	Bg      string `toml:"bg"`
	Bold    bool   `toml:"bold"`
	Faint   bool   `toml:"faint"`
	Reverse bool   `toml:"reverse"`
}

func (sc StyleConfig) style() lipgloss.Style {
	st := lipgloss.NewStyle()
	if sc.Fg != "" {
		st = st.Foreground(lipgloss.Color(sc.Fg))
	}
	if sc.Bg != "" {
		st = st.Background(lipgloss.Color(sc.Bg))
	}
	return st.Bold(sc.Bold).Faint(sc.Faint).Reverse(sc.Reverse)
}

// expandCommand substitutes {key} placeholders in a command template with
// shell-quoted values.
func expandCommand(tmpl string, vars map[string]string) string {
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", shellQuote(v))
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// configPath returns the XDG-style location of the config file.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-sessions", "config.toml"), nil
}

// loadConfig returns the defaults overlaid with the user's config file,
// writing a default file first if none exists yet.
func loadConfig() (Config, error) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		return cfg, fmt.Errorf("built-in default config: %w", err)
	}
	path, err := configPath()
	if err != nil {
		return cfg, nil // no config dir on this system; run with defaults
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Best-effort: an unwritable config dir shouldn't stop the TUI.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			os.WriteFile(path, []byte(defaultConfigTOML), 0o644)
		}
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

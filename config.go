package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
)

// defaultConfigTOML is both the shipped default configuration and the file
// written to the user's config directory on first run.
//
//go:embed config.default.toml
var defaultConfigTOML string

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
		Index    StyleConfig `toml:"index"`    // the leading row number
		Time     StyleConfig `toml:"time"`     // the last-activity timestamp
		Project  StyleConfig `toml:"project"`  // the directory column
		Branch   StyleConfig `toml:"branch"`   // the git branch column
		Model    StyleConfig `toml:"model"`    // the model column
		Subject  StyleConfig `toml:"subject"`  // the session title column
		Worktree StyleConfig `toml:"worktree"` // marker shown next to worktree projects
	} `toml:"styles"`
	Commands map[string]string `toml:"commands"`
	CircleCI struct {
		Token    string            `toml:"token"`
		Projects map[string]string `toml:"projects"`
	} `toml:"circleci"`
	Icons struct {
		Branch string `toml:"branch"` // shown before the branch; "" hides it
		Dir    string `toml:"dir"`    // shown before the directory; "" hides it
	} `toml:"icons"`
	Display struct {
		Project           string            `toml:"project"`            // "full" path or just the "name"
		ModelReplacements map[string]string `toml:"model_replacements"` // literal model-name fragment replacements
	} `toml:"display"`
	Selection struct {
		Colors       bool     `toml:"colors"`      // keep column/status colours on the cursor row
		StatusColor  bool     `toml:"statuscolor"` // in reverse mode, keep the status marker/word coloured
		StatusColors struct { // override those colours just for the reversed row
			Running string `toml:"running"`
			Waiting string `toml:"waiting"`
			Idle    string `toml:"idle"`
			Unread  string `toml:"unread"`
		} `toml:"statuscolors"`
	} `toml:"selection"`
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
	Columns  ColumnBounds `toml:"columns"`
	Worktree struct {
		Glyph string `toml:"glyph"` // marker on worktree projects; "" hides it
	} `toml:"worktree"`
	Tmux struct {
		Glyph string `toml:"glyph"` // marker on tmux-attachable sessions; "" hides it
	} `toml:"tmux"`
	Mouse struct {
		Enabled     bool   `toml:"enabled"`      // capture mouse events at all
		ClickAction string `toml:"click_action"` // "select" or "select-switch"
	} `toml:"mouse"`
	Sort struct {
		Group string `toml:"group"` // "activity" (default) or "repo"
	} `toml:"sort"`
	Filter struct {
		Running bool `toml:"running"` // start with the "only running" filter ('o') already applied
	} `toml:"filter"`
	Git struct {
		Icon   string   `toml:"icon"`   // per-repo glyph; "" hides the column
		Colors []string `toml:"colors"` // palette cycled per repo; empty uses a built-in one
	} `toml:"git"`
	Background               bool  `toml:"background"`                  // run key-bound commands detached (no terminal takeover)
	QuickTrashThresholdBytes int64 `toml:"quick_trash_threshold_bytes"` // y/n at or below; typed "yes" above
	// Sources enables/disables each adapter. Sections absent from the user's
	// config keep the defaults below (enabled). [sources.<name>] may carry an
	// optional `enter` override (per-source resume syntax differs) and
	// source-specific options (e.g. pi's session_dir).
	Sources struct {
		Claude struct {
			Enabled bool   `toml:"enabled"`
			Enter   string `toml:"enter"` // overrides the "enter" command for claude sessions
			Glyph   string `toml:"glyph"` // identifies claude sessions; "" hides it
			Color   string `toml:"color"` // glyph colour; "" uses the default text colour
		} `toml:"claude"`
		Pi struct {
			Enabled    bool   `toml:"enabled"`
			SessionDir string `toml:"session_dir"` // "" = $PI_CODING_AGENT_SESSION_DIR or ~/.pi/agent/sessions
			Enter      string `toml:"enter"`       // overrides the "enter" command for pi sessions
			Glyph      string `toml:"glyph"`       // identifies pi sessions; "" hides it
			Color      string `toml:"color"`       // glyph colour; "" uses the default text colour
		} `toml:"pi"`
		Copilot struct {
			Enabled    bool   `toml:"enabled"`
			SessionDir string `toml:"session_dir"` // "" = $COPILOT_HOME/session-state or ~/.copilot/session-state
			Enter      string `toml:"enter"`       // overrides the "enter" command for copilot sessions
			Glyph      string `toml:"glyph"`       // identifies copilot sessions; "" hides it
			Color      string `toml:"color"`       // glyph colour; "" uses the default text colour
		} `toml:"copilot"`
	} `toml:"sources"`
}

// ColumnConfig bounds the width of one column. For "content" columns (dir,
// branch, model, pane, title) the width is clamp(observed, Min, Max), where
// observed is the longest value across visible sessions. For the "last"
// column the width is clamp(rest, Min, Max), where rest is whatever's left
// of the row after the other columns -- last messages are always long, so
// the bound is the column width directly. Set Max = 0 to hide the column.
type ColumnConfig struct {
	Min int `toml:"min"`
	Max int `toml:"max"`
}

// ColumnBounds groups the per-column width configuration. Each field is a
// ColumnConfig with min/max bounds; max=0 hides the column.
type ColumnBounds struct {
	Dir    ColumnConfig `toml:"dir"`    // the project column
	Branch ColumnConfig `toml:"branch"` // the git branch column
	Model  ColumnConfig `toml:"model"`  // the model column
	Pane   ColumnConfig `toml:"pane"`   // the tmux pane
	Title  ColumnConfig `toml:"title"`  // the session title (cap in preview "column" mode)
	Last   ColumnConfig `toml:"last"`   // the last message (preview "column" mode only)
}

// ciToken returns the configured CircleCI token, falling back to the
// conventional environment variables.
func (c Config) ciToken() string {
	if c.CircleCI.Token != "" {
		return c.CircleCI.Token
	}
	if t := os.Getenv("CIRCLECI_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("CIRCLE_TOKEN")
}

// ciOverrides returns the per-directory slug overrides with ~ expanded.
func (c Config) ciOverrides() map[string]string {
	out := map[string]string{}
	home, _ := os.UserHomeDir()
	for dir, slug := range c.CircleCI.Projects {
		if home != "" {
			if rest, ok := strings.CutPrefix(dir, "~"); ok {
				dir = home + rest
			}
		}
		out[dir] = slug
	}
	return out
}

// SortDims parses the [sort] group setting into the ordered list of dimensions
// the index is sorted by (most significant first).
func (c Config) SortDims() []sortDim {
	return parseSortDims(c.Sort.Group)
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

// textInputRe matches {text-input} placeholders, with an optional prompt
// label after a colon: {text-input:Initial prompt}.
var textInputRe = regexp.MustCompile(`\{text-input(?::([^}]*))?\}`)

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
	defaults := cfg.Commands
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	// Merge semantics for [commands], like the style sections: a user file
	// adding keys keeps the default bindings unless it redefines them.
	if cfg.Commands == nil {
		cfg.Commands = map[string]string{}
	}
	for k, v := range defaults {
		if _, ok := cfg.Commands[k]; !ok {
			cfg.Commands[k] = v
		}
	}
	return cfg, nil
}

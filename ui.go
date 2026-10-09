package main

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// newLineInput returns a focused single-line input with readline-style
// editing (ctrl+a/e, alt+b/f, ctrl+w/k/u, arrows).
func newLineInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.Focus()
	return ti
}

type sessionKey struct {
	source string
	id     string
}

func keyForSession(s Session) sessionKey {
	return sessionKey{source: s.Source, id: s.ID}
}

func (m model) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// observeRunning records the first refresh where each session is seen in the
// running state. Leaving running or disappearing drops the timestamp, so a
// later running interval starts over at zero.
func (m *model) observeRunning(sessions []Session) {
	if m.runningSince == nil {
		m.runningSince = map[sessionKey]time.Time{}
	}
	running := make(map[sessionKey]bool, len(sessions))
	now := m.currentTime()
	for _, s := range sessions {
		if !s.Live() || s.State != StateRunning {
			continue
		}
		key := keyForSession(s)
		running[key] = true
		if _, ok := m.runningSince[key]; !ok {
			m.runningSince[key] = now
		}
	}
	for key := range m.runningSince {
		if !running[key] {
			delete(m.runningSince, key)
		}
	}
}

func formatRunningDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	if d < time.Hour {
		minutes := d / time.Minute
		seconds := d % time.Minute / time.Second
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	hours := d / time.Hour
	if hours > 9999 {
		return "9999h+"
	}
	minutes := d % time.Hour / time.Minute
	return fmt.Sprintf("%dh %02dm", hours, minutes)
}

func (m model) stateCell(s Session) string {
	width := colState
	switch m.runningTimer.mode {
	case timerAppend:
		width = colState + 1 + colRunningDuration
	case timerReplace, timerReplaceAfter:
		width = max(colState, colRunningDuration)
	}

	label := string(s.State)
	if s.Live() && s.State == StateRunning {
		if started, ok := m.runningSince[keyForSession(s)]; ok {
			elapsed := m.currentTime().Sub(started)
			timer := formatRunningDuration(elapsed)
			switch m.runningTimer.mode {
			case timerReplace:
				label = timer
			case timerAppend:
				label += " " + timer
			case timerReplaceAfter:
				if elapsed >= m.runningTimer.after {
					label = timer
				}
			}
		}
	}
	return " " + fmt.Sprintf("%-*s", width, label)
}

const refreshEvery = 2 * time.Second

// doubleClickWithin is the window in which a second left click on the same
// row counts as a double click.
const doubleClickWithin = 400 * time.Millisecond

// Sessions with no live process and no activity for this long are dimmed.
const dimAfter = 24 * time.Hour

// Keep agent-generated subjects from overwhelming status-bar prompts.
const trashSubjectMaxWidth = 80

// colCI, colCtx and colTime are fixed-width columns. The time cell is always
// 5 cells (HH:MM today, MM-DD otherwise); the year is never shown. The dir,
// branch, model, pane, title and last columns are sized dynamically per the
// [columns] config -- see widths and computeWidths.
const (
	colCI   = 4
	colCtx  = 5
	colTime = 5
)

// colState fits every state word the index can show. colRunningDuration fits
// compact elapsed values through 9999 hours; longer runs render as "9999h+".
var colState = func() int {
	w := len(StateUnknown)
	for _, st := range sessionStates {
		w = max(w, len(st))
	}
	return w
}()

const colRunningDuration = len("9999h 59m")

// widths holds the per-column visual widths computed for the current
// session set. dir/branch/model/pane are used in every mode; titleCap caps the
// subject in preview "column" mode. A value of 0 means the column is
// hidden: the cell and its trailing gap are both skipped, and the subject
// is hidden in column mode.
type widths struct {
	dir      int
	branch   int
	model    int
	pane     int
	titleCap int // cap for the subject in preview "column" mode
}

// styles are the configured looks of each UI element.
type styles struct {
	bar         lipgloss.Style
	chipCurrent lipgloss.Style
	selected    lipgloss.Style
	dim         lipgloss.Style
	preview     lipgloss.Style
	unread      lipgloss.Style
	offline     lipgloss.Style
	index       lipgloss.Style
	time        lipgloss.Style
	timeNow     lipgloss.Style
	project     lipgloss.Style
	branch      lipgloss.Style
	model       lipgloss.Style
	subject     lipgloss.Style
	worktree    lipgloss.Style
	state       map[SessionState]lipgloss.Style
}

func newStyles(cfg Config) styles {
	return styles{
		bar:         cfg.Styles.Bar.style(),
		chipCurrent: cfg.Styles.ChipCurrent.style(),
		selected:    cfg.Styles.Selected.style(),
		dim:         cfg.Styles.Dimmed.style(),
		preview:     cfg.Styles.Preview.style(),
		unread:      cfg.Styles.Unread.style(),
		offline:     cfg.Styles.Offline.style(),
		index:       cfg.Styles.Index.style(),
		time:        cfg.Styles.Time.style(),
		timeNow:     cfg.Styles.TimeNow.style(),
		project:     cfg.Styles.Project.style(),
		branch:      cfg.Styles.Branch.style(),
		model:       cfg.Styles.Model.style(),
		subject:     cfg.Styles.Subject.style(),
		worktree:    cfg.Styles.Worktree.style(),
		state: map[SessionState]lipgloss.Style{
			StateRunning: cfg.Styles.Running.style(),
			StateWaiting: cfg.Styles.Waiting.style(),
			StateIdle:    cfg.Styles.Idle.style(),
		},
	}
}

// marker is the status a row's leading glyph conveys. It mostly mirrors the
// session state, but adds "unread" (a finished turn not yet opened) and
// "offline" (no running process).
type marker int

const (
	markerOffline marker = iota
	markerIdle
	markerRunning
	markerWaiting
	markerUnread
)

var allMarkers = []marker{markerOffline, markerIdle, markerRunning, markerWaiting, markerUnread}

// spinnerFrames animate the running marker when its glyph is "spinner". Braille
// renders in any monospace font, so it works without a Nerd Font.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerSentinel = "spinner"

// previewMode selects how a session's last message is shown.
type previewMode string

const (
	previewRow    previewMode = "row"    // a detail line beneath the session
	previewColumn previewMode = "column" // an extra column on the session row
	previewOff    previewMode = "off"    // don't show it
)

type model struct {
	loader             *multiLoader
	styles             styles
	bgExec             bool              // run key-bound commands detached (no terminal takeover)
	enterBySource      map[string]string // per-source override of the "enter" command (key = Source)
	tmuxGlyph          string            // marker for tmux-attachable sessions; "" hides it
	tmuxBar            bool              // show the Tmux Bar row ([tmux] bar)
	tmuxKey            string            // opens the Tmux Session picker; "" unbinds it
	tmuxJump           string            // [commands] tmux: the Jump template
	tmuxSrv            *tmuxServer       // last tmux poll; nil while no server is reachable
	maxIcons           int               // agent glyphs per Tmux Chip before "+N"; 0 hides them
	bgGlyph            string            // marker for detached `claude --background` sessions; "" hides it
	agentGlyphs        map[string]string // marker keyed by Session.Source
	agentStyles        map[string]lipgloss.Style
	colAgentGlyph      int    // display width reserved for the widest source glyph
	worktreeGlyph      string // marker for worktree projects; "" hides the slot entirely
	glyphs             map[marker]string
	colGlyph           int                               // display width reserved for the status glyph
	showWords          bool                              // show the state word next to the glyph
	dirIcon            string                            // glyph before the project column; "" hides it
	branchIcon         string                            // glyph before the branch column; "" hides it
	gitIcon            string                            // per-repo glyph before the branch column; "" hides it
	repoColors         []lipgloss.TerminalColor          // palette cycled per repo for the git icon
	dirNameOnly        bool                              // show just the directory name, not the full path
	modelReplacer      *strings.Replacer                 // shortens displayed model names; nil leaves them unchanged
	selColors          bool                              // keep colours on the cursor row
	selStatusColor     bool                              // keep status marker/word coloured in reverse mode
	selStatusFg        map[marker]lipgloss.TerminalColor // per-status text-colour overrides for the reversed row
	selBG              lipgloss.TerminalColor            // highlight bg for the cursor row; nil = none
	cursorHidden       bool                              // hide the cursor highlight until the next key/focus
	previewMode        previewMode                       // how to show each session's last message
	previewRecent      int                               // max recent sessions to always preview (row mode)
	previewWithin      time.Duration
	commands           map[string]string // key name -> command template
	quickTrashMax      int64             // transcript bytes allowed to use the y/n Trash prompt
	showCtx            bool              // [ctx].enabled
	ciToken            string            // "" disables the CI column
	ciSlugs            map[string]string // cwd -> CircleCI project slug ("" = none)
	colCfg             ColumnBounds      // per-column width bounds from [columns]
	widths             widths            // computed column widths for m.sessions
	ci                 map[string]ciEntry
	ciPending          map[string]time.Time // slug@branch (or cwd@branch) in flight
	sortGroup          string               // index ordering in effect; 's' cycles the presets
	configuredSortDims []sortDim            // [sort] group parsed at startup; the status part shows only a different mode
	all                []Session            // every session, unfiltered
	sessions           []Session            // what the index shows: all, limited by query/project/branch
	query              string
	project            string                  // limit the index to this project cwd; "" is no limit
	branch             string                  // limit the index to this branch; "" is no limit
	liveOnly           bool                    // limit the index to live sessions (a running agent process)
	ageWindow          time.Duration           // limit the index to sessions active within this window; 0 = all
	input              textinput.Model         // line editor backing the search and text prompts
	searching          bool                    // the search prompt is open and capturing keys
	unread             map[string]bool         // session IDs that finished a turn unseen
	seen               map[string]SessionState // last observed live state, for transitions
	runningTimer       runningTimerConfig
	runningSince       map[sessionKey]time.Time // first observation of the current running interval
	now                func() time.Time         // replaceable clock for elapsed-time tests
	spin               int                      // running-spinner frame index
	spinning           bool                     // a spinner tick is scheduled
	showHelp           bool
	helpOffset         int // scroll position within the help screen
	deleting           *trashConfirmation
	picker             pickerState
	menu               menuState
	prompt             promptState
	switchOnClick      bool      // a single left click switches, not just selects
	lastClickRow       int       // session index of the previous left click
	lastClickAt        time.Time // when the previous left click happened
	cursor             int
	offset             int
	width              int
	height             int
	loading            bool // a Load is in flight; don't start another
	status             string
	notice             string // shown instead of status until the next keypress
}

func newModel(cfg Config) model {
	mode := previewMode(cfg.Preview.Mode)
	switch mode {
	case previewRow, previewColumn, previewOff:
	default:
		mode = previewRow
	}
	glyphs := map[marker]string{
		markerRunning: cfg.Status.Running,
		markerWaiting: cfg.Status.Waiting,
		markerIdle:    cfg.Status.Idle,
		markerUnread:  cfg.Status.Unread,
		markerOffline: cfg.Status.Offline,
	}
	var selBG lipgloss.TerminalColor
	if cfg.Selection.Colors {
		if cfg.Styles.Selected.Bg != "" {
			selBG = lipgloss.Color(cfg.Styles.Selected.Bg)
		} else {
			selBG = lipgloss.Color("236") // a dim default when none is configured
		}
	}
	selStatusFg := map[marker]lipgloss.TerminalColor{}
	for mk, c := range map[marker]string{
		markerRunning: cfg.Selection.StatusColors.Running,
		markerWaiting: cfg.Selection.StatusColors.Waiting,
		markerIdle:    cfg.Selection.StatusColors.Idle,
		markerUnread:  cfg.Selection.StatusColors.Unread,
	} {
		if c != "" {
			selStatusFg[mk] = lipgloss.Color(c)
		}
	}
	var adapters []Adapter
	if cfg.Sources.Claude.Enabled {
		adapters = append(adapters, newClaudeAdapter())
	}
	if cfg.Sources.Pi.Enabled {
		adapters = append(adapters, newPiAdapter(cfg.Sources.Pi.SessionDir))
	}
	if cfg.Sources.Copilot.Enabled {
		adapters = append(adapters, newCopilotAdapter(cfg.Sources.Copilot.SessionDir))
	}
	enterBySource := map[string]string{}
	if cfg.Sources.Claude.Enter != "" {
		enterBySource["claude"] = cfg.Sources.Claude.Enter
	}
	if cfg.Sources.Pi.Enter != "" {
		enterBySource["pi"] = cfg.Sources.Pi.Enter
	}
	if cfg.Sources.Copilot.Enter != "" {
		enterBySource["copilot"] = cfg.Sources.Copilot.Enter
	}
	agentGlyphs := map[string]string{
		"claude":  cfg.Sources.Claude.Glyph,
		"pi":      cfg.Sources.Pi.Glyph,
		"copilot": cfg.Sources.Copilot.Glyph,
	}
	agentStyles := map[string]lipgloss.Style{}
	for source, color := range map[string]string{
		"claude":  cfg.Sources.Claude.Color,
		"pi":      cfg.Sources.Pi.Color,
		"copilot": cfg.Sources.Copilot.Color,
	} {
		if color != "" {
			agentStyles[source] = lipgloss.NewStyle().Foreground(lipgloss.Color(color))
		}
	}
	dims := cfg.SortDims()
	ageWindow, ageWarn := cfg.FilterWithin()
	liveOnly, liveWarn := cfg.filterLive()
	m := model{
		loader:             newMultiLoader(adapters),
		sortGroup:          cfg.Sort.Group,
		configuredSortDims: dims,
		styles:             newStyles(cfg),
		commands:           cfg.Commands,
		liveOnly:           liveOnly,
		ageWindow:          ageWindow,
		bgExec:             cfg.Background,
		enterBySource:      enterBySource,
		tmuxGlyph:          cfg.Tmux.Glyph,
		tmuxBar:            cfg.Tmux.Bar,
		tmuxKey:            cfg.Tmux.Key,
		tmuxJump:           cfg.Commands["tmux"],
		maxIcons:           max(0, cfg.Tmux.MaxIcons),
		bgGlyph:            cfg.Bg.Glyph,
		agentGlyphs:        agentGlyphs,
		agentStyles:        agentStyles,
		colAgentGlyph:      agentGlyphWidth(agentGlyphs),
		worktreeGlyph:      cfg.Worktree.Glyph,
		glyphs:             glyphs,
		colGlyph:           glyphWidth(glyphs),
		showWords:          cfg.Status.Words,
		dirIcon:            cfg.Icons.Dir,
		branchIcon:         cfg.Icons.Branch,
		gitIcon:            cfg.Git.Icon,
		repoColors:         repoPalette(cfg.Git.Colors),
		dirNameOnly:        cfg.Display.Project == "name",
		modelReplacer:      newModelReplacer(cfg.Display.ModelReplacements),
		selColors:          cfg.Selection.Colors,
		selStatusColor:     cfg.Selection.StatusColor,
		selStatusFg:        selStatusFg,
		selBG:              selBG,
		previewMode:        mode,
		previewRecent:      cfg.Preview.Recent,
		previewWithin:      cfg.PreviewWithin(),
		quickTrashMax:      cfg.QuickTrashThresholdBytes,
		showCtx:            cfg.Ctx.Enabled,
		ciToken:            cfg.ciToken(),
		ciSlugs:            cfg.ciOverrides(),
		colCfg:             cfg.Columns,
		ci:                 map[string]ciEntry{},
		ciPending:          map[string]time.Time{},
		unread:             map[string]bool{},
		seen:               map[string]SessionState{},
		runningTimer:       cfg.runningTimer(),
		runningSince:       map[sessionKey]time.Time{},
		now:                time.Now,
		switchOnClick:      cfg.Mouse.ClickAction == "select-switch",
		lastClickRow:       -1,
		loading:            true,
	}
	// Startup warnings, cleared by the next keypress.
	var warnings []string
	if ageWarn != "" {
		warnings = append(warnings, ageWarn)
	}
	if liveWarn != "" {
		warnings = append(warnings, liveWarn)
	}
	if len(warnings) > 0 {
		m.notice = strings.Join(warnings, " · ")
	}
	m.computeWidths()
	return m
}

// pickerState is the generic selection overlay: a titled list the user
// narrows by typing (fzf-style subsequence matching) and picks from with
// Enter. It is the default way to ask for a choice — openPicker sets one
// up and the onPick callback receives the chosen item.
type pickerState struct {
	active bool
	title  string              // shown in the top bar, e.g. "Select a project"
	label  func(string) string // renders an item; nil means identity
	all    []string            // every item
	items  []string            // all, narrowed by the query
	query  string              // the narrowing text; edited via model.input
	cursor int
	offset int
	onPick func(model, string) (tea.Model, tea.Cmd)
}

// openPicker asks the user to choose one of items: typing narrows the list,
// arrows and ctrl+j/k move, Enter hands the choice to onPick, Esc cancels.
// label controls how items are displayed (and matched); pass nil for the
// items themselves.
func (m *model) openPicker(title string, items []string, label func(string) string, onPick func(model, string) (tea.Model, tea.Cmd)) {
	m.picker = pickerState{active: true, title: title, label: label, all: items, items: items, onPick: onPick}
	m.input = newLineInput()
}

// labelOf is how an item appears in the list.
func (p pickerState) labelOf(item string) string {
	if p.label == nil {
		return item
	}
	return p.label(item)
}

// applyQuery narrows the items to those the query matches and resets the
// cursor to the top, like fzf.
func (p *pickerState) applyQuery() {
	p.items = p.all
	if p.query != "" {
		p.items = nil
		for _, it := range p.all {
			if fuzzyMatch(p.query, p.labelOf(it)) {
				p.items = append(p.items, it)
			}
		}
	}
	p.cursor = 0
	p.offset = 0
}

// fuzzyMatch reports whether query's runes appear in s in order, ignoring
// case — fzf's default matching. The empty query matches everything.
func fuzzyMatch(query, s string) bool {
	qr := []rune(strings.ToLower(query))
	qi := 0
	for _, r := range strings.ToLower(s) {
		if qi == len(qr) {
			break
		}
		if r == qr[qi] {
			qi++
		}
	}
	return qi == len(qr)
}

// menuState is a prefix-key menu: after its trigger key the top bar lists
// what each follow-up key does; a bound key runs its action, and Esc (or
// any unbound key) cancels. It is the generic way to group related keys
// behind one, like f for the filters.
type menuState struct {
	active  bool
	title   string
	entries []menuEntry
}

type menuEntry struct {
	key   string
	label string
	run   func(model) (tea.Model, tea.Cmd)
}

// menuView is the top-bar listing of an open prefix menu.
func (m model) menuView() string {
	parts := make([]string, 0, len(m.menu.entries))
	for _, e := range m.menu.entries {
		parts = append(parts, e.key+":"+e.label)
	}
	return m.menu.title + ": " + strings.Join(parts, "  ") + "  (Esc:Cancel)"
}

// handleMenuKey dispatches the follow-up key of an open prefix menu: a
// matched entry runs its action, Esc or any unbound key cancels.
func (m model) handleMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	for _, e := range m.menu.entries {
		if e.key == msg.String() {
			m.menu = menuState{}
			return e.run(m)
		}
	}
	m.menu = menuState{}
	return m, nil
}

// filterMenu opens the prefix menu behind f: each entry opens the picker
// for one way of narrowing the index.
func (m *model) filterMenu() {
	m.menu = menuState{active: true, title: "Filter by", entries: []menuEntry{
		{"p", "Project", func(m model) (tea.Model, tea.Cmd) {
			m.openPicker("Filter by project", m.projectList(), displayPath,
				func(m model, choice string) (tea.Model, tea.Cmd) {
					m.project = choice
					m.applyFilter()
					return m, nil
				})
			return m, nil
		}},
		{"b", "Branch", func(m model) (tea.Model, tea.Cmd) {
			m.openPicker("Filter by branch", m.branchList(), nil,
				func(m model, choice string) (tea.Model, tea.Cmd) {
					m.branch = choice
					m.applyFilter()
					return m, nil
				})
			return m, nil
		}},
	}}
}

// branchList returns every known branch, most recently used first. When the
// index is limited to a project, only that project's branches are offered.
func (m model) branchList() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range m.all { // sorted newest first
		if m.project != "" && s.CWD != m.project {
			continue
		}
		if s.Branch != "" && !seen[s.Branch] {
			seen[s.Branch] = true
			out = append(out, s.Branch)
		}
	}
	return out
}

// promptState is the one-line text prompt shown while a command containing
// {text-input} waits for its text; the typed value lives in model.input.
type promptState struct {
	active bool
	label  string
	token  string            // the exact {text-input...} placeholder being filled
	tmpl   string            // the command awaiting the text
	vars   map[string]string // expansion vars captured at keypress
}

type trashConfirmation struct {
	session Session
	typed   bool
}

// glyphWidth is the display width to reserve for the status glyph: the widest
// configured marker (the spinner counts as one frame, not the word "spinner").
func glyphWidth(glyphs map[marker]string) int {
	w := 0
	for mk, g := range glyphs {
		if mk == markerRunning && g == spinnerSentinel {
			g = spinnerFrames[0]
		}
		w = max(w, lipgloss.Width(g))
	}
	return w
}

func agentGlyphWidth(glyphs map[string]string) int {
	w := 0
	for _, glyph := range glyphs {
		w = max(w, lipgloss.Width(glyph))
	}
	return w
}

// iconOverhead returns the display width an icon prefix takes inside its
// column: the icon itself plus the one-space separator, or 0 if the icon is
// disabled. The column's configured min/max bounds are visual (total) widths,
// so this overhead is added to the observed text width before clamping.
func iconOverhead(icon string) int {
	if icon == "" {
		return 0
	}
	return lipgloss.Width(icon) + 1
}

// colWidth applies a column's bounds: cfg.Max == 0 hides the column
// (returning 0, which the renderer treats as "skip the cell and its
// trailing gap"). Otherwise the width is clamp(observed + iconOverhead,
// cfg.Min, cfg.Max). A misconfigured min > max is treated as a pin to
// cfg.Min, since the larger value is the user's intent.
func colWidth(observed, iconOH int, cfg ColumnConfig) int {
	if cfg.Max == 0 {
		return 0
	}
	w := observed + iconOH
	if w < cfg.Min {
		w = cfg.Min
	}
	if w > cfg.Max {
		w = cfg.Max
	}
	return w
}

// newModelReplacer builds a deterministic, single-pass replacer. Longer
// fragments win when keys overlap, and replacement text is not replaced again.
func newModelReplacer(replacements map[string]string) *strings.Replacer {
	keys := make([]string, 0, len(replacements))
	for old := range replacements {
		if old != "" {
			keys = append(keys, old)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, old := range keys {
		pairs = append(pairs, old, replacements[old])
	}
	return strings.NewReplacer(pairs...)
}

func (m model) displayModel(s Session) string {
	if m.modelReplacer == nil {
		return s.Model
	}
	return m.modelReplacer.Replace(s.Model)
}

// observedDirWidth returns the widest directory value across the visible
// sessions, respecting m.dirNameOnly. Empty paths contribute 0.
func (m *model) observedDirWidth() int {
	var w int
	for _, s := range m.sessions {
		v := s.Project()
		if m.dirNameOnly {
			v = s.Dir()
		}
		if wv := lipgloss.Width(v); wv > w {
			w = wv
		}
	}
	return w
}

// observedBranchWidth returns the widest non-empty branch name across the
// visible sessions. Empty branches and "HEAD" (detached) contribute 0 --
// they don't pull the column down -- but iconBody still reserves the icon
// slot on those rows, so the column keeps its visual width.
func (m *model) observedBranchWidth() int {
	var w int
	for _, s := range m.sessions {
		b := s.Branch
		if b == "" || b == "HEAD" {
			continue
		}
		if wb := lipgloss.Width(b); wb > w {
			w = wb
		}
	}
	return w
}

// observedModelWidth returns the widest known model across visible sessions.
func (m *model) observedModelWidth() int {
	var w int
	for _, s := range m.sessions {
		if wm := lipgloss.Width(m.displayModel(s)); wm > w {
			w = wm
		}
	}
	return w
}

// observedPaneWidth returns the widest tmux pane name across the visible
// sessions. An empty pane (session not in tmux) contributes 0.
func (m *model) observedPaneWidth() int {
	var w int
	for _, s := range m.sessions {
		if wp := lipgloss.Width(s.Pane); wp > w {
			w = wp
		}
	}
	return w
}

// timeCell renders a session's timestamp compactly: HH:MM for the current
// calendar day and MM-DD for every other date. The year is never shown, so
// the cell is always colTime cells wide. Comparison and formatting happen in
// the local timezone, so timestamps stored in UTC display in local time.
func timeCell(now, when time.Time) string {
	if isToday(now, when) {
		return when.Local().Format("15:04")
	}
	return when.Local().Format("01-02")
}

// isToday reports whether when falls on the same local calendar day as now.
func isToday(now, when time.Time) bool {
	now, when = now.Local(), when.Local()
	return when.Year() == now.Year() && when.YearDay() == now.YearDay()
}

// computeWidths fills m.widths for the current m.sessions. Call this after
// any change to the visible session set (initial load, poll refresh, filter
// change, window resize). It is O(visible sessions x 4 columns), so the
// per-refresh cost is negligible even with hundreds of sessions.
func (m *model) computeWidths() {
	cfg := m.colCfg
	// The branch column carries both the branch icon prefix and the per-repo
	// git icon prefix, so the overhead is the sum of the two.
	branchOH := iconOverhead(m.branchIcon) + iconOverhead(m.gitIcon)
	m.widths.dir = colWidth(m.observedDirWidth(), iconOverhead(m.dirIcon), cfg.Dir)
	m.widths.branch = colWidth(m.observedBranchWidth(), branchOH, cfg.Branch)
	m.widths.model = colWidth(m.observedModelWidth(), 0, cfg.Model)
	m.widths.pane = colWidth(m.observedPaneWidth(), 0, cfg.Pane)
	// titleCap is the upper bound on the subject in preview "column" mode.
	// The actual subject width also depends on what's left of the row
	// (shared with the last message), so it's computed in renderRow.
	m.widths.titleCap = cfg.Title.Max
}

type sessionsLoadedMsg struct {
	sessions []Session
	tmux     *tmuxServer // the Tmux Bar's poll; nil when not polled or no server answers
	err      error
}

type tickMsg struct{}

type spinnerTickMsg struct{}

type execDoneMsg struct{ err error }

// spinnerEvery paces the running-marker animation, faster than the data
// refresh so the spinner looks alive.
const spinnerEvery = 120 * time.Millisecond

func (m model) loadCmd() tea.Msg {
	sessions, err := m.loader.Load()
	// The Bar and the Jump re-query tmux on this same 2-second poll. They read
	// the server directly, never the filtered Index, so no filter can hide a
	// workspace. The poll runs even with [tmux] bar = false: that setting hides
	// the row, not the sessions the Jump reaches.
	return sessionsLoadedMsg{sessions: sessions, tmux: pollTmux(), err: err}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func spinnerCmd() tea.Cmd {
	return tea.Tick(spinnerEvery, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// anyRunning reports whether a session's turn is currently in progress, i.e.
// whether the spinner has anything to animate.
func (m model) anyRunning() bool {
	for _, s := range m.sessions {
		if s.Live() && s.State == StateRunning {
			return true
		}
	}
	return false
}

// ensureSpinner starts the spinner ticker if a session is running and one
// isn't already scheduled, returning the command to run (or nil).
func (m *model) ensureSpinner() tea.Cmd {
	if m.spinning || !m.anyRunning() {
		return nil
	}
	m.spinning = true
	return spinnerCmd()
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.loadCmd, tickCmd(), tea.SetWindowTitle("agent-sessions"))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case sessionsLoadedMsg:
		m.loading = false
		m.tmuxSrv = msg.tmux // a dead server leaves nil, which hides the Bar
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
			return m, nil
		}
		// The loader delivers sessions in adapter order; ordering is applied
		// here, on every load, so a reload keeps the runtime sort mode.
		sortSessions(msg.sessions, parseSortDims(m.sortGroup))
		m.observeRunning(msg.sessions)
		m.detectUnread(msg.sessions)
		m.readOnScreen(msg.sessions)
		m.all = msg.sessions
		m.applyFilter()
		m.computeWidths()
		m.clampOffset()
		return m, tea.Batch(m.ciFetchCmd(), m.ensureSpinner())

	case ciMsg:
		for cwd, slug := range msg.slugs {
			m.ciSlugs[cwd] = slug
		}
		for key, e := range msg.entries {
			m.ci[key] = e
			delete(m.ciPending, key)
		}

	case tickMsg:
		if m.loading {
			return m, tickCmd()
		}
		m.loading = true
		return m, tea.Batch(m.loadCmd, tickCmd())

	case spinnerTickMsg:
		if !m.anyRunning() {
			m.spinning = false
			return m, nil
		}
		m.spin++
		return m, spinnerCmd()

	case execDoneMsg:
		if msg.err != nil {
			m.notice = fmt.Sprintf("command: %s — output in %s",
				msg.err, displayPath(commandLogPath()))
		}
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.loadCmd

	case tea.FocusMsg:
		m.cursorHidden = false // regaining focus brings the cursor back

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		m.notice = ""
		m.cursorHidden = false // any key brings the cursor back
		if m.deleting != nil {
			return m.handleTrashKey(msg)
		}
		if m.showHelp {
			switch msg.String() {
			case "j", "down":
				m.helpOffset++
			case "k", "up":
				m.helpOffset = max(0, m.helpOffset-1)
			default:
				m.showHelp = false
				m.helpOffset = 0
			}
			return m, nil
		}
		if m.menu.active {
			return m.handleMenuKey(msg)
		}
		if m.picker.active {
			return m.handlePickerKey(msg)
		}
		if m.prompt.active {
			return m.handlePromptKey(msg)
		}
		if m.searching {
			cmd := m.handleSearchKey(msg)
			m.clampOffset()
			return m, cmd
		}
		if tmpl := m.commands[msg.String()]; tmpl != "" {
			return m.runCommand(tmpl)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = true
		case "d":
			if m.cursor >= len(m.sessions) {
				break
			}
			s := m.sessions[m.cursor]
			if s.Live() {
				m.notice = "Won't move a session with a running agent process to Trash."
				break
			}
			m.deleting = &trashConfirmation{
				session: s,
				typed:   s.Size > m.quickTrashMax,
			}
			if m.deleting.typed {
				m.input = newLineInput()
			}
		case "/":
			m.searching = true
			m.query = ""
			m.input = newLineInput()
			m.applyFilter()
		case "f":
			m.filterMenu()
		case "o":
			m.liveOnly = !m.liveOnly
			m.applyFilter()
		case "a":
			m.ageWindow = nextAgeWindow(m.ageWindow)
			m.applyFilter()
		case "s":
			m.setSortGroup(nextSortGroup(m.sortGroup))
		case "esc":
			// The sort mode is not a filter: Esc leaves it alone.
			if m.filterActive() {
				m.query = ""
				m.project = ""
				m.branch = ""
				m.liveOnly = false
				m.ageWindow = 0
				m.applyFilter()
			}
		case "j", "down":
			m.cursor = min(m.cursor+1, m.lastRow())
		case "k", "up":
			m.cursor = max(m.cursor-1, 0)
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			m.cursor = m.lastRow()
		case "ctrl+d", "pgdown":
			m.cursor = min(m.cursor+m.pageSize()/2, m.lastRow())
		case "ctrl+u", "pgup":
			m.cursor = max(m.cursor-m.pageSize()/2, 0)
		case "r":
			if m.loading {
				break
			}
			m.loading = true
			m.status = "Refreshing..."
			return m, m.loadCmd
		case m.tmuxKey:
			// The built-in keys win a collision, so a misconfigured [tmux] key
			// can never shadow Quit or Trash. An empty key matches nothing.
			return m.tmuxPicker()
		}
	}
	m.clampOffset()
	return m, nil
}

// piEnterBuiltin is the default resume/jump command for pi sessions. It is
// used when the user has no per-source [sources.pi]enter override configured,
// protecting older configs that predate the override from falling through to
// the Claude-oriented global enter template.
const piEnterBuiltin = `tmux select-pane -t {pane} && tmux select-window -t {pane} && tmux switch-client -t {pane} 2>/dev/null || (cd {cwd} && pi --session {id})`

// copilotEnterBuiltin is the default resume/jump command for Copilot CLI
// sessions, used when the user has no [sources.copilot]enter override. Like the
// pi built-in, it jumps to the session's tmux pane if one is known, else
// resumes the session in the current terminal (copilot --resume=<id>).
const copilotEnterBuiltin = `tmux select-pane -t {pane} && tmux select-window -t {pane} && tmux switch-client -t {pane} 2>/dev/null || (cd {cwd} && copilot --resume={id})`

// sourceEnterTemplate resolves the command template for a session's source.
// Per-source overrides win; pi and copilot sessions without one fall back to
// their source-specific built-in instead of the Claude-oriented global default.
func sourceEnterTemplate(source, global string, overrides map[string]string) string {
	if cmd, ok := overrides[source]; ok && cmd != "" {
		return cmd
	}
	switch source {
	case "pi":
		return piEnterBuiltin
	case "copilot":
		return copilotEnterBuiltin
	}
	return global
}

// handleMouse turns mouse input into selection and, per config, a switch.
// The wheel moves the cursor; a left click selects the clicked row (or the
// session its preview line belongs to), and switches to it too — running the
// enter command — when [mouse] click_action is "select-switch" or the click
// is the second of a double click. Only the page's own rows respond: the top
// bar, the Tmux Bar and the status bar are not the Index. Overlays keep their
// own keyboard driving.
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.showHelp || m.picker.active || m.prompt.active || m.searching || m.deleting != nil {
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.cursor = max(m.cursor-1, 0)
		m.clampOffset()
		return m, nil
	case tea.MouseButtonWheelDown:
		m.cursor = min(m.cursor+1, m.lastRow())
		m.clampOffset()
		return m, nil
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft || msg.Y < 1 {
		return m, nil
	}
	// A click on a chip Jumps immediately and ignores [mouse] click_action: a
	// chip has no selection state to set, so there is nothing to select first.
	if m.tmuxBarShown() && msg.Y == m.pageSize()+1 {
		c, ok := m.tmuxChipAt(msg.X)
		if !ok {
			return m, nil
		}
		return m.jumpTmux(c)
	}
	if msg.Y > m.pageSize() {
		return m, nil
	}
	// The top bar is row 0 and the status bar sits below the page, so display
	// lines occupy click Y in [1, pageSize]. Resolve the line through the
	// current layout to the session it belongs to (its row or preview line).
	rows := m.layout()
	line := m.offset + msg.Y - 1
	if line >= len(rows) {
		return m, nil
	}
	si := rows[line].si
	m.notice = ""
	m.cursorHidden = false
	now := time.Now()
	doubleClick := si == m.lastClickRow && now.Sub(m.lastClickAt) < doubleClickWithin
	m.cursor = si
	m.lastClickRow, m.lastClickAt = si, now
	m.clampOffset()
	if tmpl := m.commands["enter"]; tmpl != "" && (m.switchOnClick || doubleClick) {
		m.lastClickRow = -1 // consumed; don't let a third click re-switch
		return m.runCommand(tmpl)
	}
	return m, nil
}

// commandVars builds the placeholder values for one session's command
// template. {pane} means "the tmux pane hosting this session's running
// process": it is offered only for a live session, because a pane an adapter
// guessed from the session's cwd (pi without a live marker) may be a shell or
// a different session's pane -- a template branching on {pane?} must fall
// through to its resume path rather than jump to a dead end.
func commandVars(s Session) map[string]string {
	vars := map[string]string{
		"id":     s.ID,
		"pid":    strconv.Itoa(s.PID),
		"pid?":   "",
		"pane":   "",
		"pane?":  "",
		"cwd":    s.CWD,
		"file":   s.File,
		"state":  string(s.State),
		"jobid":  s.JobID, // set when Background; "" otherwise
		"jobid?": s.JobID,
	}
	if s.Live() {
		vars["pid?"] = strconv.Itoa(s.PID)
		// Prefer the pane the adapter already resolved; otherwise -- e.g. a
		// fresh lookup handles a pane move since Live() ran -- walk the process
		// tree to the pane hosting it. Skipped for a background job: it has no
		// pane of its own, and the walk would just resolve to whatever pane
		// happened to launch it.
		if pane := s.Pane; pane != "" {
			vars["pane"], vars["pane?"] = pane, pane
		} else if !s.Background {
			if pane, ok := tmuxPaneFor(s.PID); ok {
				vars["pane"], vars["pane?"] = pane, pane
			}
		}
	}
	return vars
}

// runCommand runs a command template for the selected session, handing it
// the terminal so interactive commands (tmux attach, editors) work.
// A template using {pid} needs a live session and shows a notice otherwise;
// {pane} expands to "" instead so a template can fall back. The optional
// forms {pane?} and {pid?} expand to "" too, so one command can branch.
func (m model) runCommand(tmpl string) (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.sessions) {
		return m, nil
	}
	s := m.sessions[m.cursor]
	tmpl = sourceEnterTemplate(s.Source, tmpl, m.enterBySource)
	vars := commandVars(s)
	// {pid} needs a real process -- a PID of 0 is meaningless to substitute -- so
	// a template using it requires a live session. {pane} is left empty when
	// there's none (rather than hard-blocked), so a template can fall back (pi:
	// jump to the pane, else resume in the current terminal) via `||`.
	if strings.Contains(tmpl, "{pid}") && !s.Live() {
		m.notice = "Session has no running agent process."
		return m, nil
	}
	if strings.Contains(tmpl, "{jobid}") && !s.Background {
		m.notice = "Session is not running as a background job."
		return m, nil
	}
	if strings.Contains(tmpl, "{ci-build-url}") {
		u := m.ciBuildURL(s)
		if u == "" {
			m.notice = "No CircleCI project known for this session."
			return m, nil
		}
		vars["ci-build-url"] = u
	}
	delete(m.unread, s.ID) // acting on a session counts as reading it
	return m.continueCommand(tmpl, vars)
}

// continueCommand resolves the next interactive placeholder in a command
// template — opening the project picker or the text prompt — and executes
// the command once none remain.
func (m model) continueCommand(tmpl string, vars map[string]string) (tea.Model, tea.Cmd) {
	if strings.Contains(tmpl, "{project-picker}") && vars["project-picker"] == "" {
		m.openPicker("Select a project", m.projectList(), displayPath,
			func(m model, choice string) (tea.Model, tea.Cmd) {
				vars["project-picker"] = choice
				return m.continueCommand(tmpl, vars)
			})
		return m, nil
	}
	if match := textInputRe.FindStringSubmatch(tmpl); match != nil {
		label := match[1]
		if label == "" {
			label = "Input"
		}
		m.prompt = promptState{active: true, label: label, token: match[0], tmpl: tmpl, vars: vars}
		m.input = newLineInput()
		return m, nil
	}
	m.cursorHidden = true // hide the highlight until the next key or focus
	return m, execCmd(tmpl, vars, m.bgExec)
}

// projectList returns every known project cwd, most recently used first.
func (m model) projectList() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range m.all { // sorted newest first
		if s.CWD != "" && !seen[s.CWD] {
			seen[s.CWD] = true
			out = append(out, s.CWD)
		}
	}
	return out
}

// handlePickerKey drives the selection overlay: movement keys navigate,
// Enter picks via onPick, Esc cancels, ctrl+c quits, and every other key
// edits the narrowing query (so the keys you press first narrow the list,
// then arrow/Enter pick from what remains).
func (m model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := &m.picker
	switch msg.String() {
	case "esc":
		m.picker = pickerState{}
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		if len(p.items) == 0 {
			m.picker = pickerState{}
			return m, nil
		}
		choice, onPick := p.items[p.cursor], p.onPick
		m.picker = pickerState{}
		return onPick(m, choice)
	case "up", "ctrl+k", "ctrl+p":
		p.cursor = max(p.cursor-1, 0)
	case "down", "ctrl+j", "ctrl+n":
		p.cursor = min(p.cursor+1, max(0, len(p.items)-1))
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if v := m.input.Value(); v != p.query {
			p.query = v
			p.applyQuery()
		}
		return m, cmd
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+m.pageSize() {
		p.offset = p.cursor - m.pageSize() + 1
	}
	return m, nil
}

// handlePromptKey edits the pending {text-input} value. Enter substitutes
// it (shell-quoted) and continues resolving the command; Esc cancels.
func (m model) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		p := m.prompt
		tmpl := strings.ReplaceAll(p.tmpl, p.token, shellQuote(m.input.Value()))
		m.prompt = promptState{}
		return m.continueCommand(tmpl, p.vars)
	case "esc":
		m.prompt = promptState{}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) handleTrashKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	confirmation := *m.deleting
	if !confirmation.typed {
		m.deleting = nil
		if msg.String() != "y" {
			return m, nil
		}
		return m.trashSession(confirmation.session)
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.deleting = nil
		return m, nil
	case "enter":
		m.deleting = nil
		if m.input.Value() != "yes" {
			m.notice = `Trash cancelled: confirmation must be exactly "yes".`
			return m, nil
		}
		return m.trashSession(confirmation.session)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) trashSession(s Session) (tea.Model, tea.Cmd) {
	if err := m.loader.Trash(s); err != nil {
		m.notice = "trash: " + err.Error()
		return m, nil
	}
	const prefix = "Moved "
	const suffix = " to Trash."
	m.notice = prefix + m.trashSubject(s.Subject(), lipgloss.Width(prefix+suffix)) + suffix
	if !m.loading {
		m.loading = true
		return m, m.loadCmd
	}
	return m, nil
}

// detectUnread flags sessions that just finished a turn (running -> idle) so
// they stand out from long-idle ones, and clears the flag when a session
// starts a new turn or disappears. The flag is otherwise cleared only by
// opening the session. State comparison is against the previous refresh.
func (m *model) detectUnread(sessions []Session) {
	present := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		present[s.ID] = true
		prev, seen := m.seen[s.ID]
		switch {
		case !s.Live():
			delete(m.seen, s.ID)
		case s.State == StateRunning:
			delete(m.unread, s.ID) // a fresh turn supersedes an old completion
			m.seen[s.ID] = s.State
		default:
			if seen && prev == StateRunning && s.State == StateIdle {
				m.unread[s.ID] = true
			}
			m.seen[s.ID] = s.State
		}
	}
	for id := range m.seen {
		if !present[id] {
			delete(m.seen, id)
		}
	}
	for id := range m.unread {
		if !present[id] {
			delete(m.unread, id)
		}
	}
}

// readOnScreen clears Unread for every session whose Pane an attached tmux
// client has on screen: read means seen (docs/adr/0001-read-means-seen.md). It
// runs after detectUnread, so a turn that finishes while you are looking at its
// Pane never shows as unread at all. With no reachable server there is no
// on-screen set, and reading falls back to acting on the session through the
// TUI.
func (m *model) readOnScreen(sessions []Session) {
	if m.tmuxSrv == nil {
		return
	}
	for _, s := range sessions {
		if s.Pane != "" && m.tmuxSrv.onScreen[s.Pane] {
			delete(m.unread, s.ID)
		}
	}
}

// handleSearchKey edits the query while the search prompt is open. The list
// filters as the query changes; Enter keeps the filter, Esc clears it.
func (m *model) handleSearchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		return tea.Quit
	case "enter":
		m.searching = false
		return nil
	case "esc":
		m.searching = false
		m.query = ""
		m.applyFilter()
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if v := m.input.Value(); v != m.query {
		m.query = v
		m.applyFilter()
	}
	return cmd
}

// filterActive reports whether any filter is narrowing the index: Esc has
// something to clear, and the top bar advertises the Esc binding only then.
func (m model) filterActive() bool {
	return m.query != "" || m.project != "" || m.branch != "" || m.liveOnly || m.ageWindow != 0
}

// applyFilter rebuilds the visible list from the full one, keeps the cursor
// on the same session where possible, and refreshes the status counts.
func (m *model) applyFilter() {
	var selectedID string
	if m.cursor < len(m.sessions) {
		selectedID = m.sessions[m.cursor].ID
	}
	m.sessions = m.all
	if m.filterActive() {
		q := strings.ToLower(m.query)
		var ageCutoff time.Time
		if m.ageWindow > 0 {
			ageCutoff = m.currentTime().Add(-m.ageWindow)
		}
		m.sessions = nil
		for _, s := range m.all {
			if m.liveOnly && !s.Live() {
				continue
			}
			if m.project != "" && s.CWD != m.project {
				continue
			}
			if m.branch != "" && s.Branch != m.branch {
				continue
			}
			// The window never hides a live session (a long-running one may
			// have no recent entry) or one of unknown age (When is zero).
			if m.ageWindow > 0 && !s.Live() {
				if w := s.When(); !w.IsZero() && w.Before(ageCutoff) {
					continue
				}
			}
			displayModel := strings.ToLower(m.displayModel(s))
			if q != "" && !s.matches(q) && !strings.Contains(displayModel, q) {
				continue
			}
			m.sessions = append(m.sessions, s)
		}
	}
	m.cursor = min(m.cursor, m.lastRow())
	m.selectSession(selectedID)
	counts := map[SessionState]int{}
	for _, s := range m.sessions {
		if s.Live() {
			counts[s.State]++
		}
	}
	noun := "sessions"
	if len(m.sessions) == 1 {
		noun = "session"
	}
	parts := []string{fmt.Sprintf("%d %s", len(m.sessions), noun)}
	for _, st := range sessionStates {
		parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
	}
	if m.query != "" {
		parts = append(parts, fmt.Sprintf("filter %q", m.query))
	}
	if m.project != "" {
		parts = append(parts, "project "+displayPath(m.project))
	}
	if m.branch != "" {
		parts = append(parts, "branch "+m.branch)
	}
	if m.liveOnly {
		parts = append(parts, "live only")
	}
	if label := ageWindowLabel(m.ageWindow); label != "" {
		parts = append(parts, label)
	}
	// The sort mode is only worth showing once it differs from the configured
	// order; the default status bar then stays as it was.
	if m.sortGroup != "" && !sameSortDims(parseSortDims(m.sortGroup), m.configuredSortDims) {
		parts = append(parts, "sorted by "+m.sortGroup)
	}
	m.status = strings.Join(parts, ", ")
	m.computeWidths()
}

// selectSession moves the cursor onto the session with the given id, leaving
// the position alone when the session is not visible.
func (m *model) selectSession(id string) {
	if id == "" {
		return
	}
	for i, s := range m.sessions {
		if s.ID == id {
			m.cursor = i
			return
		}
	}
}

// setSortGroup switches the index ordering and re-sorts the loaded list in
// place -- sorting only needs fields Load already fills, so no reload is
// needed. Every later load is ordered by the new mode too, and the cursor
// stays on the session it was on.
func (m *model) setSortGroup(group string) {
	var selectedID string
	if m.cursor < len(m.sessions) {
		selectedID = m.sessions[m.cursor].ID
	}
	m.sortGroup = group
	sortSessions(m.all, parseSortDims(group))
	m.applyFilter()
	m.selectSession(selectedID)
}

// ciFetchCmd starts a background fetch of CI statuses for visible rows
// that are missing or stale, or returns nil when there is nothing to do.
func (m *model) ciFetchCmd() tea.Cmd {
	if m.ciToken == "" {
		return nil
	}
	now := time.Now()
	var targets []ciTarget
	for i := m.offset; i < min(m.offset+m.pageSize(), len(m.sessions)); i++ {
		s := m.sessions[i]
		if s.CWD == "" || s.Branch == "" || s.Branch == "HEAD" {
			continue
		}
		slug, known := m.ciSlugs[s.CWD]
		if known && slug == "" {
			continue // this directory has no CircleCI project
		}
		key := slug + "@" + s.Branch
		if slug == "" {
			key = s.CWD + "@" + s.Branch // slug not derived yet
		} else if e, ok := m.ci[key]; ok && now.Sub(e.At) < ciTTL {
			continue
		}
		if t, ok := m.ciPending[key]; ok && now.Sub(t) < ciTTL {
			continue
		}
		m.ciPending[key] = now
		targets = append(targets, ciTarget{CWD: s.CWD, Branch: s.Branch, Slug: slug})
	}
	if len(targets) == 0 {
		return nil
	}
	return fetchCICmd(m.ciToken, targets)
}

// ciStatus returns the CI column value for a session, or "" when unknown.
func (m model) ciStatus(s Session) string {
	slug := m.ciSlugs[s.CWD]
	if slug == "" || s.Branch == "" {
		return ""
	}
	return m.ci[slug+"@"+s.Branch].Status
}

// inputView renders the line editor sized to the space left of its label,
// scrolling horizontally when the value outgrows it.
func (m model) inputView(label string) string {
	in := m.input
	in.Width = max(8, m.width-lipgloss.Width(label)-2)
	return in.View()
}

// lastRow is the highest valid cursor position.
func (m model) lastRow() int {
	return max(0, len(m.sessions)-1)
}

// pageSize is the number of index rows visible between the two bars. The Tmux
// Bar, while shown, takes a row of its own: height-3, back to height-2 without
// it, so non-tmux users lose no rows.
func (m *model) pageSize() int {
	bars := 2
	if m.tmuxBarShown() {
		bars++
	}
	return max(1, m.height-bars)
}

// dispRow is one rendered line: a session's main row, or its preview detail.
type dispRow struct {
	si     int
	detail bool
}

// layout expands the session list into display lines, inserting a preview
// detail line after each session that should show one.
func (m model) layout() []dispRow {
	rows := make([]dispRow, 0, len(m.sessions))
	for i := range m.sessions {
		rows = append(rows, dispRow{si: i})
		if m.showPreviewRow(i) {
			rows = append(rows, dispRow{si: i, detail: true})
		}
	}
	return rows
}

// showPreviewRow reports whether session i gets a preview detail line. Only
// "row" mode uses detail lines; the selected session always shows one, and
// the most recently active sessions show one so recent answers stay visible.
func (m model) showPreviewRow(i int) bool {
	if m.previewMode != previewRow || m.sessions[i].LastMsg == "" {
		return false
	}
	if i == m.cursor {
		return true
	}
	return i < m.previewRecent && time.Since(m.sessions[i].Activity) <= m.previewWithin
}

// cursorLine is the display-line index of the selected session's main row.
func (m model) cursorLine(rows []dispRow) int {
	for i, r := range rows {
		if r.si == m.cursor && !r.detail {
			return i
		}
	}
	return 0
}

func (m *model) clampOffset() {
	rows := m.layout()
	cl := m.cursorLine(rows)
	page := m.pageSize()
	if cl < m.offset {
		m.offset = cl
	}
	// Keep the cursor's own preview line on screen too, when it has one.
	end := cl
	if cl+1 < len(rows) && rows[cl+1].detail && rows[cl+1].si == m.cursor {
		end = cl + 1
	}
	if end >= m.offset+page {
		m.offset = end - page + 1
	}
	if maxOff := max(0, len(rows)-page); m.offset > maxOff {
		m.offset = maxOff
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	if m.showHelp {
		return m.helpView()
	}
	if m.picker.active {
		return m.pickerView()
	}

	help := "q:Quit  j/k:Move  Enter:Go  /:Search  f:Filter  o:Running  r:Refresh  ?:Help"
	if m.filterActive() {
		help = "q:Quit  j/k:Move  Enter:Go  /:Search  f:Filter  o:Running  Esc:Clear filter  r:Refresh  ?:Help"
	}
	if m.menu.active {
		help = m.menuView()
	}
	var b strings.Builder
	b.WriteString(m.styles.bar.Render(pad(help, m.width)))
	b.WriteString("\n")

	rows := m.layout()
	page := m.pageSize()
	for i := 0; i < page; i++ {
		idx := m.offset + i
		if idx < len(rows) {
			r := rows[idx]
			s := m.sessions[r.si]
			var line string
			switch {
			case r.detail:
				line = m.styles.preview.Render(m.previewLine(s))
			case r.si == m.cursor && !m.cursorHidden && m.selColors:
				line = m.renderRow(r.si, true, lipgloss.NewStyle().Background(m.selBG), true)
			case r.si == m.cursor && !m.cursorHidden:
				line = m.renderRow(r.si, false, m.styles.selected, true)
			case !s.Live() && time.Since(s.Activity) > dimAfter:
				line = m.renderRow(r.si, false, m.styles.dim, false)
			default:
				line = m.renderRow(r.si, true, lipgloss.NewStyle(), false)
			}
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	pos := "all"
	if len(m.sessions) > page {
		pos = fmt.Sprintf("%d%%", (m.cursor+1)*100/len(m.sessions))
	}
	status := fmt.Sprintf("---Sessions: %s---(%s)", m.status, pos)
	if m.notice != "" {
		status = m.notice
	}
	if m.searching {
		status = "Search: " + m.inputView("Search: ")
	}
	if m.prompt.active {
		status = m.prompt.label + ": " + m.inputView(m.prompt.label+": ")
	}
	if m.deleting != nil {
		s := m.deleting.session
		if m.deleting.typed {
			suffix := fmt.Sprintf(
				" is %s, over the %s quick limit. Type yes to move to Trash: ",
				displaySize(s.Size), displaySize(m.quickTrashMax),
			)
			label := m.trashSubject(s.Subject(), lipgloss.Width(suffix)+8) + suffix
			status = label + m.inputView(label)
		} else {
			const prefix = "Move "
			suffix := fmt.Sprintf(" [%s] to Trash? (y/n)", displaySize(s.Size))
			status = prefix + m.trashSubject(
				s.Subject(),
				lipgloss.Width(prefix+suffix),
			) + suffix
		}
	}
	if m.tmuxBarShown() {
		b.WriteString(m.tmuxBarView())
		b.WriteString("\n")
	}
	b.WriteString(m.styles.bar.Render(pad(status, m.width)))
	return b.String()
}

// tmuxChipAgent is one live agent on a Tmux Chip: the Agent Source mark, its
// Attention, and what a Jump needs — the agent session and its Pane.
type tmuxChipAgent struct {
	Source  string
	Waiting bool
	Unread  bool
	ID      string // the agent session's id, to read it on a Jump
	Pane    string // the pane's %N id, the Jump's target
}

// tmuxChip is one Tmux Session in the Tmux Bar.
type tmuxChip struct {
	ID      string // $N, the Jump's session target
	Name    string
	Windows int
	Current bool
	Agents  []tmuxChipAgent
}

// chipSep separates one chip from the next, chipPad is the padding around a
// chip's name, and chipNameFloor is the narrowest a chip name shrinks to before
// the row's tail is cut instead: a shorter name says nothing about which session
// it is.
const (
	chipSep       = "│"
	chipPad       = " "
	chipNameFloor = 4
)

// tmuxBarShown reports whether the Tmux Bar occupies a row on this screen. Help
// and the picker render their own two-bar layout, and the row is absent while no
// tmux server is reachable: no error, no empty line.
func (m model) tmuxBarShown() bool {
	return m.tmuxBar && m.tmuxSrv != nil && len(m.tmuxSrv.sessions) > 0 &&
		!m.showHelp && !m.picker.active
}

// tmuxChips maps the last tmux poll onto the Bar's chips: one per Tmux Session,
// in tmux's own order so a click target never moves, each holding the live
// agents attributed to it.
func (m model) tmuxChips() []tmuxChip {
	if m.tmuxSrv == nil {
		return nil
	}
	chips := make([]tmuxChip, 0, len(m.tmuxSrv.sessions))
	index := make(map[string]int, len(m.tmuxSrv.sessions))
	for _, s := range m.tmuxSrv.sessions {
		index[s.name] = len(chips)
		chips = append(chips, tmuxChip{
			ID:      s.id,
			Name:    s.name,
			Windows: s.windows,
			Current: s.id != "" && s.id == m.tmuxSrv.current,
		})
	}
	// Agents come from the unfiltered list, so the liveness filter cannot empty a
	// chip, and only from Live() sessions: a Pane can be set on a dead session by
	// an adapter's cwd heuristic, and those must not appear. Their order is the
	// Index order, so a chip's marks never react to state.
	for _, s := range m.all {
		if !s.Live() || s.Pane == "" {
			continue
		}
		name, ok := m.tmuxSrv.byPane[s.Pane]
		if !ok {
			continue // a pane on another server, or one that has since closed
		}
		i, ok := index[name.session]
		if !ok {
			continue
		}
		chips[i].Agents = append(chips[i].Agents, tmuxChipAgent{
			Source:  s.Source,
			Waiting: s.State == StateWaiting,
			Unread:  m.unread[s.ID] && s.State == StateIdle,
			ID:      s.ID,
			Pane:    name.id,
		})
	}
	return chips
}

// tmuxBarView renders the Tmux Bar: one chip per Tmux Session in tmux's own
// order, across the full width. Every piece is styled on its own -- a single
// wrapper around the row would let one cell's reset drop the strip's style for
// the rest of it.
func (m model) tmuxBarView() string {
	chips := m.tmuxChips()
	if len(chips) == 0 {
		return ""
	}
	row := m.renderTmuxBar(chips, m.tmuxBarNameCap(chips))
	row = trunc(row, m.width)
	// The slack is the same plain strip the chips sit on, so the row reads as one
	// bar to the end rather than a band of styled chips over a bare tail.
	if gap := m.width - lipgloss.Width(row); gap > 0 {
		row += m.chipStrip(strings.Repeat(" ", gap))
	}
	return row
}

// chipStrip renders the row's own cells: the separators between chips and the
// slack after the last one. It is [styles.bar] with its reverse lifted, which
// leaves the strip on the terminal's own background: the chips are then plain
// text separated by a rule, as a status bar's tabs are, instead of a row of
// blocks each carrying its own video.
func (m model) chipStrip(text string) string {
	return m.chipStripStyle().Render(text)
}

// chipStripStyle is the style chipStrip draws in, and the base every chip's own
// style is built on top of.
func (m model) chipStripStyle() lipgloss.Style {
	return m.styles.bar.Reverse(false)
}

// chipStyle is one chip's style: the strip's own, with [styles.chip_current]
// layered on for the session the TUI itself runs in. Since the strip keeps its
// reverse lifted, a colour set in [styles.chip_current] is literal -- `bg` is
// the chip's background and `fg` its text, with nothing swapped underneath.
func (m model) chipStyle(c tmuxChip) lipgloss.Style {
	base := m.chipStripStyle()
	if c.Current {
		base = overlay(base, m.styles.chipCurrent)
	}
	return base
}

// renderTmuxBar renders every chip at one name width, separated by the strip's
// rule so the tab boundaries never depend on colour.
func (m model) renderTmuxBar(chips []tmuxChip, nameCap int) string {
	parts := make([]string, len(chips))
	for i, c := range chips {
		parts[i] = m.renderTmuxChip(c, nameCap)
	}
	return strings.Join(parts, m.chipStrip(chipSep))
}

// renderTmuxChip renders one chip: padding around the session name, its Window
// count when greater than one, then one mark per live agent, capped at [tmux]
// max_icons with a "+N" overflow. Nothing here is conditional on the chip being
// current, so marking a chip never moves the ones after it: the mark is
// [styles.chip_current], and every chip carries the same cells either way.
func (m model) renderTmuxChip(c tmuxChip, nameCap int) string {
	base := m.chipStyle(c)
	cell := func(st lipgloss.Style, text string) string {
		return overlay(base, st).Render(text)
	}
	var b strings.Builder
	b.WriteString(cell(lipgloss.NewStyle(), trunc(c.Name, nameCap)))
	if c.Windows > 1 {
		b.WriteString(cell(lipgloss.NewStyle(), fmt.Sprintf("(%d)", c.Windows)))
	}
	if m.maxIcons > 0 && m.colAgentGlyph > 0 {
		shown := c.Agents
		if len(shown) > m.maxIcons {
			shown = shown[:m.maxIcons]
		}
		for _, a := range shown {
			b.WriteString(cell(lipgloss.NewStyle(), " "))
			b.WriteString(m.tmuxGlyphCell(a, base))
		}
		if n := len(c.Agents) - len(shown); n > 0 {
			b.WriteString(cell(lipgloss.NewStyle(), fmt.Sprintf(" +%d", n)))
		}
	}
	// The padding keeps the tab off its own rule, and it is drawn in the chip's
	// style so the current chip's background covers its whole cell.
	return base.Render(chipPad) + b.String() + base.Render(chipPad)
}

// tmuxGlyphCell renders one agent's mark: the Agent Source, decorated by
// Attention and never replaced by it. waiting inverts the style it is drawn in,
// so it reads as reverse video against that cell instead of vanishing into it;
// Unread carries the attention colour; running and idle stay plain.
func (m model) tmuxGlyphCell(a tmuxChipAgent, base lipgloss.Style) string {
	mark := pad(m.agentGlyphs[a.Source], m.colAgentGlyph)
	switch {
	case a.Waiting:
		bold := m.styles.state[StateWaiting].GetBold() || base.GetBold()
		return base.Bold(bold).Reverse(!base.GetReverse()).Render(mark)
	case a.Unread:
		fg := m.styles.unread.GetForeground()
		if isNoColor(fg) {
			return base.Render(mark)
		}
		bold := m.styles.unread.GetBold() || base.GetBold()
		// On a reverse row, the colour goes on the channel the terminal swaps, so
		// the mark stays coloured text on the row's own background (the rule the
		// reversed cursor row's status marker follows).
		if base.GetReverse() {
			return lipgloss.NewStyle().Background(fg).Reverse(true).Bold(bold).Render(mark)
		}
		return base.Foreground(fg).Bold(bold).Render(mark)
	}
	return base.Render(mark)
}

// tmuxBarNameCap is the width every chip name shrinks to: the widest name,
// reduced while the row overflows and never below the floor. The hit test and
// the renderer both use it, so a click lands on the chip it looks like.
func (m model) tmuxBarNameCap(chips []tmuxChip) int {
	cap := 0
	for _, c := range chips {
		cap = max(cap, lipgloss.Width(c.Name))
	}
	for cap > chipNameFloor && lipgloss.Width(m.renderTmuxBar(chips, cap)) > m.width {
		cap--
	}
	return cap
}

// tmuxChipAt returns the chip under column x on the Tmux Bar's row, laid out
// exactly as the row was drawn. false when x falls on a separator, or past the
// row's cut.
func (m model) tmuxChipAt(x int) (tmuxChip, bool) {
	if x >= m.width {
		return tmuxChip{}, false // past the row's cut
	}
	chips := m.tmuxChips()
	nameCap := m.tmuxBarNameCap(chips)
	start := 0
	for _, c := range chips {
		w := lipgloss.Width(m.renderTmuxChip(c, nameCap))
		if x >= start && x < start+w {
			return c, true
		}
		start += w + lipgloss.Width(chipSep)
	}
	return tmuxChip{}, false
}

// tmuxJumpTarget returns the agent a Jump diverts to, if any: the first waiting
// agent, else the first Unread one, because waiting is blocked on the user now.
// No such agent means the Jump leaves tmux on the session's last-active Window.
func tmuxJumpTarget(c tmuxChip) (tmuxChipAgent, bool) {
	for _, a := range c.Agents {
		if a.Waiting && a.Pane != "" {
			return a, true
		}
	}
	for _, a := range c.Agents {
		if a.Unread && a.Pane != "" {
			return a, true
		}
	}
	return tmuxChipAgent{}, false
}

// tmuxJumpVars are the Jump template's placeholders: the Tmux Session to switch
// to, and the Pane to select first (empty when the Jump does not divert).
//
// The session is passed as its id ($N), not its name: tmux reads a name
// containing ':' as session:window, so `-t x:y` fails with "can't find session:
// x", while an id addresses every session (x:y is a real name tmux allows, and
// the fixtures keep one).
func tmuxJumpVars(session, pane string) map[string]string {
	return map[string]string{
		"tmux-session": session,
		"tmux-target":  pane,
		"tmux-target?": pane,
	}
}

// jumpTmux moves the terminal to a Tmux Session: the Pane of an agent that
// wants the user when there is one, otherwise tmux's own last-active Window.
// It is a [commands] tmux template, so it stays configurable, and it reads the
// agent session it lands on — and only that one.
//
// Attention here is the Unread flag: waiting is the agent's own live state,
// which only the agent can leave. The flag clears when the Jump is issued, as
// Enter on a session does, and only for the agent the Jump diverts to: a chip
// with no Attention agent lands on a Window whose agents are already read.
func (m model) jumpTmux(c tmuxChip) (tea.Model, tea.Cmd) {
	if m.tmuxJump == "" {
		return m, nil // [commands] tmux = "" unbinds the Jump, click included
	}
	target, diverts := tmuxJumpTarget(c)
	if diverts {
		delete(m.unread, target.ID)
	}
	m.notice = ""
	m.cursorHidden = true // hide the highlight until the next key or focus
	return m, execCmd(m.tmuxJump, tmuxJumpVars(c.ID, target.Pane), m.bgExec)
}

// tmuxPicker asks which Tmux Session to Jump to, over the same sessions the Bar
// shows. Attention comes first: the picker is transient, so it can afford the
// relevance-first order the Bar cannot, where chips must never move.
func (m model) tmuxPicker() (tea.Model, tea.Cmd) {
	chips := tmuxAttentionFirst(m.tmuxChips())
	if len(chips) == 0 {
		m.notice = "No tmux sessions."
		return m, nil
	}
	items := make([]string, len(chips))
	byName := make(map[string]tmuxChip, len(chips))
	for i, c := range chips {
		items[i] = c.Name
		byName[c.Name] = c
	}
	label := func(name string) string {
		c := byName[name]
		return ansi.Strip(m.renderTmuxChip(c, lipgloss.Width(c.Name)))
	}
	m.openPicker("Jump to a tmux session", items, label,
		func(m model, name string) (tea.Model, tea.Cmd) {
			return m.jumpTmux(byName[name])
		})
	return m, nil
}

// tmuxAttentionFirst orders chips for the picker: a waiting agent ranks above
// an Unread one, and tmux's own order holds within a rank.
func tmuxAttentionFirst(chips []tmuxChip) []tmuxChip {
	rank := func(c tmuxChip) int {
		r := 0
		for _, a := range c.Agents {
			if a.Waiting {
				return 2
			}
			if a.Unread {
				r = 1
			}
		}
		return r
	}
	out := append([]tmuxChip(nil), chips...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) > rank(out[j]) })
	return out
}

// pickerView renders the selection overlay: the narrowed item list, with
// the query editor and match count in the bottom bar.
func (m model) pickerView() string {
	p := m.picker
	var b strings.Builder
	b.WriteString(m.styles.bar.Render(pad(p.title, m.width)))
	b.WriteString("\n")
	page := m.pageSize()
	for i := 0; i < page; i++ {
		idx := p.offset + i
		if idx < len(p.items) {
			line := trunc(fmt.Sprintf("%4d  %s", idx+1, p.labelOf(p.items[idx])), m.width)
			if idx == p.cursor {
				line = m.styles.selected.Render(pad(line, m.width))
			}
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	status := fmt.Sprintf("---%s: %s---(%d/%d, Enter:Pick Esc:Cancel)",
		p.title, m.inputView(p.title+": "), len(p.items), len(p.all))
	b.WriteString(m.styles.bar.Render(pad(status, m.width)))
	return b.String()
}

// helpView lists the built-in keys and every configured command.
func (m model) helpView() string {
	mouseHelp := "    mouse              click a row to select, double-click to switch; a chip jumps; wheel scrolls"
	if m.switchOnClick {
		mouseHelp = "    mouse              click a row to select and switch; a chip jumps; wheel scrolls"
	}
	lines := []string{
		"",
		"  Built-in keys",
		"    j / k, arrows      move down / up",
		"    ctrl+d / ctrl+u    half page down / up",
		"    g / G              first / last session",
		"    /                  search; Enter keeps the filter, Esc clears it",
		"    f                  filter menu: p by project, b by branch (pickers)",
		"    o                  toggle live only: sessions with a running agent process",
		"    a                  cycle the age window: all, 7 days, 30 days",
		"    s                  cycle the sort order: activity, repo, active,repo",
	}
	if m.tmuxKey != "" {
		lines = append(lines, fmt.Sprintf(
			"    %-18s jump to a tmux session (picker, Attention first)", m.tmuxKey))
	}
	// The Jump template is not a key binding -- a chip click runs it too -- so it
	// is named here instead of in the [commands] key list below.
	if m.tmuxJump != "" {
		lines = append(lines, fmt.Sprintf(
			"    %-18s the Jump template a chip click or the key runs", "[commands] tmux"))
	}
	lines = append(lines,
		"    d                  move session to Trash (large sessions: type yes)",
		mouseHelp,
		"    r                  refresh now",
		"    ?                  this help",
		"    q                  quit",
		"",
		"  Picker (selecting from a list, e.g. projects)",
		"    type               narrow the list (fzf-style subsequence match)",
		"    arrows, ctrl+j/k   move",
		"    Enter              pick the highlighted item",
		"    Esc                cancel",
		"",
		"  Prefix menus (e.g. f)",
		"    follow-up key      run the action shown in the top bar",
		"    Esc or other key   cancel the menu",
		"",
		"  Columns (see [columns] in your config)",
		"    The content columns (dir, branch, pane, title) size to the longest",
		"    visible value, clamped to a per-column min/max. Set max = 0 to hide",
		"    a column; set min = max to pin it to a fixed width.",
		"",
	)
	if m.ciToken != "" {
		lines = append(lines,
			"  CI column: the branch's latest CircleCI pipeline, workflows combined",
			"  (a workflow further up the list wins over everything below it)",
			"    fail     a workflow failed or errored",
			"    run      a workflow is still running",
			"    hold     a workflow is waiting for manual approval",
			"    cxl      a workflow was cancelled",
			"    pass     all workflows succeeded",
			"    -        the project is on CircleCI, but the branch has no pipelines",
			"    blank    no CircleCI project, fetch failed, or not fetched yet",
			"    Fetched in the background for visible rows only, cached for 30s; the",
			"    project slug comes from the git origin remote or [circleci.projects].",
			"",
		)
	}
	lines = append(lines,
		"  Command placeholders (values expand shell-quoted in [commands])",
		"    {id}                the session id (as used by claude --resume)",
		"    {cwd}               the session's working directory",
		"    {file}              the session's transcript (.jsonl) path",
		"    {state}             running/waiting/idle for live sessions, else empty",
		"    {pid}               pid of the running claude process (live only)",
		"    {pane}              tmux pane hosting the process (live, in tmux)",
		"    {jobid}             id for `claude attach`/`stop` (live, --background only)",
		"    {tmux-session}      tmux session id ($N) a chip's Jump switches to",
		"    {tmux-target} / {tmux-target?}   pane the Jump selects first; empty when none wants you",
		"    {pid?} / {pane?} / {jobid?}   optional forms: expand empty instead of blocking",
		"    {ci-build-url}      the latest CircleCI build's page (needs [circleci])",
		"    {project-picker}    asks: pick a project from every known one",
		"    {text-input:Label}  asks: a line of text (the label is optional)",
		"",
	)
	lines = append(lines, "  Commands (from config)")
	keys := make([]string, 0, len(m.commands))
	for k, tmpl := range m.commands {
		if tmpl == "" || k == "tmux" {
			continue // "" unbinds the binding; tmux is the Jump template, named above
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		oneLine := strings.Join(strings.Fields(m.commands[k]), " ")
		lines = append(lines, fmt.Sprintf("    %-18s %s", k, oneLine))
	}

	var b strings.Builder
	b.WriteString(m.styles.bar.Render(pad("Help", m.width)))
	b.WriteString("\n")
	page := m.pageSize()
	offset := min(m.helpOffset, max(0, len(lines)-page))
	for i := 0; i < page; i++ {
		if idx := offset + i; idx < len(lines) {
			b.WriteString(trunc(lines[idx], m.width))
		}
		b.WriteString("\n")
	}
	b.WriteString(m.styles.bar.Render(pad("---Help---(j/k scroll, any other key returns)", m.width)))
	return b.String()
}

// renderRow builds one session line. When colored, each cell keeps its own
// colour; otherwise it keeps only its bold, so emphasis (a running session)
// survives even in the uncoloured reverse/dim rows. sel is the row overlay —
// the reverse video, background highlight, or faint applied to the cursor and
// stale rows — layered onto every cell, separator and the trailing pad so it
// covers the whole row. fill pads the row to full width (for the bar-like
// reverse and highlight rows). Bold and reverse are independent attributes, so
// bold text stays bold under reverse video.
func (m model) renderRow(idx int, colored bool, sel lipgloss.Style, fill bool) string {
	s := m.sessions[idx]
	seg := func(st lipgloss.Style, txt string) string {
		r := lipgloss.NewStyle().Bold(st.GetBold())
		if colored {
			r = st
		}
		r = overlay(r, sel)
		return r.Render(txt)
	}
	// statusSeg is seg for the marker and state word. In a reverse row with
	// statuscolor on, it shows the status as coloured *text* on the bar's own
	// background (rather than a coloured block, or a default-background hole).
	// Reverse video has no nameable background colour, so we put the status
	// colour on the background channel and let the terminal's reverse swap turn
	// it into the foreground — leaving the background as the same default-derived
	// colour the rest of the reversed row uses.
	statusSeg := func(st lipgloss.Style, mk marker, txt string) string {
		if m.selStatusColor && sel.GetReverse() && !isNoColor(st.GetForeground()) {
			fg := st.GetForeground()
			if o, ok := m.selStatusFg[mk]; ok {
				fg = o // colour picked for the reversed bar
			}
			return lipgloss.NewStyle().
				Background(fg).
				Reverse(true).
				Bold(st.GetBold()).
				Render(txt)
		}
		return seg(st, txt)
	}
	gap := func(n int) string { return seg(lipgloss.NewStyle(), strings.Repeat(" ", n)) }

	mk := m.markerFor(s)
	var b strings.Builder
	// Prefix: index, status group (status + state word + tmux and source
	// markers), and time. All fixed-width; the time cell has no trailing gap
	// -- the first emit() call below adds it.
	b.WriteString(seg(m.styles.index, fmt.Sprintf("%4d", idx+1)))
	b.WriteString(gap(1))
	b.WriteString(statusSeg(m.styleFor(mk), mk, m.statusCell(mk)))
	if m.showWords {
		st := lipgloss.NewStyle()
		if ws, ok := m.styles.state[s.State]; ok && s.Live() {
			st = ws
		}
		b.WriteString(statusSeg(st, wordMarker(s.State), m.stateCell(s)))
	}
	b.WriteString(seg(m.agentStyles[s.Source], m.agentCell(s)))
	b.WriteString(seg(lipgloss.NewStyle(), m.tmuxCell(s)))
	b.WriteString(gap(1))
	now, when := m.currentTime(), s.When()
	timeStyle := m.styles.time
	if isToday(now, when) {
		timeStyle = m.styles.timeNow
	}
	b.WriteString(seg(timeStyle, truncPad(timeCell(now, when), colTime)))

	// Dynamic-width columns. Each emit() prepends a 2-space gap, so a hidden
	// column (width 0) skips both its cell and the gap that would separate
	// it from the previous one -- keeping the row tight when the user turns
	// columns off.
	dirOH := iconOverhead(m.dirIcon)
	branchOH := iconOverhead(m.branchIcon)
	gitOH := iconOverhead(m.gitIcon)
	emit := func(cell string) {
		b.WriteString(gap(2))
		b.WriteString(cell)
	}

	// dir column.
	if m.widths.dir > 0 {
		project := s.Project()
		if m.dirNameOnly {
			project = s.Dir()
		}
		emit(seg(m.styles.project, iconBody(m.dirIcon, project, m.widths.dir-dirOH)))
	}

	// worktree marker slot (between project and branch). It carries its own
	// 2-space gap and is a fixed-width slot — not a dynamic column — so
	// column-widths config doesn't apply. The seg() wrapper applies the
	// worktree style and the row's sel overlay, so the selection bar
	// extends through the slot instead of breaking around the marker.
	b.WriteString(seg(m.styles.worktree, m.worktreeCell(s)))

	// branch column, with an optional per-repo git icon prefix. When the git
	// icon is on, the icon and the branch text share the repo's colour, so
	// the whole git area reads as one colour-coded unit per repo.
	if m.widths.branch > 0 {
		var repoFg lipgloss.TerminalColor
		gitCell := ""
		if m.gitIcon != "" {
			cell := strings.Repeat(" ", lipgloss.Width(m.gitIcon)+1) // reserved, aligned slot
			st := lipgloss.NewStyle()
			if s.Repo != "" {
				cell = m.gitIcon + " "
				repoFg = m.repoColor(s.Repo)
				if repoFg != nil {
					st = st.Foreground(repoFg)
				}
			}
			gitCell = seg(st, cell)
		}
		branchStyle := m.styles.branch
		if repoFg != nil {
			branchStyle = branchStyle.Foreground(repoFg)
		}
		bodyW := m.widths.branch - branchOH - gitOH
		emit(gitCell + seg(branchStyle, iconBody(m.branchIcon, s.Branch, bodyW)))
	}

	// model column.
	if m.widths.model > 0 {
		emit(seg(m.styles.model, truncPad(m.displayModel(s), m.widths.model)))
	}

	// ctx column (gated by [ctx].enabled).
	if m.showCtx {
		text, style := ctxCell(s)
		emit(seg(style, truncPad(text, colCtx)))
	}

	// pane column.
	if m.widths.pane > 0 {
		emit(seg(lipgloss.NewStyle(), truncPad(s.Pane, m.widths.pane)))
	}

	// ci column (gated by the CircleCI token, not by a column bound).
	if m.ciToken != "" {
		emit(seg(lipgloss.NewStyle(), truncPad(m.ciStatus(s), colCI)))
	}

	// Subject and last message. In preview "column" mode the subject is
	// capped at titleCap and shares the remaining row width with the last
	// message (which has its own min/max bounds). In row/off modes the
	// subject takes whatever's left and the last message lives in the detail
	// line below.
	prefixW := lipgloss.Width(b.String())
	rest := max(0, m.width-prefixW)
	titleMax := m.colCfg.Title.Max
	lastMax := m.colCfg.Last.Max
	lastMin := m.colCfg.Last.Min
	inColumn := m.previewMode == previewColumn
	showSubject := !inColumn || titleMax > 0
	showLast := inColumn && s.LastMsg != "" && lastMax > 0

	var subjectW int
	if showSubject {
		switch {
		case inColumn && showLast:
			// Subject takes titleMax first; squeeze to lastMin if needed so
			// the last message has room.
			subjectW = min(titleMax, rest)
			if rest-subjectW < lastMin {
				subjectW = max(0, rest-lastMin)
			}
		case inColumn:
			subjectW = min(titleMax, rest)
		default:
			subjectW = rest
		}
		// In column mode the subject cell is padded to its width so the last
		// message's gap is well-defined. In row/off modes the row ends at the
		// subject -- we only truncate, never pad -- so a short subject leaves
		// the row at its natural width (fill, if requested, pads the tail).
		if inColumn {
			emit(seg(m.styles.subject, truncPad(s.Subject(), subjectW)))
		} else {
			emit(seg(m.styles.subject, trunc(s.Subject(), subjectW)))
		}
	}

	if showLast {
		var lastW int
		if showSubject {
			lastW = max(0, rest-subjectW)
			if lastW > lastMax {
				lastW = lastMax
			}
		} else {
			// subject hidden: the last message takes the rest, bounded by
			// lastMin / lastMax directly.
			lastW = max(lastMin, min(rest, lastMax))
		}
		emit(seg(m.styles.preview, truncPad(s.LastMsg, lastW)))
	}

	line := b.String()
	if fill { // extend the overlay across the rest of the row
		if d := m.width - lipgloss.Width(line); d > 0 {
			line += gap(d)
		}
	}
	return trunc(line, m.width)
}

func ctxCell(s Session) (text string, style lipgloss.Style) {
	style = lipgloss.NewStyle()
	if s.CtxTokens <= 0 {
		return "", style
	}
	// Round to nearest k, cap at 999 (compaction kicks in way before 1M).
	roundedK := (s.CtxTokens + 500) / 1000
	if roundedK > 999 {
		roundedK = 999
	}
	return fmt.Sprintf("%dk", roundedK), style
}

// overlay layers the set attributes of ov onto base (ov wins), used to apply a
// row's selection/dim style to every cell without discarding the cell's own.
func overlay(base, ov lipgloss.Style) lipgloss.Style {
	if ov.GetBold() {
		base = base.Bold(true)
	}
	if ov.GetFaint() {
		base = base.Faint(true)
	}
	if ov.GetReverse() {
		base = base.Reverse(true)
	}
	if c := ov.GetForeground(); !isNoColor(c) {
		base = base.Foreground(c)
	}
	if c := ov.GetBackground(); !isNoColor(c) {
		base = base.Background(c)
	}
	return base
}

func isNoColor(c lipgloss.TerminalColor) bool {
	_, ok := c.(lipgloss.NoColor)
	return ok
}

// iconBody is a fixed-width column value optionally prefixed with an icon. The
// icon slot is reserved on every row (blank when the value is empty) so the
// columns stay aligned regardless of whether the icon is drawn.
func iconBody(icon, text string, w int) string {
	body := truncPad(text, w)
	if icon != "" {
		if strings.TrimSpace(text) == "" {
			body = strings.Repeat(" ", lipgloss.Width(icon)+1) + body
		} else {
			body = icon + " " + body
		}
	}
	return body
}

// defaultRepoColors is the built-in palette for the per-repo git icon, used
// when the config leaves [git] colors unset. It avoids red (reserved for
// alarming states) and leans on ANSI base colours so it follows the terminal
// theme like the rest of the columns.
var defaultRepoColors = []string{"2", "3", "4", "5", "6", "10", "12", "13", "14", "208"}

// repoPalette resolves the configured colour strings (or the built-in palette)
// into lipgloss colours; a nil result disables the tint.
func repoPalette(colors []string) []lipgloss.TerminalColor {
	if len(colors) == 0 {
		colors = defaultRepoColors
	}
	out := make([]lipgloss.TerminalColor, len(colors))
	for i, c := range colors {
		out[i] = lipgloss.Color(c)
	}
	return out
}

// repoColor picks a stable palette colour for a repo by hashing its key, so a
// repo keeps the same colour across runs regardless of index order. Returns nil
// for a session outside any repo or when no palette is configured.
func (m model) repoColor(repo string) lipgloss.TerminalColor {
	if repo == "" || len(m.repoColors) == 0 {
		return nil
	}
	h := fnv.New32a()
	h.Write([]byte(repo))
	return m.repoColors[h.Sum32()%uint32(len(m.repoColors))]
}

// iconCell renders an iconBody with a column style unless plain. Kept as a
// thin wrapper for callers that want a finished cell.
func (m model) iconCell(icon, text string, w int, st lipgloss.Style, plain bool) string {
	body := iconBody(icon, text, w)
	if plain {
		return body
	}
	return st.Render(body)
}

// wordMarker maps a session state to the marker whose reversed-row override
// colour applies to the state word (which is coloured by its state style).
func wordMarker(s SessionState) marker {
	switch s {
	case StateRunning:
		return markerRunning
	case StateWaiting:
		return markerWaiting
	default:
		return markerIdle
	}
}

// markerFor is the status a session's leading glyph should convey.
func (m model) markerFor(s Session) marker {
	if !s.Live() {
		return markerOffline
	}
	if m.unread[s.ID] && s.State == StateIdle {
		return markerUnread
	}
	switch s.State {
	case StateRunning:
		return markerRunning
	case StateWaiting:
		return markerWaiting
	default:
		return markerIdle
	}
}

// styleFor is the colour style for a marker's glyph.
func (m model) styleFor(mk marker) lipgloss.Style {
	switch mk {
	case markerRunning:
		return m.styles.state[StateRunning]
	case markerWaiting:
		return m.styles.state[StateWaiting]
	case markerIdle:
		return m.styles.state[StateIdle]
	case markerUnread:
		return m.styles.unread
	default:
		return m.styles.offline
	}
}

// statusCell is the fixed-width glyph slot for a marker, blank-padded so the
// columns after it stay aligned regardless of which glyph shows.
func (m model) statusCell(mk marker) string {
	g := m.glyphs[mk]
	if mk == markerRunning && g == spinnerSentinel {
		g = spinnerFrames[m.spin%len(spinnerFrames)]
	}
	return pad(g, m.colGlyph)
}

// tmuxCell is the fixed-width session-reachability marker slot: the glyph
// for sessions attachable via a tmux pane, or — mutually exclusive, since a
// live session is either sitting in a pane or running as an unattached
// `claude --background` job, never both — the glyph for a background job,
// blank otherwise. The wider of the two configured glyphs sets the slot's
// width, so columns stay aligned whichever one shows. It is empty (no slot
// at all) when both markers are disabled.
func (m model) tmuxCell(s Session) string {
	w := max(lipgloss.Width(m.tmuxGlyph), lipgloss.Width(m.bgGlyph))
	if w == 0 {
		return ""
	}
	switch {
	case s.InTmux():
		return "  " + pad(m.tmuxGlyph, w)
	case s.Background:
		return "  " + pad(m.bgGlyph, w)
	default:
		return "  " + strings.Repeat(" ", w)
	}
}

// agentCell is the fixed-width source marker slot. It reserves the widest
// configured glyph so sources stay aligned even when their glyphs differ in
// display width. If every glyph is disabled, the slot disappears entirely.
func (m model) agentCell(s Session) string {
	if m.colAgentGlyph == 0 {
		return ""
	}
	return "  " + pad(m.agentGlyphs[s.Source], m.colAgentGlyph)
}

// worktreeCell is the fixed-width worktree marker slot, between the project
// and branch columns, holding the glyph for linked-worktree projects and
// whitespace otherwise so the column stays aligned across rows. The slot is
// empty (no whitespace) when the marker is disabled. The caller wraps the
// result in seg() to apply the worktree style and the row's sel overlay —
// doing the styling here would skip the sel overlay and leave a hole in the
// selection bar.
func (m model) worktreeCell(s Session) string {
	if m.worktreeGlyph == "" {
		return ""
	}
	if s.Worktree {
		return "  " + m.worktreeGlyph
	}
	return "  " + strings.Repeat(" ", lipgloss.Width(m.worktreeGlyph))
}

// previewLine is the indented detail line shown beneath a session in "row"
// mode, carrying its last assistant message.
func (m model) previewLine(s Session) string {
	return trunc("     ↳ "+s.LastMsg, m.width)
}

// pad and trunc both measure display cells (not runes or bytes), so wide
// characters in titles and paths can't skew the columns.
func pad(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func trunc(s string, w int) string {
	return ansi.Truncate(s, w, "…")
}

func (m model) trashSubject(subject string, reserved int) string {
	w := trashSubjectMaxWidth
	if m.width > 0 {
		w = min(w, max(3, m.width-reserved))
	}

	quoted := strconv.Quote(subject)
	if lipgloss.Width(quoted) <= w {
		return quoted
	}
	return `"` + trunc(quoted[1:len(quoted)-1], w-2) + `"`
}

func truncPad(s string, w int) string {
	return pad(trunc(s, w), w)
}

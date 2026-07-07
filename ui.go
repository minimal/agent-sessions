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

const refreshEvery = 2 * time.Second

// doubleClickWithin is the window in which a second left click on the same
// row counts as a double click.
const doubleClickWithin = 400 * time.Millisecond

// Sessions with no live process and no activity for this long are dimmed.
const dimAfter = 24 * time.Hour

// Index column widths. The last column takes the remaining width: the
// subject, unless preview "column" mode caps it (colSubject) to make room
// for the last message.
const (
	colProject = 28
	colBranch  = 24
	colPane    = 12
	colCI      = 4
	colSubject = 30
)

// colState fits every state word the index can show.
var colState = func() int {
	w := len(StateUnknown)
	for _, st := range sessionStates {
		w = max(w, len(st))
	}
	return w
}()

// styles are the configured looks of each UI element.
type styles struct {
	bar      lipgloss.Style
	selected lipgloss.Style
	dim      lipgloss.Style
	preview  lipgloss.Style
	unread   lipgloss.Style
	offline  lipgloss.Style
	index    lipgloss.Style
	time     lipgloss.Style
	project  lipgloss.Style
	branch   lipgloss.Style
	subject  lipgloss.Style
	state    map[SessionState]lipgloss.Style
}

func newStyles(cfg Config) styles {
	return styles{
		bar:      cfg.Styles.Bar.style(),
		selected: cfg.Styles.Selected.style(),
		dim:      cfg.Styles.Dimmed.style(),
		preview:  cfg.Styles.Preview.style(),
		unread:   cfg.Styles.Unread.style(),
		offline:  cfg.Styles.Offline.style(),
		index:    cfg.Styles.Index.style(),
		time:     cfg.Styles.Time.style(),
		project:  cfg.Styles.Project.style(),
		branch:   cfg.Styles.Branch.style(),
		subject:  cfg.Styles.Subject.style(),
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
	loader         *loader
	styles         styles
	bgExec         bool   // run key-bound commands detached (no terminal takeover)
	tmuxGlyph      string // marker for tmux-attachable sessions; "" hides it
	glyphs         map[marker]string
	colGlyph       int                               // display width reserved for the status glyph
	showWords      bool                              // show the state word next to the glyph
	dirIcon        string                            // glyph before the project column; "" hides it
	branchIcon     string                            // glyph before the branch column; "" hides it
	gitIcon        string                            // per-repo glyph before the branch column; "" hides it
	repoColors     []lipgloss.TerminalColor          // palette cycled per repo for the git icon
	dirNameOnly    bool                              // show just the directory name, not the full path
	selColors      bool                              // keep colours on the cursor row
	selStatusColor bool                              // keep status marker/word coloured in reverse mode
	selStatusFg    map[marker]lipgloss.TerminalColor // per-status text-colour overrides for the reversed row
	selBG          lipgloss.TerminalColor            // highlight bg for the cursor row; nil = none
	cursorHidden   bool                              // hide the cursor highlight until the next key/focus
	previewMode    previewMode                       // how to show each session's last message
	previewRecent  int                               // max recent sessions to always preview (row mode)
	previewWithin  time.Duration
	commands       map[string]string // key name -> command template
	ciToken        string            // "" disables the CI column
	ciSlugs        map[string]string // cwd -> CircleCI project slug ("" = none)
	ci             map[string]ciEntry
	ciPending      map[string]time.Time // slug@branch (or cwd@branch) in flight
	all            []Session            // every session, unfiltered
	sessions       []Session            // what the index shows: all, limited by query/project
	query          string
	project        string                  // limit the index to this project cwd; "" is no limit
	liveOnly       bool                    // limit the index to sessions with a running claude process
	input          textinput.Model         // line editor backing the search and text prompts
	searching      bool                    // the search prompt is open and capturing keys
	unread         map[string]bool         // session IDs that finished a turn unseen
	seen           map[string]SessionState // last observed live state, for transitions
	spin           int                     // running-spinner frame index
	spinning       bool                    // a spinner tick is scheduled
	showHelp       bool
	deleting       *Session // awaiting y/n confirmation to delete
	picker         pickerState
	prompt         promptState
	switchOnClick  bool      // a single left click switches, not just selects
	lastClickRow   int       // session index of the previous left click
	lastClickAt    time.Time // when the previous left click happened
	cursor         int
	offset         int
	width          int
	height         int
	loading        bool // a Load is in flight; don't start another
	status         string
	notice         string // shown instead of status until the next keypress
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
	return model{
		loader:         newLoader(cfg.SortDims()),
		styles:         newStyles(cfg),
		commands:       cfg.Commands,
		bgExec:         cfg.Background,
		tmuxGlyph:      cfg.Tmux.Glyph,
		glyphs:         glyphs,
		colGlyph:       glyphWidth(glyphs),
		showWords:      cfg.Status.Words,
		dirIcon:        cfg.Icons.Dir,
		branchIcon:     cfg.Icons.Branch,
		gitIcon:        cfg.Git.Icon,
		repoColors:     repoPalette(cfg.Git.Colors),
		dirNameOnly:    cfg.Display.Project == "name",
		selColors:      cfg.Selection.Colors,
		selStatusColor: cfg.Selection.StatusColor,
		selStatusFg:    selStatusFg,
		selBG:          selBG,
		previewMode:    mode,
		previewRecent:  cfg.Preview.Recent,
		previewWithin:  cfg.PreviewWithin(),
		ciToken:        cfg.ciToken(),
		ciSlugs:        cfg.ciOverrides(),
		ci:             map[string]ciEntry{},
		ciPending:      map[string]time.Time{},
		unread:         map[string]bool{},
		seen:           map[string]SessionState{},
		switchOnClick:  cfg.Mouse.ClickAction == "select-switch",
		lastClickRow:   -1,
		loading:        true,
	}
}

// pickerState is the project-selection overlay, opened either by a command
// containing {project-picker} or by the f (filter by project) key.
type pickerState struct {
	active bool
	filter bool     // the pick becomes the project filter, not a command var
	items  []string // project cwds, most recently used first
	cursor int
	offset int
	tmpl   string            // the command awaiting the pick
	vars   map[string]string // expansion vars captured at keypress
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

type sessionsLoadedMsg struct {
	sessions []Session
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
	return sessionsLoadedMsg{sessions: sessions, err: err}
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
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
			return m, nil
		}
		m.detectUnread(msg.sessions)
		m.all = msg.sessions
		m.applyFilter()
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
			s := *m.deleting
			m.deleting = nil
			if msg.String() == "y" {
				if err := s.Delete(); err != nil {
					m.notice = "delete: " + err.Error()
					return m, nil
				}
				m.notice = fmt.Sprintf("Deleted %q.", s.Subject())
				if !m.loading {
					m.loading = true
					return m, m.loadCmd
				}
			}
			return m, nil
		}
		if m.showHelp {
			m.showHelp = false
			return m, nil
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
				m.notice = "Won't delete a session with a running claude process."
				break
			}
			m.deleting = &s
		case "/":
			m.searching = true
			m.query = ""
			m.input = newLineInput()
			m.applyFilter()
		case "f":
			m.picker = pickerState{active: true, filter: true, items: m.projectList()}
		case "o":
			m.liveOnly = !m.liveOnly
			m.applyFilter()
		case "esc":
			if m.query != "" || m.project != "" || m.liveOnly {
				m.query = ""
				m.project = ""
				m.liveOnly = false
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
		}
	}
	m.clampOffset()
	return m, nil
}

// handleMouse turns mouse input into selection and, per config, a switch.
// The wheel moves the cursor; a left click selects the clicked row (or the
// session its preview line belongs to), and switches to it too — running the
// enter command — when [mouse] click_action is "select-switch" or the click
// is the second of a double click. Overlays keep their own keyboard driving.
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

// runCommand runs a command template for the selected session, handing it
// the terminal so interactive commands (tmux attach, editors) work.
// Templates using {pane} or {pid} need a live session ({pane} additionally
// a tmux pane hosting it) and show a notice otherwise; the optional forms
// {pane?} and {pid?} expand to "" instead, so one command can branch.
func (m model) runCommand(tmpl string) (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.sessions) {
		return m, nil
	}
	s := m.sessions[m.cursor]
	vars := map[string]string{
		"id":    s.ID,
		"pid":   strconv.Itoa(s.PID),
		"pid?":  "",
		"pane?": "",
		"cwd":   s.CWD,
		"file":  s.File,
		"state": string(s.State),
	}
	if s.Live() {
		vars["pid?"] = strconv.Itoa(s.PID)
	}
	if strings.Contains(tmpl, "{pane}") || strings.Contains(tmpl, "{pid}") {
		if !s.Live() {
			m.notice = "Session has no running claude process."
			return m, nil
		}
	}
	if strings.Contains(tmpl, "{pane}") || strings.Contains(tmpl, "{pane?}") {
		pane, ok := tmuxPaneFor(s.PID)
		if ok {
			vars["pane"], vars["pane?"] = pane, pane
		} else if strings.Contains(tmpl, "{pane}") {
			m.notice = "Session is not running in a tmux pane."
			return m, nil
		}
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
		m.picker = pickerState{active: true, items: m.projectList(), tmpl: tmpl, vars: vars}
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

// handlePickerKey drives the project-selection overlay.
func (m model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := &m.picker
	switch msg.String() {
	case "esc", "q":
		m.picker = pickerState{}
	case "enter":
		if len(p.items) == 0 {
			m.picker = pickerState{}
			break
		}
		choice := p.items[p.cursor]
		if p.filter {
			m.picker = pickerState{}
			m.project = choice
			m.applyFilter()
			break
		}
		p.vars["project-picker"] = choice
		tmpl, vars := p.tmpl, p.vars
		m.picker = pickerState{}
		return m.continueCommand(tmpl, vars)
	case "j", "down":
		p.cursor = min(p.cursor+1, max(0, len(p.items)-1))
	case "k", "up":
		p.cursor = max(p.cursor-1, 0)
	case "g", "home":
		p.cursor = 0
	case "G", "end":
		p.cursor = max(0, len(p.items)-1)
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

// applyFilter rebuilds the visible list from the full one, keeps the cursor
// on the same session where possible, and refreshes the status counts.
func (m *model) applyFilter() {
	var selectedID string
	if m.cursor < len(m.sessions) {
		selectedID = m.sessions[m.cursor].ID
	}
	m.sessions = m.all
	if q := strings.ToLower(m.query); q != "" || m.project != "" || m.liveOnly {
		m.sessions = nil
		for _, s := range m.all {
			if m.liveOnly && !s.Live() {
				continue
			}
			if m.project != "" && s.CWD != m.project {
				continue
			}
			if q != "" && !s.matches(q) {
				continue
			}
			m.sessions = append(m.sessions, s)
		}
	}
	m.cursor = min(m.cursor, m.lastRow())
	counts := map[SessionState]int{}
	for i, s := range m.sessions {
		if s.ID == selectedID {
			m.cursor = i
		}
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
	if m.liveOnly {
		parts = append(parts, "running only")
	}
	m.status = strings.Join(parts, ", ")
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

// pageSize is the number of index rows visible between the two bars.
func (m *model) pageSize() int {
	return max(1, m.height-2)
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
	if m.query != "" || m.project != "" || m.liveOnly {
		help = "q:Quit  j/k:Move  Enter:Go  /:Search  f:Filter  o:Running  Esc:Clear filter  r:Refresh  ?:Help"
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
	status := fmt.Sprintf("---Claude Sessions: %s---(%s)", m.status, pos)
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
		status = fmt.Sprintf("Delete %q? (y/n)", m.deleting.Subject())
	}
	b.WriteString(m.styles.bar.Render(pad(status, m.width)))
	return b.String()
}

// pickerView renders the project-selection overlay.
func (m model) pickerView() string {
	var b strings.Builder
	b.WriteString(m.styles.bar.Render(pad("Select a project", m.width)))
	b.WriteString("\n")
	page := m.pageSize()
	for i := 0; i < page; i++ {
		idx := m.picker.offset + i
		if idx < len(m.picker.items) {
			line := trunc(fmt.Sprintf("%4d  %s", idx+1, displayPath(m.picker.items[idx])), m.width)
			if idx == m.picker.cursor {
				line = m.styles.selected.Render(pad(line, m.width))
			}
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	status := fmt.Sprintf("---Select a project: %d known---(Enter:Pick Esc:Cancel)", len(m.picker.items))
	b.WriteString(m.styles.bar.Render(pad(status, m.width)))
	return b.String()
}

// helpView lists the built-in keys and every configured command.
func (m model) helpView() string {
	mouseHelp := "    mouse              click a row to select, double-click to switch; wheel scrolls"
	if m.switchOnClick {
		mouseHelp = "    mouse              click a row to select and switch; wheel scrolls"
	}
	lines := []string{
		"",
		"  Built-in keys",
		"    j / k, arrows      move down / up",
		"    ctrl+d / ctrl+u    half page down / up",
		"    g / G              first / last session",
		"    /                  search; Enter keeps the filter, Esc clears it",
		"    f                  filter the list to one project (opens the picker)",
		"    o                  toggle showing only sessions with a running claude process",
		"    d                  delete session (transcript + sidecar files; asks y/n)",
		mouseHelp,
		"    r                  refresh now",
		"    ?                  this help",
		"    q                  quit",
		"",
	}
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
	lines = append(lines, "  Commands (from config)")
	keys := make([]string, 0, len(m.commands))
	for k, tmpl := range m.commands {
		if tmpl != "" {
			keys = append(keys, k)
		}
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
	for i := 0; i < page; i++ {
		if i < len(lines) {
			b.WriteString(trunc(lines[i], m.width))
		}
		b.WriteString("\n")
	}
	b.WriteString(m.styles.bar.Render(pad("---Help---(press any key to return)", m.width)))
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
	b.WriteString(seg(m.styles.index, fmt.Sprintf("%4d", idx+1)))
	b.WriteString(gap(1))
	b.WriteString(statusSeg(m.styleFor(mk), mk, m.statusCell(mk)))
	if m.showWords {
		w := " " + fmt.Sprintf("%-*s", colState, string(s.State))
		st := lipgloss.NewStyle()
		if ws, ok := m.styles.state[s.State]; ok && s.Live() {
			st = ws
		}
		b.WriteString(statusSeg(st, wordMarker(s.State), w))
	}
	b.WriteString(seg(lipgloss.NewStyle(), m.tmuxCell(s)))
	b.WriteString(gap(2))
	b.WriteString(seg(m.styles.time, s.When().Format("Jan 02 15:04")))
	b.WriteString(gap(2))

	project := s.Project()
	if m.dirNameOnly {
		project = s.Dir()
	}
	b.WriteString(seg(m.styles.project, iconBody(m.dirIcon, project, colProject)))
	b.WriteString(gap(2))
	// With the git icon on, the icon and the branch text share the repo's
	// colour, so the whole git area reads as one colour-coded unit per repo.
	var repoFg lipgloss.TerminalColor
	if m.gitIcon != "" && s.Repo != "" {
		repoFg = m.repoColor(s.Repo)
	}
	if m.gitIcon != "" {
		cell := strings.Repeat(" ", lipgloss.Width(m.gitIcon)+1) // reserved, aligned slot
		st := lipgloss.NewStyle()
		if s.Repo != "" {
			cell = m.gitIcon + " "
			if repoFg != nil {
				st = st.Foreground(repoFg)
			}
		}
		b.WriteString(seg(st, cell))
	}
	branchStyle := m.styles.branch
	if repoFg != nil {
		branchStyle = branchStyle.Foreground(repoFg)
	}
	b.WriteString(seg(branchStyle, iconBody(m.branchIcon, s.Branch, colBranch)))
	b.WriteString(gap(2))
	b.WriteString(seg(lipgloss.NewStyle(), truncPad(s.Pane, colPane)))
	b.WriteString(gap(2))

	if m.ciToken != "" {
		b.WriteString(seg(lipgloss.NewStyle(), truncPad(m.ciStatus(s), colCI)))
		b.WriteString(gap(2))
	}

	subject := s.Subject()
	if m.previewMode == previewColumn {
		subject = truncPad(subject, colSubject)
	}
	b.WriteString(seg(m.styles.subject, subject))
	if m.previewMode == previewColumn && s.LastMsg != "" {
		b.WriteString(gap(2))
		b.WriteString(seg(m.styles.preview, s.LastMsg))
	}

	line := b.String()
	if fill { // extend the overlay across the rest of the row
		if d := m.width - lipgloss.Width(line); d > 0 {
			line += gap(d)
		}
	}
	return trunc(line, m.width)
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

// tmuxCell is the fixed-width tmux marker slot, holding the glyph for
// attachable sessions and blank otherwise, so columns stay aligned. It is
// empty (no slot at all) when the marker is disabled.
func (m model) tmuxCell(s Session) string {
	if m.tmuxGlyph == "" {
		return ""
	}
	if s.InTmux() {
		return "  " + m.tmuxGlyph
	}
	return "  " + strings.Repeat(" ", lipgloss.Width(m.tmuxGlyph))
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

func truncPad(s string, w int) string {
	return pad(trunc(s, w), w)
}

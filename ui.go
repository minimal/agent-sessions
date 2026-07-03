package main

import (
	"fmt"
	"os/exec"
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

// Sessions with no live process and no activity for this long are dimmed.
const dimAfter = 24 * time.Hour

// Index column widths. The last column takes the remaining width: the
// subject, unless preview "column" mode caps it (colSubject) to make room
// for the last message.
const (
	colProject = 28
	colBranch  = 24
	colPane    = 12
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
	state    map[SessionState]lipgloss.Style
}

func newStyles(cfg Config) styles {
	return styles{
		bar:      cfg.Styles.Bar.style(),
		selected: cfg.Styles.Selected.style(),
		dim:      cfg.Styles.Dimmed.style(),
		preview:  cfg.Styles.Preview.style(),
		state: map[SessionState]lipgloss.Style{
			StateRunning: cfg.Styles.Running.style(),
			StateWaiting: cfg.Styles.Waiting.style(),
			StateIdle:    cfg.Styles.Idle.style(),
		},
	}
}

// previewMode selects how a session's last message is shown.
type previewMode string

const (
	previewRow    previewMode = "row"    // a detail line beneath the session
	previewColumn previewMode = "column" // an extra column on the session row
	previewOff    previewMode = "off"    // don't show it
)

type model struct {
	loader        *loader
	styles        styles
	previewMode   previewMode // how to show each session's last message
	previewRecent int         // max recent sessions to always preview (row mode)
	previewWithin time.Duration
	commands      map[string]string // key name -> command template
	all           []Session         // every session, unfiltered
	sessions      []Session         // what the index shows: all, limited by query/project
	query         string
	project       string          // limit the index to this project cwd; "" is no limit
	input         textinput.Model // line editor backing the search and text prompts
	searching     bool            // the search prompt is open and capturing keys
	showHelp      bool
	deleting      *Session // awaiting y/n confirmation to delete
	picker        pickerState
	prompt        promptState
	cursor        int
	offset        int
	width         int
	height        int
	loading       bool // a Load is in flight; don't start another
	status        string
	notice        string // shown instead of status until the next keypress
}

func newModel(cfg Config) model {
	mode := previewMode(cfg.Preview.Mode)
	switch mode {
	case previewRow, previewColumn, previewOff:
	default:
		mode = previewRow
	}
	return model{
		loader:        newLoader(),
		styles:        newStyles(cfg),
		commands:      cfg.Commands,
		previewMode:   mode,
		previewRecent: cfg.Preview.Recent,
		previewWithin: cfg.PreviewWithin(),
		loading:       true,
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

type sessionsLoadedMsg struct {
	sessions []Session
	err      error
}

type tickMsg struct{}

type execDoneMsg struct{ err error }

func (m model) loadCmd() tea.Msg {
	sessions, err := m.loader.Load()
	return sessionsLoadedMsg{sessions: sessions, err: err}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
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
		m.all = msg.sessions
		m.applyFilter()

	case tickMsg:
		if m.loading {
			return m, tickCmd()
		}
		m.loading = true
		return m, tea.Batch(m.loadCmd, tickCmd())

	case execDoneMsg:
		if msg.err != nil {
			m.notice = "command: " + msg.err.Error()
		}
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.loadCmd

	case tea.KeyMsg:
		m.notice = ""
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
		case "esc":
			if m.query != "" || m.project != "" {
				m.query = ""
				m.project = ""
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
	return m, execCmd(tmpl, vars)
}

// execCmd runs an expanded command template with the terminal attached.
func execCmd(tmpl string, vars map[string]string) tea.Cmd {
	cmd := exec.Command("sh", "-c", expandCommand(tmpl, vars))
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
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
	if q := strings.ToLower(m.query); q != "" || m.project != "" {
		m.sessions = nil
		for _, s := range m.all {
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
	m.status = strings.Join(parts, ", ")
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
	return i < m.previewRecent && time.Since(m.sessions[i].Modified) <= m.previewWithin
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

	help := "q:Quit  j/k:Move  Enter:Go  /:Search  f:Filter  r:Refresh  ?:Help"
	if m.query != "" || m.project != "" {
		help = "q:Quit  j/k:Move  Enter:Go  /:Search  f:Filter  Esc:Clear filter  r:Refresh  ?:Help"
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
			case r.si == m.cursor:
				line = m.styles.selected.Render(pad(m.renderRow(r.si), m.width))
			case s.Live():
				line = m.renderRow(r.si)
				if style, ok := m.styles.state[s.State]; ok {
					line = style.Render(line)
				}
			case time.Since(s.Modified) > dimAfter:
				line = m.styles.dim.Render(m.renderRow(r.si))
			default:
				line = m.renderRow(r.si)
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
	lines := []string{
		"",
		"  Built-in keys",
		"    j / k, arrows      move down / up",
		"    ctrl+d / ctrl+u    half page down / up",
		"    g / G              first / last session",
		"    /                  search; Enter keeps the filter, Esc clears it",
		"    f                  filter the list to one project (opens the picker)",
		"    d                  delete session (transcript + sidecar files; asks y/n)",
		"    r                  refresh now",
		"    ?                  this help",
		"    q                  quit",
		"",
		"  Commands (from config)",
	}
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

func (m model) renderRow(idx int) string {
	s := m.sessions[idx]
	subject, tail := s.Subject(), ""
	if m.previewMode == previewColumn {
		subject = truncPad(subject, colSubject)
		if s.LastMsg != "" {
			tail = "  " + s.LastMsg
		}
	}
	line := fmt.Sprintf("%4d %-*s  %s  %s  %s  %s  %s%s",
		idx+1,
		colState, string(s.State),
		s.Modified.Format("Jan 02 15:04"),
		truncPad(s.Project(), colProject),
		truncPad(s.Branch, colBranch),
		truncPad(s.Pane, colPane),
		subject,
		tail,
	)
	return trunc(line, m.width)
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

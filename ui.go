package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const refreshEvery = 2 * time.Second

// Sessions with no live process and no activity for this long are dimmed.
const dimAfter = 24 * time.Hour

// Index column widths. The last column takes the remaining width: the
// subject, unless preview "column" mode caps it (colSubject) to make room
// for the last message.
const (
	colProject = 28
	colBranch  = 24
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
	enterCmd      string      // command template bound to Enter
	previewMode   previewMode // how to show each session's last message
	previewRecent int         // max recent sessions to always preview (row mode)
	previewWithin time.Duration
	all           []Session // every session, unfiltered
	sessions      []Session // what the index shows: all, limited by query
	query         string
	searching     bool // the search prompt is open and capturing keys
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
		enterCmd:      cfg.Commands.Enter,
		previewMode:   mode,
		previewRecent: cfg.Preview.Recent,
		previewWithin: cfg.PreviewWithin(),
		loading:       true,
	}
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
	return tea.Batch(m.loadCmd, tickCmd())
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
			m.notice = "enter command: " + msg.err.Error()
		}
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.loadCmd

	case tea.KeyMsg:
		m.notice = ""
		if m.searching {
			m.handleSearchKey(msg)
			m.clampOffset()
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter":
			return m.gotoSession()
		case "/":
			m.searching = true
			m.query = ""
			m.applyFilter()
		case "esc":
			if m.query != "" {
				m.query = ""
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

// gotoSession runs the configured enter command for the selected session,
// handing it the terminal so interactive commands (tmux attach, editors)
// work. Commands using {pane} or {pid} need a live session; {pane} also
// needs the session's claude process to sit inside a tmux pane.
func (m model) gotoSession() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.sessions) {
		return m, nil
	}
	s := m.sessions[m.cursor]
	tmpl := m.enterCmd
	if tmpl == "" {
		m.notice = "No [commands] enter configured."
		return m, nil
	}
	vars := map[string]string{
		"id":   s.ID,
		"pid":  strconv.Itoa(s.PID),
		"cwd":  s.CWD,
		"file": s.File,
	}
	if strings.Contains(tmpl, "{pane}") || strings.Contains(tmpl, "{pid}") {
		if !s.Live() {
			m.notice = "Session has no running claude process."
			return m, nil
		}
	}
	if strings.Contains(tmpl, "{pane}") {
		pane, ok := tmuxPaneFor(s.PID)
		if !ok {
			m.notice = "Session is not running in a tmux pane."
			return m, nil
		}
		vars["pane"] = pane
	}
	cmd := exec.Command("sh", "-c", expandCommand(tmpl, vars))
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
}

// handleSearchKey edits the query while the search prompt is open. The list
// filters as the query changes; Enter keeps the limit, Esc clears it.
func (m *model) handleSearchKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "enter":
		m.searching = false
	case "esc":
		m.searching = false
		m.query = ""
		m.applyFilter()
	case "backspace":
		if r := []rune(m.query); len(r) > 0 {
			m.query = string(r[:len(r)-1])
			m.applyFilter()
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.query += string(msg.Runes)
			m.applyFilter()
		}
	}
}

// applyFilter rebuilds the visible list from the full one, keeps the cursor
// on the same session where possible, and refreshes the status counts.
func (m *model) applyFilter() {
	var selectedID string
	if m.cursor < len(m.sessions) {
		selectedID = m.sessions[m.cursor].ID
	}
	m.sessions = m.all
	if q := strings.ToLower(m.query); q != "" {
		m.sessions = nil
		for _, s := range m.all {
			if s.matches(q) {
				m.sessions = append(m.sessions, s)
			}
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
		parts = append(parts, fmt.Sprintf("limit %q", m.query))
	}
	m.status = strings.Join(parts, ", ")
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

	var b strings.Builder
	b.WriteString(m.styles.bar.Render(pad("q:Quit  j:Down  k:Up  Enter:Switch  /:Search  Esc:Clear  g/G:Top/Bottom  r:Refresh", m.width)))
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
		status = "Search: " + m.query + "█"
	}
	b.WriteString(m.styles.bar.Render(pad(status, m.width)))
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
	line := fmt.Sprintf("%4d %-*s  %s  %s  %s  %s%s",
		idx+1,
		colState, string(s.State),
		s.Modified.Format("Jan 02 15:04"),
		truncPad(s.Project(), colProject),
		truncPad(s.Branch, colBranch),
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

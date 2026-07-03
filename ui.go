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
	enterCmd       string // command template bound to Enter
	enterBg        bool   // run the Enter command in the background (no terminal takeover)
	tmuxGlyph      string // marker for tmux-attachable sessions; "" hides it
	glyphs         map[marker]string
	colGlyph       int                    // display width reserved for the status glyph
	showWords      bool                   // show the state word next to the glyph
	dirIcon        string                 // glyph before the project column; "" hides it
	branchIcon     string                 // glyph before the branch column; "" hides it
	dirNameOnly    bool                   // show just the directory name, not the full path
	selColors      bool                   // keep colours on the cursor row
	selStatusColor bool                   // keep status marker/word coloured in reverse mode
	selBG          lipgloss.TerminalColor // highlight bg for the cursor row; nil = none
	cursorHidden   bool                   // hide the cursor highlight until the next key/focus
	previewMode    previewMode            // how to show each session's last message
	previewRecent  int                    // max recent sessions to always preview (row mode)
	previewWithin  time.Duration
	all            []Session // every session, unfiltered
	sessions       []Session // what the index shows: all, limited by query
	query          string
	searching      bool                    // the search prompt is open and capturing keys
	unread         map[string]bool         // session IDs that finished a turn unseen
	seen           map[string]SessionState // last observed live state, for transitions
	spin           int                     // running-spinner frame index
	spinning       bool                    // a spinner tick is scheduled
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
	return model{
		loader:         newLoader(),
		styles:         newStyles(cfg),
		enterCmd:       cfg.Commands.Enter,
		enterBg:        cfg.Commands.Background,
		tmuxGlyph:      cfg.Tmux.Glyph,
		glyphs:         glyphs,
		colGlyph:       glyphWidth(glyphs),
		showWords:      cfg.Status.Words,
		dirIcon:        cfg.Icons.Dir,
		branchIcon:     cfg.Icons.Branch,
		dirNameOnly:    cfg.Display.Project == "name",
		selColors:      cfg.Selection.Colors,
		selStatusColor: cfg.Selection.StatusColor,
		selBG:          selBG,
		previewMode:    mode,
		previewRecent:  cfg.Preview.Recent,
		previewWithin:  cfg.PreviewWithin(),
		unread:         map[string]bool{},
		seen:           map[string]SessionState{},
		loading:        true,
	}
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
		m.detectUnread(msg.sessions)
		m.all = msg.sessions
		m.applyFilter()
		m.clampOffset()
		return m, m.ensureSpinner()

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
			m.notice = "enter command: " + msg.err.Error()
		}
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.loadCmd

	case tea.FocusMsg:
		m.cursorHidden = false // regaining focus brings the cursor back

	case tea.KeyMsg:
		m.notice = ""
		m.cursorHidden = false // any key brings the cursor back
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
		if !s.InTmux() {
			m.notice = "Session is not running in a tmux pane."
			return m, nil
		}
		vars["pane"] = s.Pane
	}
	delete(m.unread, s.ID) // opening it counts as reading it
	m.cursorHidden = true  // hide the highlight until the next key or focus
	cmd := exec.Command("sh", "-c", expandCommand(tmpl, vars))
	if m.enterBg {
		// Run detached from the terminal: no alt-screen handoff (which flashes
		// the app closed) and no output to corrupt the display. For commands
		// that only switch a tmux client or focus a pane.
		return m, func() tea.Msg { return execDoneMsg{cmd.Run()} }
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
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
		status = "Search: " + m.query + "█"
	}
	b.WriteString(m.styles.bar.Render(pad(status, m.width)))
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
	// statuscolor on, it keeps the cell's colour and still reverses, so the
	// status shows as a coloured block that matches the bar (rather than the
	// colour dropping out, or punching a default-background hole in the bar).
	statusSeg := func(st lipgloss.Style, txt string) string {
		if m.selStatusColor && sel.GetReverse() && !isNoColor(st.GetForeground()) {
			return overlay(st, sel).Render(txt)
		}
		return seg(st, txt)
	}
	gap := func(n int) string { return seg(lipgloss.NewStyle(), strings.Repeat(" ", n)) }

	mk := m.markerFor(s)
	var b strings.Builder
	b.WriteString(seg(m.styles.index, fmt.Sprintf("%4d", idx+1)))
	b.WriteString(gap(1))
	b.WriteString(statusSeg(m.styleFor(mk), m.statusCell(mk)))
	if m.showWords {
		w := " " + fmt.Sprintf("%-*s", colState, string(s.State))
		st := lipgloss.NewStyle()
		if ws, ok := m.styles.state[s.State]; ok && s.Live() {
			st = ws
		}
		b.WriteString(statusSeg(st, w))
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
	b.WriteString(seg(m.styles.branch, iconBody(m.branchIcon, s.Branch, colBranch)))
	b.WriteString(gap(2))

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

// iconCell renders an iconBody with a column style unless plain. Kept as a
// thin wrapper for callers that want a finished cell.
func (m model) iconCell(icon, text string, w int, st lipgloss.Style, plain bool) string {
	body := iconBody(icon, text, w)
	if plain {
		return body
	}
	return st.Render(body)
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

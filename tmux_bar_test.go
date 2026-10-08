package main

import (
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// tmuxPanesFixture mirrors a real `tmux list-panes -a` result: sessions in
// tmux's order, several panes per session, and a session whose name contains
// the ':' and '.' separators the pane names use.
const tmuxPanesFixture = "\n" +
	"$0\t_home\t1\t%0\t_home:0.0\n" +
	"$4\tagent-sessions\t5\t%6\tagent-sessions:0.0\n" +
	"$4\tagent-sessions\t5\t%13\tagent-sessions:1.0\n" +
	"$4\tagent-sessions\t5\t%7\tagent-sessions:2.0\n" +
	"$5\tagent-sessions_latest\t2\t%8\tagent-sessions_latest:0.0\n" +
	"$5\tagent-sessions_latest\t2\t%12\tagent-sessions_latest:3.0\n" +
	"$6\tx:y\t1\t%19\tx:y:0.0\n" +
	"$7\tx.z\t1\t%20\tx.z:0.0\n"

func TestParseTmuxPanesKeepsTmuxOrderAndWindows(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	want := []tmuxSession{
		{id: "$0", name: "_home", windows: 1},
		{id: "$4", name: "agent-sessions", windows: 5},
		{id: "$5", name: "agent-sessions_latest", windows: 2},
		{id: "$6", name: "x:y", windows: 1},
		{id: "$7", name: "x.z", windows: 1},
	}
	if len(srv.sessions) != len(want) {
		t.Fatalf("got %d sessions, want %d: %+v", len(srv.sessions), len(want), srv.sessions)
	}
	for i, w := range want {
		if got := srv.sessions[i]; got != w {
			t.Errorf("session %d = %+v, want %+v (tmux's own order, one entry per session)", i, got, w)
		}
	}
}

func TestParseTmuxPanesKeysBothPaneFormsToSessionName(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	cases := map[string]string{
		"%6":                        "agent-sessions", // pi's marker form
		"agent-sessions:1.0":        "agent-sessions", // claude's registry form
		"%12":                       "agent-sessions_latest",
		"agent-sessions_latest:3.0": "agent-sessions_latest",
		"x:y:0.0":                   "x:y", // a ':' in the session name is not a separator
		"x.z:0.0":                   "x.z", // nor is a '.' at the end of a name
	}
	for key, want := range cases {
		if got, ok := srv.byPane[key]; !ok || got != want {
			t.Errorf("byPane[%q] = %q (present=%v), want %q", key, got, ok, want)
		}
	}
	// A splitter that took the text before the first ':' would attribute this
	// pane to a session named "x", which does not exist.
	if got := srv.byPane["x:y:0.0"]; got == "x" {
		t.Error("pane forms must be matched exactly, never split on ':'")
	}
}

func TestParseTmuxPanesIgnoresJunkAndEmptyOutput(t *testing.T) {
	srv := parseTmuxPanes("garbage\n$1\tonly-two\tfields\n\n")
	if len(srv.sessions) != 0 {
		t.Errorf("junk lines should be skipped, got %+v", srv.sessions)
	}
	if len(srv.byPane) != 0 {
		t.Errorf("junk lines should add no attribution, got %v", srv.byPane)
	}
	if srv = parseTmuxPanes(""); len(srv.sessions) != 0 {
		t.Errorf("an empty listing should hold no sessions, got %+v", srv.sessions)
	}
}

func TestTmuxCurrentSession(t *testing.T) {
	cases := []struct{ tmux, want string }{
		{"", ""},                                // outside tmux
		{"/tmp/tmux-1000/default,1290,4", "$4"}, // socket, server pid, session id
		{"/tmp/tmux-1000/default,1290,", ""},    // no session id
		{"/tmp/tmux-1000/default,4", ""},        // not the three-field form
	}
	for _, c := range cases {
		t.Setenv("TMUX", c.tmux)
		if got := tmuxCurrentSession(); got != c.want {
			t.Errorf("tmuxCurrentSession() with TMUX=%q = %q, want %q", c.tmux, got, c.want)
		}
	}
}

// TestPollTmuxLive checks the format strings against a real server. It is the
// only test that can catch a mistyped tmux variable, which would silently make
// every chip disappear.
func TestPollTmuxLive(t *testing.T) {
	srv := pollTmux()
	if srv == nil {
		t.Skip("no reachable tmux server")
	}
	if len(srv.sessions) == 0 {
		t.Skip("tmux server has no sessions")
	}
	names := map[string]bool{}
	for _, s := range srv.sessions {
		if s.name == "" {
			t.Errorf("session %q has no name", s.id)
		}
		if s.windows < 1 {
			t.Errorf("session %q reports %d windows", s.name, s.windows)
		}
		names[s.name] = true
	}
	if len(srv.byPane) == 0 {
		t.Error("a server with sessions should attribute at least one pane")
	}
	for key, name := range srv.byPane {
		if !names[name] {
			t.Errorf("pane %q maps to unknown session %q", key, name)
		}
	}
	if srv.current != "" {
		found := false
		for _, s := range srv.sessions {
			found = found || s.id == srv.current
		}
		if !found {
			t.Errorf("current session %q is not on the server: %+v", srv.current, srv.sessions)
		}
	}
}

// barModel builds a model from the shipped defaults, with the given tmux
// snapshot and sessions. It is the starting point for the Bar's rendering tests.
func barModel(t *testing.T, srv *tmuxServer, sessions ...Session) model {
	t.Helper()
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	m := newModel(cfg)
	m.all, m.sessions = sessions, sessions
	m.tmuxSrv = srv
	m.width, m.height = 100, 20
	m.now = time.Now
	return m
}

// liveAgent is a live session sitting in pane, for chip-attribution fixtures.
func liveAgent(id, source string, pane string, state SessionState) Session {
	return Session{
		ID:       id,
		PID:      int64Hash(id),
		Source:   source,
		Pane:     pane,
		State:    state,
		Modified: time.Now(),
	}
}

// int64Hash turns a fixture id into a non-zero PID, so Live() holds without
// naming a process that has to exist.
func int64Hash(id string) int {
	h := 0
	for _, r := range id {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return h + 1
}

func TestTmuxChipsFollowTheServer(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	srv.current = "$5"
	m := barModel(t, srv)
	chips := m.tmuxChips()
	want := []string{"_home", "agent-sessions", "agent-sessions_latest", "x:y", "x.z"}
	if len(chips) != len(want) {
		t.Fatalf("got %d chips, want %d: %+v", len(chips), len(want), chips)
	}
	for i, name := range want {
		if chips[i].Name != name {
			t.Errorf("chip %d = %q, want %q: chips keep tmux's order", i, chips[i].Name, name)
		}
		if chips[i].Current != (name == "agent-sessions_latest") {
			t.Errorf("chip %q Current = %v, want %v", name, chips[i].Current, name == "agent-sessions_latest")
		}
	}
	if chips[1].Windows != 5 {
		t.Errorf("agent-sessions should carry its Window count, got %d", chips[1].Windows)
	}
	if chips[0].Windows != 1 {
		t.Errorf("_home has one Window, got %d", chips[0].Windows)
	}
}

func TestTmuxChipsCountOnlyLiveSessionsWithKnownPanes(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv,
		liveAgent("pi-live", "pi", "%6", StateIdle),                 // %N form
		liveAgent("claude-live", "claude", "x:y:0.0", StateWaiting), // session:window.pane, ':' in the name
		Session{ID: "pi-dead", Pane: "%13"},                         // dead, Pane from pi's cwd heuristic
		liveAgent("unknown-pane", "pi", "%99", StateRunning),        // a pane no session owns
	)
	agents := map[string][]tmuxChipAgent{}
	for _, c := range m.tmuxChips() {
		agents[c.Name] = c.Agents
	}
	if got := agents["agent-sessions"]; len(got) != 1 || got[0].Source != "pi" {
		t.Errorf("agent-sessions agents = %+v, want the one pi session", got)
	}
	if got := agents["x:y"]; len(got) != 1 || !got[0].Waiting {
		t.Errorf("x:y agents = %+v, want the waiting claude session", got)
	}
	total := 0
	for _, a := range agents {
		total += len(a)
	}
	if total != 2 {
		t.Errorf("%d agents on the bar, want 2: a dead session's cwd-guessed Pane and an unknown pane must not show", total)
	}
}

func TestTmuxChipsSurviveTheLiveFilter(t *testing.T) {
	m := barModel(t, parseTmuxPanes(tmuxPanesFixture), Session{ID: "dead", Title: "idle workspace"})
	m.liveOnly = true
	m.applyFilter()
	if len(m.sessions) != 0 {
		t.Fatalf("the live filter should empty the Index, got %d rows", len(m.sessions))
	}
	if chips := m.tmuxChips(); len(chips) != 5 {
		t.Errorf("got %d chips with an empty Index, want all 5 sessions", len(chips))
	}
	if !m.tmuxBarShown() {
		t.Error("the bar should stay visible while the live filter empties the Index")
	}
}

func TestTmuxBarAbsent(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	cases := []struct {
		name  string
		apply func(*model)
	}{
		{"bar disabled", func(m *model) { m.tmuxBar = false }},
		{"no server", func(m *model) { m.tmuxSrv = nil }},
		{"server without sessions", func(m *model) { m.tmuxSrv = parseTmuxPanes("") }},
		{"help screen", func(m *model) { m.showHelp = true }},
		{"picker screen", func(m *model) { m.picker.active = true }},
	}
	for _, c := range cases {
		m := barModel(t, srv)
		m.height = 12
		c.apply(&m)
		if m.tmuxBarShown() {
			t.Errorf("%s: the bar should be absent", c.name)
		}
		if got, want := m.pageSize(), m.height-2; got != want {
			t.Errorf("%s: pageSize() = %d, want %d so the Index regains the row", c.name, got, want)
		}
	}
}

func TestTmuxBarShownTakesARow(t *testing.T) {
	m := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	m.height = 12
	if !m.tmuxBarShown() {
		t.Fatal("a reachable server should show the bar")
	}
	if got, want := m.pageSize(), m.height-3; got != want {
		t.Errorf("pageSize() = %d, want %d while the bar shows", got, want)
	}
}

func TestTmuxBarRendersAboveTheStatusBar(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	srv.current = "$4"
	m := barModel(t, srv, liveAgent("a", "pi", "%6", StateIdle))
	m.width, m.height = 120, 10
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("the view should fill its height, got %d lines:\n%s", len(lines), m.View())
	}
	bar, status := lines[m.height-2], lines[m.height-1]
	if !strings.Contains(status, "Sessions:") {
		t.Errorf("the status bar should stay the last row, got %q", status)
	}
	if !strings.Contains(bar, "agent-sessions") {
		t.Errorf("the row above the status bar should be the Tmux Bar, got %q", bar)
	}
	if !strings.Contains(bar, "▸") {
		t.Errorf("the current session's chip should carry the marker, got %q", bar)
	}
	m.tmuxBar = false
	lines = strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("the view should still fill its height, got %d lines", len(lines))
	}
	if strings.Contains(lines[m.height-2], "▸") {
		t.Errorf("with the bar off the Index should own the row, got %q", lines[m.height-2])
	}
}

func TestTmuxChipContent(t *testing.T) {
	m := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	row := m.renderTmuxBar(m.tmuxChips(), 20)
	if !strings.Contains(row, "agent-sessions(5)") {
		t.Errorf("a chip should show its Window count above one, got %q", row)
	}
	if strings.Contains(row, "_home(") {
		t.Errorf("a single-Window session should show no count, got %q", row)
	}
	if !strings.Contains(row, "x:y") || !strings.Contains(row, "x.z") {
		t.Errorf("names containing ':' and '.' should render whole, got %q", row)
	}
}

func TestTmuxChipGlyphCapAndOverflow(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	sessions := []Session{
		liveAgent("a", "pi", "%6", StateIdle),
		liveAgent("b", "claude", "%6", StateRunning),
		liveAgent("c", "copilot", "%6", StateIdle),
		liveAgent("d", "pi", "%6", StateRunning),
		liveAgent("e", "pi", "%6", StateIdle),
	}
	m := barModel(t, srv, sessions...)
	if m.maxIcons != 3 {
		t.Fatalf("the shipped default should cap glyphs at 3, got %d", m.maxIcons)
	}
	row := m.renderTmuxBar(m.tmuxChips(), 20)
	glyphs := strings.Count(row, m.agentGlyphs["pi"]) + strings.Count(row, m.agentGlyphs["claude"]) +
		strings.Count(row, m.agentGlyphs["copilot"])
	if glyphs != 3 {
		t.Errorf("got %d glyphs, want the cap of 3: %q", glyphs, row)
	}
	if !strings.Contains(row, "+2") {
		t.Errorf("the hidden agents should be counted as +2, got %q", row)
	}
	m.maxIcons = 1
	row = m.renderTmuxBar(m.tmuxChips(), 20)
	if !strings.Contains(row, "+4") {
		t.Errorf("with a cap of 1 the rest should show as +4, got %q", row)
	}
	m.maxIcons = 0
	row = m.renderTmuxBar(m.tmuxChips(), 20)
	if strings.Contains(row, "+") || strings.Contains(row, m.agentGlyphs["pi"]) {
		t.Errorf("a cap of 0 should hide the agent marks, got %q", row)
	}
	if !strings.Contains(row, "agent-sessions(5)") {
		t.Errorf("a cap of 0 should keep the name and Window count, got %q", row)
	}
}

func TestTmuxChipAttentionDecoration(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	// A plain bar: waiting is reverse video.
	m.styles.bar = lipgloss.NewStyle()
	waiting := m.tmuxGlyphCell(tmuxChipAgent{Source: "pi", Waiting: true}, m.styles.bar)
	if !boldAndReverse(waiting) {
		t.Errorf("waiting should render reverse video, got %q", waiting)
	}
	if !strings.Contains(waiting, m.agentGlyphs["pi"]) {
		t.Errorf("the waiting mark should stay the Agent Source, got %q", waiting)
	}
	// The default bar is a reverse row: the mark inverts that row's own video.
	bar := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	waiting = bar.tmuxGlyphCell(tmuxChipAgent{Source: "pi", Waiting: true}, bar.styles.bar)
	if boldAndReverse(waiting) {
		t.Errorf("on a reverse bar, waiting should invert the row rather than repeat its video, got %q", waiting)
	}
	if !strings.Contains(waiting, m.agentGlyphs["pi"]) {
		t.Errorf("the waiting mark should stay the Agent Source, got %q", waiting)
	}
	// Unread carries the attention colour, on the channel the row's reverse swaps.
	unread := bar.tmuxGlyphCell(tmuxChipAgent{Source: "claude", Unread: true}, bar.styles.bar)
	if !strings.Contains(unread, "48;5;208") {
		t.Errorf("unread should use the attention colour, got %q", unread)
	}
	if !strings.Contains(unread, bar.agentGlyphs["claude"]) {
		t.Errorf("the unread mark should stay the Agent Source, got %q", unread)
	}
	// running and idle are plain: the Bar's own style, unchanged.
	for _, a := range []tmuxChipAgent{{Source: "claude"}, {Source: "pi"}} {
		want := bar.styles.bar.Render(pad(bar.agentGlyphs[a.Source], bar.colAgentGlyph))
		if got := bar.tmuxGlyphCell(a, bar.styles.bar); got != want {
			t.Errorf("the %s mark should be plain: got %q, want %q", a.Source, got, want)
		}
	}
	// A source with no configured mark leaves its slot, as in the Index.
	m.agentGlyphs = map[string]string{"pi": "◆", "claude": "", "copilot": ""}
	m.colAgentGlyph = agentGlyphWidth(m.agentGlyphs)
	if got := m.tmuxGlyphCell(tmuxChipAgent{Source: "claude"}, m.styles.bar); strings.TrimSpace(ansi.Strip(got)) != "" {
		t.Errorf("a source with no mark should leave a blank slot, got %q", got)
	}
}

func TestTmuxBarOverflow(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv)
	names := []string{"_home", "agent-sessions", "agent-sessions_latest", "x:y", "x.z"}

	// Room to spare: every chip is whole, and the row fills its width.
	m.width = 200
	row := m.tmuxBarView()
	for _, name := range names {
		if !strings.Contains(row, name) {
			t.Errorf("width 200: chip %q should render whole, got %q", name, ansi.Strip(row))
		}
	}
	if w := lipgloss.Width(row); w != 200 {
		t.Errorf("the bar row should span the width, got %d", w)
	}

	// Narrow: names shrink together, and still nothing is dropped.
	m.width = 40
	row = m.tmuxBarView()
	if w := lipgloss.Width(row); w != 40 {
		t.Errorf("width 40: the bar row should span the width, got %d: %q", w, ansi.Strip(row))
	}
	if strings.Contains(row, "agent-sessions_latest") {
		t.Errorf("an overflowing row should truncate names first, got %q", ansi.Strip(row))
	}
	if len(m.tmuxChips()) != len(names) {
		t.Errorf("truncation should drop no chip, got %d", len(m.tmuxChips()))
	}

	// Too narrow for even the floor: the tail is cut, and the row still fits.
	m.width = 12
	row = m.tmuxBarView()
	if w := lipgloss.Width(row); w != 12 {
		t.Errorf("width 12: the cut row should fill its width, got %d", w)
	}
	if plain := ansi.Strip(row); !strings.HasSuffix(strings.TrimRight(plain, " "), "…") {
		t.Errorf("the cut row should end with a cut mark, got %q", plain)
	}
	if len(m.tmuxChips()) != len(names) {
		t.Errorf("the cut is visual: no chip leaves the list, got %d", len(m.tmuxChips()))
	}
}

func TestTmuxBarEveryRunIsStyled(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := barModel(t, parseTmuxPanes(tmuxPanesFixture),
		liveAgent("a", "pi", "%6", StateIdle))
	m.width = 60
	row := m.tmuxBarView()
	// Text right after a reset would render in the terminal's own colours: the
	// trailing slack must be a styled run of its own, like every chip.
	if strings.Contains(row, "\x1b[0m ") || !strings.HasSuffix(row, "\x1b[0m") {
		t.Errorf("every run of the bar should carry a style, got %q", row)
	}
	// The current session's marker is bold, without reverse: Attention owns that.
	m.tmuxSrv.current = "$4"
	row = m.tmuxBarView()
	if !strings.Contains(row, "▸ ") || strings.Contains(row, "\x1b[7;1m▸") {
		t.Errorf("the current chip should be bold with a plain marker, got %q", row)
	}
}

func TestDefaultConfigShowsTheTmuxBar(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Tmux.Bar {
		t.Error("the shipped default should show the Tmux Bar")
	}
	if cfg.Tmux.MaxIcons != 3 {
		t.Errorf("the shipped default should cap glyphs at 3, got %d", cfg.Tmux.MaxIcons)
	}
	// loadConfig decodes the shipped defaults first, so a user config that
	// predates the key keeps them.
	var merged Config
	if _, err := toml.Decode(defaultConfigTOML, &merged); err != nil {
		t.Fatal(err)
	}
	if _, err := toml.Decode("[tmux]\nglyph = \"X\"\n", &merged); err != nil {
		t.Fatal(err)
	}
	if !merged.Tmux.Bar || merged.Tmux.MaxIcons != 3 {
		t.Errorf("a config setting only [tmux] glyph should keep bar=true and max_icons=3, got %+v", merged.Tmux)
	}
	// A negative cap would panic the slice, so it clamps to none.
	merged.Tmux.MaxIcons = -1
	if m := newModel(merged); m.maxIcons != 0 {
		t.Errorf("a negative max_icons should clamp to 0, got %d", m.maxIcons)
	}
}

// TestTmuxBarLive renders the bar over the real server, when there is one.
func TestTmuxBarLive(t *testing.T) {
	srv := pollTmux()
	if srv == nil || len(srv.sessions) == 0 {
		t.Skip("no reachable tmux server")
	}
	m := barModel(t, srv)
	// Wide enough for every chip whole, whatever the machine's session names are.
	width := 20
	for _, s := range srv.sessions {
		width += lipgloss.Width(s.name) + 12
	}
	m.width, m.height = width, 12
	row := m.tmuxBarView()
	if w := lipgloss.Width(row); w != m.width {
		t.Errorf("the bar row should span the width, got %d", w)
	}
	for _, s := range srv.sessions {
		if !strings.Contains(row, s.name) {
			t.Errorf("session %q is missing from the bar: %q", s.name, ansi.Strip(row))
		}
	}
	t.Logf("bar: %s", ansi.Strip(row))
}

func TestTmuxBarRowIsNotPartOfTheIndex(t *testing.T) {
	m := barModel(t, parseTmuxPanes(tmuxPanesFixture),
		liveAgent("a", "pi", "%6", StateIdle),
		liveAgent("b", "claude", "%6", StateRunning),
		Session{ID: "c"}, Session{ID: "d"}, Session{ID: "e"},
	)
	m.width, m.height = 80, 6
	m.cursorHidden = false

	// A row inside the page still selects.
	mm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 2})
	if got := mm.(model); got.cursor != 1 {
		t.Fatalf("a click on the second visible row should select it, got cursor %d", got.cursor)
	}

	// The bar's own row is below the last index row: it selects nothing, even
	// though more sessions exist off the page.
	m.cursor = 0
	m.cursorHidden = true
	if barY := m.pageSize() + 1; barY >= m.height-1 {
		t.Fatalf("the fixture should put the bar above the status bar, got Y=%d for height %d", barY, m.height)
	}
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: m.pageSize() + 1})
	if got := mm.(model); got.cursor != 0 || !got.cursorHidden {
		t.Errorf("a click on the Tmux Bar should do nothing, got cursor %d hidden %v", got.cursor, got.cursorHidden)
	}
	// So does the status bar.
	m.cursorHidden = true
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: m.height - 1})
	if got := mm.(model); got.cursor != 0 || !got.cursorHidden {
		t.Errorf("a click on the status bar should do nothing, got cursor %d hidden %v", got.cursor, got.cursorHidden)
	}
}

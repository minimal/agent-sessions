package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// tmuxPanesFixture mirrors a real `tmux list-panes -a` result: session id, name
// and Window count, the pane id and its session:window.pane name, then the
// read-tracking columns (attached clients, window active, window zoomed, pane
// active). It covers a name holding ':' and one holding '.', and the three ways
// a pane can be off screen: no attached client, not the session's current
// Window, and a zoomed Window whose pane is not the focused one.
const tmuxPanesFixture = "\n" +
	"$0\t_home\t1\t%0\t_home:0.0\t1\t1\t0\t1\n" + // on screen
	"$4\tagent-sessions\t5\t%6\tagent-sessions:0.0\t1\t1\t1\t1\n" + // zoomed, focused: on screen
	"$4\tagent-sessions\t5\t%13\tagent-sessions:1.0\t1\t1\t1\t0\n" + // zoomed, unfocused: off
	"$4\tagent-sessions\t5\t%7\tagent-sessions:2.0\t1\t0\t0\t1\n" + // not the current Window: off
	"$4\tagent-sessions\t5\t%14\tagent-sessions:3.0\t0\t1\t0\t1\n" + // no client attached: off
	"$4\tagent-sessions\t5\t%18\tagent-sessions:4.0\t1\t1\t0\t0\n" + // side by side: on screen
	"$5\tagent-sessions_latest\t2\t%8\tagent-sessions_latest:0.0\t0\t1\t0\t1\n" +
	"$5\tagent-sessions_latest\t2\t%12\tagent-sessions_latest:3.0\t0\t0\t0\t1\n" +
	"$6\tx:y\t1\t%19\tx:y:0.0\t1\t1\t0\t1\n" +
	"$7\tx.z\t1\t%20\tx.z:0.0\t0\t1\t0\t1\n"

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
		if got, ok := srv.byPane[key]; !ok || got.session != want {
			t.Errorf("byPane[%q].session = %q (present=%v), want %q", key, got.session, ok, want)
		}
	}
	// Both forms point at the same pane, and the id form is the Jump target.
	if got := srv.byPane["x:y:0.0"]; got.id != "%19" || got.name != "x:y:0.0" {
		t.Errorf("byPane[\"x:y:0.0\"] = %+v, want the %%19 pane", got)
	}
	// A splitter that took the text before the first ':' would attribute this
	// pane to a session named "x", which does not exist.
	if got := srv.byPane["x:y:0.0"]; got.session == "x" {
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

// TestSoleAttachedIgnoresPaneCount is the guard for a bug this shipped with.
// tmux repeats session_attached on every pane line of a session, so counting it
// per line made "the only attached session" depend on that session having an
// odd number of panes: an even-pane session left no answer and its chip was
// never marked. Both counts below belong to one attached session.
func TestSoleAttachedIgnoresPaneCount(t *testing.T) {
	for _, panes := range []int{1, 2, 3, 4, 5, 6} {
		var b strings.Builder
		b.WriteString("$1\twork\t1\t%1\twork:0.0\t1\t1\t0\t1\t/dev/pts/1\n")
		for i := 2; i <= panes; i++ {
			fmt.Fprintf(&b, "$1\twork\t1\t%%%d\twork:0.%d\t1\t0\t0\t1\t/dev/pts/1\n", i, i)
		}
		// A second session with no client, so it must not become the answer.
		b.WriteString("$2\tother\t1\t%90\tother:0.0\t0\t1\t0\t1\t/dev/pts/2\n")

		srv := parseTmuxPanes(b.String())
		if srv.attached != "$1" {
			t.Errorf("an attached session with %d panes should be the answer, got %q", panes, srv.attached)
		}
	}
}

// TestSoleAttachedNeedsExactlyOneSession covers the other half: the fallback is
// only a stand-in for identity, so it declines when the answer is ambiguous.
func TestSoleAttachedNeedsExactlyOneSession(t *testing.T) {
	two := "$1\tone\t1\t%1\tone:0.0\t1\t1\t0\t1\t/dev/pts/1\n" +
		"$2\ttwo\t1\t%2\ttwo:0.0\t1\t1\t0\t1\t/dev/pts/2\n"
	if got := parseTmuxPanes(two).attached; got != "" {
		t.Errorf("two attached sessions should give no single answer, got %q", got)
	}
	none := "$1\tone\t1\t%1\tone:0.0\t0\t1\t0\t1\t/dev/pts/1\n"
	if got := parseTmuxPanes(none).attached; got != "" {
		t.Errorf("no attached session should give no answer, got %q", got)
	}
	// The same session reported on several lines is still one session.
	one := "$1\tone\t3\t%1\tone:0.0\t1\t1\t0\t1\t/dev/pts/1\n" +
		"$1\tone\t3\t%2\tone:1.0\t1\t0\t0\t1\t/dev/pts/1\n" +
		"$1\tone\t3\t%3\tone:2.0\t1\t0\t0\t1\t/dev/pts/1\n" +
		"$1\tone\t3\t%4\tone:2.1\t1\t0\t0\t1\t/dev/pts/1\n"
	if got := parseTmuxPanes(one).attached; got != "$1" {
		t.Errorf("one attached session over four lines is still one session, got %q", got)
	}
}

// ttyPanesFixture is the same shape as tmuxPanesFixture plus the pane_tty
// column. The pane the "our own pane" cases point at ($0) is deliberately NOT
// the attached one ($4), so a test can tell the two sources apart: a tty that
// matches must give $0, a tty that does not must fall through to $4.
const ttyPanesFixture = "\n" +
	"$0\t_home\t1\t%0\t_home:0.0\t0\t1\t0\t1\t/dev/pts/9\n" +
	"$4\tagent-sessions\t5\t%6\tagent-sessions:0.0\t1\t1\t0\t1\t/dev/pts/3\n" +
	"$5\tagent-sessions_latest\t2\t%8\tagent-sessions_latest:0.0\t0\t1\t0\t1\t/dev/pts/4\n"

// pinOwnTTY makes this process claim to be on tty for the duration of the test.
func pinOwnTTY(t *testing.T, tty string) {
	t.Helper()
	prev := ownTTY
	ownTTY = func() (string, bool) {
		if tty == "" {
			return "", false
		}
		return tty, true
	}
	t.Cleanup(func() { ownTTY = prev })
}

// TestCurrentTmuxSessionPrefersTMUX covers the normal case: $TMUX names the
// session and that session is on the server being polled.
func TestCurrentTmuxSessionPrefersTMUX(t *testing.T) {
	srv := parseTmuxPanes(ttyPanesFixture)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1290,4")
	t.Setenv("TMUX_PANE", "%6")
	pinOwnTTY(t, "/dev/pts/3")
	if got := currentTmuxSession(srv); got != "$4" {
		t.Errorf("a $TMUX naming a polled session should win, got %q", got)
	}
}

// TestCurrentTmuxSessionIgnoresAForeignTMUX covers $TMUX naming a session that
// is not on the polled server. That is the dashboard's shape in miniature: the
// app sits in a pane of another server, so the id can never match a chip and
// must not stop the search for a real answer.
func TestCurrentTmuxSessionIgnoresAForeignTMUX(t *testing.T) {
	srv := parseTmuxPanes(ttyPanesFixture)
	t.Setenv("TMUX", "/tmp/tmux-1000/other,99,7") // $7 is not in the fixture
	t.Setenv("TMUX_PANE", "%6")
	pinOwnTTY(t, "/dev/pts/3")
	// Falls through to the pane, which really is ours.
	if got := currentTmuxSession(srv); got != "$4" {
		t.Errorf("a foreign $TMUX should fall through to our own pane, got %q", got)
	}
}

// TestCurrentTmuxSessionTrustsThePaneOnlyOnItsOwnTTY guards against marking
// the wrong session. Pane ids are unique only per server, so a pane id from
// another server can name a real pane here by coincidence; the tty is what
// tells the two apart.
func TestCurrentTmuxSessionTrustsThePaneOnlyOnItsOwnTTY(t *testing.T) {
	srv := parseTmuxPanes(ttyPanesFixture)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "%0")

	// Our own pane: the tty matches.
	pinOwnTTY(t, "/dev/pts/9")
	if got := currentTmuxSession(srv); got != "$0" {
		t.Errorf("our own pane should give its session, got %q", got)
	}
	// A pane on another server, which happens to share the id %0. The pane
	// cannot be trusted, so the answer must come from the attached session
	// instead -- $4, not the $0 the pane id would have given.
	pinOwnTTY(t, "/dev/pts/77")
	if got := currentTmuxSession(srv); got != "$4" {
		t.Errorf("a pane with someone else's tty must not be trusted, got %q", got)
	}
	// A poll with no tty column at all: nothing can be proven, so nothing is
	// claimed from the pane.
	bare := parseTmuxPanes(tmuxPanesFixture)
	pinOwnTTY(t, "/dev/pts/9")
	if got := currentTmuxSession(bare); got != "" {
		t.Errorf("a pane with no tty must not be trusted, got %q", got)
	}
}

// TestCurrentTmuxSessionFallsBackToTheSoleAttachedSession covers the dashboard:
// the app is given a $TMUX whose session field is blank and sits in a pane of
// a server the Bar does not show, so the attached work session is the best
// available answer.
func TestCurrentTmuxSessionFallsBackToTheSoleAttachedSession(t *testing.T) {
	srv := parseTmuxPanes(ttyPanesFixture)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,,") // what agent-dashboard sets
	t.Setenv("TMUX_PANE", "%0")                  // a pane of the outer server
	pinOwnTTY(t, "/dev/pts/77")                  // not a pane of this server
	if got := currentTmuxSession(srv); got != "$4" {
		t.Errorf("the sole attached session should stand in, got %q", got)
	}
}

// TestCurrentTmuxSessionClaimsNothingWhenAmbiguous covers several attached
// sessions: there is no single answer, so the Bar marks nothing rather than
// picking one.
func TestCurrentTmuxSessionClaimsNothingWhenAmbiguous(t *testing.T) {
	// tmuxPanesFixture has three sessions with an attached client.
	srv := parseTmuxPanes(tmuxPanesFixture)
	if srv.attached != "" {
		t.Errorf("several attached sessions should leave no single answer, got %q", srv.attached)
	}
	t.Setenv("TMUX", "/tmp/tmux-1000/default,,")
	t.Setenv("TMUX_PANE", "")
	pinOwnTTY(t, "")
	if got := currentTmuxSession(srv); got != "" {
		t.Errorf("with no answer available nothing should be marked, got %q", got)
	}
	// And outside tmux entirely.
	t.Setenv("TMUX", "")
	if got := currentTmuxSession(parseTmuxPanes("")); got != "" {
		t.Errorf("outside tmux there is no current session, got %q", got)
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
	for key, pane := range srv.byPane {
		if !names[pane.session] {
			t.Errorf("pane %q maps to unknown session %q", key, pane.session)
		}
		if pane.tty == "" {
			// A mistyped pane_tty in the format string would not empty the Bar,
			// so nothing else would notice; the "is this my own pane" check
			// would just quietly stop trusting panes.
			t.Errorf("pane %q has no tty, so it can never be identified as ours", key)
		}
		if pane.id == "" || pane.name == "" {
			t.Errorf("pane %q has an incomplete entry: %+v", key, pane)
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
	return barModelWith(t, func(*Config) {}, srv, sessions...)
}

// barModelWith is barModel with a tweak applied to the shipped config first.
func barModelWith(t *testing.T, tweak func(*Config), srv *tmuxServer, sessions ...Session) model {
	t.Helper()
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	tweak(&cfg)
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

func TestTmuxBarRendersBelowTheStatusBar(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	srv.current = "$4"
	m := barModel(t, srv, liveAgent("a", "pi", "%6", StateIdle))
	m.width, m.height = 120, 10
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("the view should fill its height, got %d lines:\n%s", len(lines), m.View())
	}
	status, bar := lines[m.height-2], lines[m.height-1]
	if !strings.Contains(status, "Sessions:") {
		t.Errorf("the status bar should hold the row under the Index, got %q", status)
	}
	if !strings.Contains(bar, "agent-sessions") {
		t.Errorf("the Tmux Bar should own the bottom row, got %q", bar)
	}
	// "You are here" is the current chip's own drawing, so the row has to carry
	// that chip exactly as the renderer marks it.
	chips := m.tmuxChips()
	nameCap := m.tmuxBarNameCap(chips)
	marked := false
	for _, c := range chips {
		if !c.Current {
			continue
		}
		marked = true
		if mark := m.renderTmuxChip(c, nameCap); !strings.Contains(bar, mark) {
			t.Errorf("the current session's chip should be drawn marked, got %q for %q", bar, mark)
		}
	}
	if !marked {
		t.Fatalf("no chip is current for session $4, got %+v", chips)
	}
	m.tmuxBar = false
	lines = strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("the view should still fill its height, got %d lines", len(lines))
	}
	if strings.Contains(m.View(), chipSep) {
		t.Errorf("with the bar off no row should draw chips, got %q", m.View())
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
	// A plain chip, as every chip but the current one is drawn: waiting has to
	// bring its own reverse video to be seen at all.
	strip := m.chipStripStyle()
	waiting := m.tmuxGlyphCell(tmuxChipAgent{Source: "pi", Waiting: true}, strip)
	if !boldAndReverse(waiting) {
		t.Errorf("waiting should render reverse video, got %q", waiting)
	}
	if !strings.Contains(waiting, m.agentGlyphs["pi"]) {
		t.Errorf("the waiting mark should stay the Agent Source, got %q", waiting)
	}
	// Waiting inverts the cell it is drawn in, never repeating it: a reversed
	// mark inside a reversed chip would be a second block inside the first. A
	// config can still ask for a reversed chip, so that base is pinned too.
	block := m.chipStyle(tmuxChip{Current: true}).Reverse(true)
	waiting = m.tmuxGlyphCell(tmuxChipAgent{Source: "pi", Waiting: true}, block)
	if boldAndReverse(waiting) {
		t.Errorf("on a reversed chip, waiting should invert the chip rather than repeat its video, got %q", waiting)
	}
	if !strings.Contains(waiting, m.agentGlyphs["pi"]) {
		t.Errorf("the waiting mark should stay the Agent Source, got %q", waiting)
	}
	// The shipped current chip is a coloured block rather than a reversed one,
	// so its waiting mark is a notch: the block's own colours, inverted.
	waiting = m.tmuxGlyphCell(tmuxChipAgent{Source: "pi", Waiting: true}, m.chipStyle(tmuxChip{Current: true}))
	if !boldAndReverse(waiting) || !strings.Contains(waiting, "46") {
		t.Errorf("waiting on the coloured chip should invert that chip's own colours, got %q", waiting)
	}
	// Unread carries the attention colour on the channel its cell leaves free: a
	// reversed cell swaps the two, so the colour has to go on the background
	// there and on the text everywhere else.
	unread := m.tmuxGlyphCell(tmuxChipAgent{Source: "claude", Unread: true}, block)
	if !strings.Contains(unread, "48;5;208") {
		t.Errorf("unread on a reversed chip should colour its background, got %q", unread)
	}
	unread = m.tmuxGlyphCell(tmuxChipAgent{Source: "claude", Unread: true}, m.chipStyle(tmuxChip{Current: true}))
	if !strings.Contains(unread, "38;5;208") {
		t.Errorf("unread on a plain chip should colour its text, got %q", unread)
	}
	if !strings.Contains(unread, m.agentGlyphs["claude"]) {
		t.Errorf("the unread mark should stay the Agent Source, got %q", unread)
	}
	// running and idle are plain: the chip's own style, unchanged.
	for _, a := range []tmuxChipAgent{{Source: "claude"}, {Source: "pi"}} {
		want := strip.Render(pad(m.agentGlyphs[a.Source], m.colAgentGlyph))
		if got := m.tmuxGlyphCell(a, strip); got != want {
			t.Errorf("the %s mark should be plain: got %q, want %q", a.Source, got, want)
		}
	}
	// A source with no configured mark leaves its slot, as in the Index.
	m.agentGlyphs = map[string]string{"pi": "◆", "claude": "", "copilot": ""}
	m.colAgentGlyph = agentGlyphWidth(m.agentGlyphs)
	if got := m.tmuxGlyphCell(tmuxChipAgent{Source: "claude"}, strip); strings.TrimSpace(ansi.Strip(got)) != "" {
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

// TestTmuxBarChipsAreTabsOnAStrip pins the Bar's tmux-tab look: the chips are
// plain text on the row's own background, separated by a rule, so the row reads
// as one bar of tabs. Only the current chip is a block, and that block is what
// marks it.
func TestTmuxBarChipsAreTabsOnAStrip(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := barModel(t, parseTmuxPanes(tmuxPanesFixture),
		liveAgent("a", "pi", "%6", StateIdle))
	m.width = 200 // room to spare, so the slack after the last chip is obvious
	m.tmuxSrv.current = "$4"
	chips := m.tmuxChips()
	nameCap := m.tmuxBarNameCap(chips)

	// Every chip but the current one is plain: no reverse, no background of its
	// own. A row of blocks is what the tabs replaced.
	for _, c := range chips {
		if c.Current {
			continue
		}
		if st := m.chipStyle(c); st.GetReverse() || !isNoColor(st.GetBackground()) {
			t.Errorf("chip %q should be plain text on the strip, got %+v", c.Name, st)
		}
		plain := ansi.Strip(m.renderTmuxChip(c, nameCap))
		if !strings.HasPrefix(plain, chipPad) || !strings.HasSuffix(plain, chipPad) {
			t.Errorf("chip %q should be padded, got %q", c.Name, plain)
		}
	}
	// The chips are separated by a rule drawn in the strip's own style, so a tab
	// boundary never depends on colour, and the strip itself keeps its reverse
	// lifted: a reversed strip would be the band of video the tabs replaced.
	row := m.tmuxBarView()
	if got, want := strings.Count(row, chipSep), len(chips)-1; got != want {
		t.Errorf("the row should draw %d separators, got %d: %q", want, got, ansi.Strip(row))
	}
	if strip := m.chipStripStyle(); strip.GetReverse() {
		t.Error("the strip should be drawn with the Bar's reverse lifted")
	}
	if got := m.chipStrip(chipSep); !strings.Contains(row, got) {
		t.Errorf("the separator should be drawn in the strip's style, got %q", row)
	}
	// The row's trailing slack is that same strip, so the bar runs to the row's
	// end instead of stopping at the last chip.
	if plain := ansi.Strip(row); lipgloss.Width(plain) != m.width || !strings.HasSuffix(plain, "  ") {
		t.Errorf("the row should end on the strip's slack, got %q", plain)
	}
}

// TestTmuxBarChipWidthsIgnoreWhatIsCurrent pins the shape of the strip: which
// chip is current changes that chip's colours and nothing else. A mark that
// added a cell would make the row jump every time the user moved between
// sessions, and shift every chip after the current one.
func TestTmuxBarChipWidthsIgnoreWhatIsCurrent(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv)
	m.width = 120

	// Lay the row out with each session in turn marked current, and with none.
	var want []int
	for _, id := range []string{"", "$0", "$4", "$5", "$6", "$7"} {
		srv.current = id
		chips := m.tmuxChips()
		nameCap := m.tmuxBarNameCap(chips)
		starts := make([]int, 0, len(chips))
		at := 0
		for _, c := range chips {
			starts = append(starts, at)
			at += lipgloss.Width(m.renderTmuxChip(c, nameCap)) + lipgloss.Width(chipSep)
		}
		if want == nil {
			want = starts
			continue
		}
		if !slices.Equal(starts, want) {
			t.Errorf("with session %q current the chips start at %v, want %v", id, starts, want)
		}
		if got := lipgloss.Width(m.tmuxBarView()); got != m.width {
			t.Errorf("with session %q current the row is %d wide, want %d", id, got, m.width)
		}
	}
}

// TestTmuxBarCurrentChipIsDistinct pins the "you are here" mark: the current
// chip is the row's only block, drawn in [styles.chip_current] on top of the
// strip.
//
// Note what this style must not use: underline. lipgloss routes whitespace
// through a separate space styler when a style is underlined, and that styler
// does not inherit the block's video -- a chip padded with underlined spaces
// loses the padding's background, so the block opens a notch. Bold, a colour,
// and reverse all survive whitespace.
func TestTmuxBarCurrentChipIsDistinct(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	m := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	m.width = 100
	m.tmuxSrv.current = "$4"
	chips := m.tmuxChips()

	var current *tmuxChip
	for i, c := range chips {
		if c.Current {
			current = &chips[i]
			break
		}
	}
	if current == nil {
		t.Fatalf("no chip is marked current for session $4, got %+v", chips)
	}
	st := m.chipStyle(*current)
	if isNoColor(st.GetBackground()) {
		t.Error("the current chip should carry a background by default, so 'you are here' reads on a row of plain tabs")
	}
	if !st.GetBold() {
		t.Error("the current chip should be bold by default: where the colours are not there, bold is the whole mark")
	}
	if st.GetUnderline() {
		t.Error("the current chip must not be underlined: lipgloss would drop the block's background from its padding")
	}
	// The mark is not reverse video. A reverse run over the terminal's own
	// default colours is dropped by the renderer while the rest of the screen
	// stays static, which is exactly when the mark has to hold: the block has to
	// name real colours instead.
	if st.GetReverse() {
		t.Error("the current chip should be marked with its own colours rather than reverse video")
	}
	// Only the current chip is marked, so the mark still means something.
	for _, c := range chips {
		if c.Current {
			continue
		}
		if other := m.chipStyle(c); other.GetBold() || other.GetReverse() || !isNoColor(other.GetBackground()) {
			t.Errorf("chip %q should not carry the current style, got bold=%v reverse=%v bg=%v",
				c.Name, other.GetBold(), other.GetReverse(), other.GetBackground())
		}
	}
	// The chip is identifiable without colour: the block itself is the marker.
	chip := m.renderTmuxChip(*current, 20)
	if plain := ansi.Strip(chip); !strings.HasPrefix(plain, chipPad+current.Name) {
		t.Errorf("the current chip should carry only its own name, got %q", plain)
	}
	// The padding carries the block's background too. lipgloss renders a
	// whitespace-only string through a separate space styler for some attribute
	// sets, dropping the other attributes there, so a styled pad is not a given:
	// if it lost the background, the current chip's block would open a notch at
	// both ends. Pinned on the bytes the app emits -- SGR 46 is the background
	// cyan the shipped default names.
	if leading := chip[:strings.Index(chip, " ")]; !strings.Contains(leading, "46") {
		t.Errorf("the current chip's leading pad should carry its background, got %q", chip)
	}
	// A colour named for the current chip is literal: `bg` colours the block and
	// `fg` its text, with no reverse underneath to swap them into the other
	// channel.
	coloured := barModelWith(t, func(c *Config) {
		c.Styles.ChipCurrent = StyleConfig{Bg: "2", Fg: "7"}
	}, parseTmuxPanes(tmuxPanesFixture))
	coloured.tmuxSrv.current = "$4"
	coloured.width = 100
	st = coloured.chipStyle(tmuxChip{ID: "$4", Name: "agent-sessions", Current: true})
	if st.GetReverse() {
		t.Error("a chip given a colour should not also be reversed: that swaps the colour into the other channel")
	}
	if got := st.GetBackground(); got != lipgloss.Color("2") {
		t.Errorf("the current chip should take the configured background, got %v", got)
	}
	if got := st.GetForeground(); got != lipgloss.Color("7") {
		t.Errorf("the current chip should take the configured foreground, got %v", got)
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

	// The bar's own row is the screen's bottom one: it selects nothing, even
	// though more sessions exist off the page.
	m.cursor = 0
	m.cursorHidden = true
	if barY := m.height - 1; barY <= m.pageSize() {
		t.Fatalf("the fixture should put the bar below the page, got Y=%d for height %d", barY, m.height)
	}
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: m.height - 1})
	if got := mm.(model); got.cursor != 0 || !got.cursorHidden {
		t.Errorf("a click on the Tmux Bar should do nothing, got cursor %d hidden %v", got.cursor, got.cursorHidden)
	}
	// So does the status bar, which now sits above it.
	m.cursorHidden = true
	mm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: m.height - 2})
	if got := mm.(model); got.cursor != 0 || !got.cursorHidden {
		t.Errorf("a click on the status bar should do nothing, got cursor %d hidden %v", got.cursor, got.cursorHidden)
	}
}

// chipNamed returns the chip for a Tmux Session, for the Jump tests.
func chipNamed(t *testing.T, m model, name string) tmuxChip {
	t.Helper()
	for _, c := range m.tmuxChips() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no chip for session %q", name)
	return tmuxChip{}
}

func TestTmuxJumpTargetPicksTheAttentionAgent(t *testing.T) {
	idle := tmuxChipAgent{Source: "pi", ID: "idle", Pane: "%1"}
	unread := tmuxChipAgent{Source: "claude", ID: "unread", Unread: true, Pane: "%2"}
	waiting := tmuxChipAgent{Source: "pi", ID: "waiting", Waiting: true, Pane: "%3"}
	both := tmuxChipAgent{Source: "pi", ID: "both", Waiting: true, Unread: true, Pane: "%4"}
	cases := []struct {
		name   string
		agents []tmuxChipAgent
		pane   string
		id     string
	}{
		{"no agents", nil, "", ""},
		{"no attention", []tmuxChipAgent{idle}, "", ""},
		{"unread", []tmuxChipAgent{idle, unread}, "%2", "unread"},
		{"waiting", []tmuxChipAgent{idle, unread, waiting}, "%3", "waiting"},
		{"waiting beats an earlier unread", []tmuxChipAgent{unread, waiting}, "%3", "waiting"},
		{"one agent with both", []tmuxChipAgent{idle, both}, "%4", "both"},
	}
	for _, c := range cases {
		agent, ok := tmuxJumpTarget(tmuxChip{Agents: c.agents})
		if agent.Pane != c.pane || agent.ID != c.id || ok != (c.pane != "") {
			t.Errorf("%s: tmuxJumpTarget = (%q, %q, %v), want (%q, %q)", c.name, agent.Pane, agent.ID, ok, c.pane, c.id)
		}
	}
}

func TestTmuxJumpTargetUsesThePaneID(t *testing.T) {
	// A claude-form Pane carries ':' and '.', which tmux would misread as a
	// session:window target: the Jump uses the pane's %N id instead.
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv, liveAgent("claude-live", "claude", "x:y:0.0", StateWaiting))
	agent, ok := tmuxJumpTarget(chipNamed(t, m, "x:y"))
	if !ok || agent.Pane != "%19" || agent.ID != "claude-live" {
		t.Errorf("tmuxJumpTarget = (%+v, %v), want the %%19 pane and claude-live", agent, ok)
	}
}

func TestTmuxJumpReadsOnlyTheLandedSession(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv,
		liveAgent("a", "pi", "%6", StateIdle), // agent-sessions
		liveAgent("b", "pi", "%8", StateIdle), // agent-sessions_latest
	)
	m.unread = map[string]bool{"a": true, "b": true}
	mm, cmd := m.jumpTmux(chipNamed(t, m, "agent-sessions"))
	got := mm.(model)
	if cmd == nil {
		t.Error("the Jump should run the [commands] tmux template")
	}
	if got.unread["a"] {
		t.Error("the Jump should read the session it lands on")
	}
	if !got.unread["b"] {
		t.Error("the Jump should read only that one")
	}
	if !got.cursorHidden {
		t.Error("the Jump should hide the cursor highlight until the next key")
	}
	// A chip with no Attention agent lands on a Window whose agents are read
	// already, so nothing changes. _home has no agent at all.
	mm, cmd = got.jumpTmux(chipNamed(t, got, "_home"))
	if cmd == nil {
		t.Error("a chip with no Attention agent should still Jump")
	}
	if !mm.(model).unread["b"] {
		t.Error("a Jump that diverts nowhere should clear nothing")
	}
}

func TestTmuxJumpTargetsTheSessionByID(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv)
	// tmux cannot address x:y by name -- `-t x:y` reads as session "x", window
	// "y" -- so the Jump passes the session id, which addresses any name.
	c := chipNamed(t, m, "x:y")
	if c.ID != "$6" {
		t.Fatalf("a chip should carry its session id, got %q", c.ID)
	}
	line := expandCommand(m.tmuxJump, tmuxJumpVars(c.ID, ""))
	if !strings.Contains(line, "'$6'") {
		t.Errorf("the Jump should target the session by id, got %q", line)
	}
}

// TestTmuxSessionIDsAreValidTargets proves the target form the Jump depends on
// against a real server: every session id resolves.
func TestTmuxSessionIDsAreValidTargets(t *testing.T) {
	srv := pollTmux()
	if srv == nil || len(srv.sessions) == 0 {
		t.Skip("no reachable tmux server")
	}
	for _, s := range srv.sessions {
		if err := exec.Command("tmux", "has-session", "-t", s.id).Run(); err != nil {
			t.Errorf("session id %s should be a valid tmux target: %v", s.id, err)
		}
	}
}

func TestTmuxJumpTemplateVars(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	tmpl := cfg.Commands["tmux"]
	if tmpl == "" {
		t.Fatal("the shipped config should bind [commands] tmux")
	}
	for _, want := range []string{"{tmux-session}", "{tmux-target?}"} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("the shipped Jump should take %s, got %q", want, tmpl)
		}
	}
	// Outside tmux switch-client fails, so the same line falls back to attach;
	// inside tmux it moves the client, and does nothing in the current session.
	if !strings.Contains(tmpl, "switch-client") || !strings.Contains(tmpl, "|| tmux attach") {
		t.Errorf("the shipped Jump should switch the client, else attach, got %q", tmpl)
	}
	// The lines are separate, so a failed select-pane cannot short-circuit the
	// switch: no line joins the selects to the switch with &&.
	for _, line := range strings.Split(tmpl, "\n") {
		if strings.Contains(line, "select-") && strings.Contains(line, "switch-client") {
			t.Errorf("the Jump's selects and switch should be separate lines, got %q", line)
		}
	}
	line := expandCommand(tmpl, tmuxJumpVars("$4", "%6"))
	for _, want := range []string{"'$4'", "'%6'"} {
		if !strings.Contains(line, want) {
			t.Errorf("the expanded Jump should contain %s, got %q", want, line)
		}
	}
	// No Attention agent: the target is empty, so tmux keeps its own Window.
	line = expandCommand(tmpl, tmuxJumpVars("$4", ""))
	if !strings.Contains(line, "select-pane -t ''") {
		t.Errorf("an empty target should expand empty, got %q", line)
	}
}

func TestTmuxKeyOpensPickerAttentionFirst(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv,
		liveAgent("idle", "pi", "%0", StateIdle),        // _home
		liveAgent("unread", "claude", "%8", StateIdle),  // agent-sessions_latest
		liveAgent("waiting", "pi", "%19", StateWaiting), // x:y
	)
	m.unread = map[string]bool{"unread": true}
	mm, _ := m.Update(key("t"))
	got := mm.(model)
	if !got.picker.active {
		t.Fatal("t should open the picker over the Tmux Sessions")
	}
	// Every Tmux Session is offered -- the bar's point -- with the ones that
	// want the user first: waiting, then unread, then tmux's own order.
	want := []string{"x:y", "agent-sessions_latest", "_home", "agent-sessions", "x.z"}
	if !slices.Equal(got.picker.items, want) {
		t.Errorf("picker items = %v, want %v", got.picker.items, want)
	}
	// Picking one Jumps exactly as a click does.
	mm, cmd := got.Update(key("enter"))
	if cmd == nil {
		t.Error("picking a session should Jump")
	}
	if picked := mm.(model); picked.picker.active {
		t.Error("the picker should close on pick")
	}
}

func TestTmuxKeyPickJumpsAndReads(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv, liveAgent("unread", "pi", "%6", StateIdle))
	m.unread = map[string]bool{"unread": true}
	mm, _ := m.Update(key("t"))
	mm, cmd := mm.(model).Update(key("enter"))
	if cmd == nil {
		t.Error("picking a session should Jump")
	}
	if mm.(model).unread["unread"] {
		t.Error("picking a session should read it, like a click")
	}
}

func TestTmuxKeyConfigurable(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	z := barModelWith(t, func(c *Config) { c.Tmux.Key = "z" }, srv)
	if z.tmuxKey != "z" {
		t.Fatalf("[tmux] key should be configurable, got %q", z.tmuxKey)
	}
	if mm, _ := z.Update(key("z")); !mm.(model).picker.active {
		t.Error("the configured key should open the picker")
	}
	if mm, _ := z.Update(key("t")); mm.(model).picker.active {
		t.Error("the old key should be unbound once [tmux] key changes")
	}
	// An empty key unbinds it, like an empty [commands] binding.
	off := barModelWith(t, func(c *Config) { c.Tmux.Key = "" }, srv)
	if mm, _ := off.Update(key("t")); mm.(model).picker.active {
		t.Error("an empty [tmux] key should unbind the picker")
	}
	// Nothing to pick: a notice, not an empty picker.
	bare := barModelWith(t, func(*Config) {}, nil)
	if mm, _ := bare.Update(key("t")); mm.(model).picker.active || mm.(model).notice == "" {
		t.Errorf("with no server the picker should stay shut with a notice, got %+v", mm.(model).notice)
	}
}

func TestTmuxChipAtHitsOnlyChips(t *testing.T) {
	m := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	m.width = 100
	chips := m.tmuxChips()
	nameCap := m.tmuxBarNameCap(chips)
	x := 0
	for _, c := range chips {
		if got, ok := m.tmuxChipAt(x); !ok || got.Name != c.Name {
			t.Errorf("column %d should hit chip %q, got %q (hit=%v)", x, c.Name, got.Name, ok)
		}
		w := lipgloss.Width(m.renderTmuxChip(c, nameCap))
		if got, ok := m.tmuxChipAt(x + w); ok {
			t.Errorf("column %d is the separator after %q, but it hit %q", x+w, c.Name, got.Name)
		}
		x += w + lipgloss.Width(chipSep)
	}
	if _, ok := m.tmuxChipAt(m.width); ok {
		t.Error("a column past the row should hit nothing")
	}
}

func TestTmuxChipClickJumps(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv, liveAgent("a", "pi", "%6", StateIdle))
	m.unread = map[string]bool{"a": true}
	m.width, m.height = 100, 12
	if m.switchOnClick {
		t.Fatal("the fixture should not switch on a plain click")
	}
	// The chips own the screen's bottom row: find it in the view, then click it.
	barY := -1
	for i, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, chipSep) {
			barY = i
		}
	}
	if barY != m.height-1 {
		t.Fatalf("the chips should own the bottom row, got Y=%d for height %d", barY, m.height)
	}
	// The agent-sessions chip is the second one; the first is _home.
	chips := m.tmuxChips()
	x := lipgloss.Width(m.renderTmuxChip(chips[0], m.tmuxBarNameCap(chips))) + lipgloss.Width(chipSep)
	mm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: barY})
	got := mm.(model)
	if cmd == nil {
		t.Fatal("a click on a chip should Jump immediately, whatever [mouse] click_action says")
	}
	if got.unread["a"] {
		t.Error("the click's Jump should read the session it lands on")
	}
	if !got.cursorHidden {
		t.Error("a chip click should not go through the Index's click path")
	}

	// The gap between chips, the status bar and the help row all do nothing.
	for _, c := range []struct {
		name string
		x, y int
	}{
		{"the gap after a chip", x - 1, barY},
		{"the status bar", x, m.height - 2},
		{"the top bar", x, 0}, // the help screen itself is inert too: handleMouse returns early
	} {
		m.unread = map[string]bool{"a": true}
		mm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: c.x, Y: c.y})
		if cmd != nil || !mm.(model).unread["a"] {
			t.Errorf("a click on %s should do nothing", c.name)
		}
	}
}

func TestTmuxJumpToADeadSessionNotices(t *testing.T) {
	srv := pollTmux()
	if srv == nil {
		t.Skip("no reachable tmux server")
	}
	m := barModel(t, srv)
	m.bgExec = true // run the Jump detached, so the test can wait for it
	m.tmuxJump = "tmux switch-client -t {tmux-session} 2>/dev/null || tmux attach -t {tmux-session}"
	_, cmd := m.jumpTmux(tmuxChip{ID: "$9999", Name: "agent-sessions-gone-xyz"})
	if cmd == nil {
		t.Fatal("a Jump to a dead session should still run its command")
	}
	done, ok := cmd().(execDoneMsg)
	if !ok || done.err == nil {
		t.Fatalf("the Jump to a missing session should fail, got %#v", done)
	}
	mm, _ := m.Update(done)
	if notice := mm.(model).notice; !strings.Contains(notice, "command:") {
		t.Errorf("a failed Jump should notice instead of crashing, got %q", notice)
	}
}

func TestParseTmuxPanesMarksOnScreenPanes(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	on := []string{"%0", "_home:0.0", "%6", "agent-sessions:0.0", "%18", "agent-sessions:4.0", "%19", "x:y:0.0"}
	off := []string{
		"%13", "agent-sessions:1.0", // zoomed Window, pane not focused
		"%7", "agent-sessions:2.0", // not the session's current Window
		"%14", "agent-sessions:3.0", // no client attached
		"%8", "agent-sessions_latest:0.0", // no client attached
		"%12", "agent-sessions_latest:3.0", // no client attached, not current
		"%20", "x.z:0.0", // no client attached
	}
	for _, key := range on {
		if !srv.onScreen[key] {
			t.Errorf("%s is on screen in an attached client, want it marked", key)
		}
	}
	for _, key := range off {
		if srv.onScreen[key] {
			t.Errorf("%s is off screen, want it unmarked", key)
		}
	}
}

func TestReadOnScreenClearsOnlyVisiblePanes(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv,
		liveAgent("focused", "pi", "%6", StateIdle),       // zoomed Window, focused pane
		liveAgent("side", "claude", "x:y:0.0", StateIdle), // side by side, ':' in the name
		liveAgent("unfocused", "pi", "%13", StateIdle),    // zoomed Window, other pane
		liveAgent("otherwin", "pi", "%7", StateIdle),      // another Window
		liveAgent("detached", "pi", "%14", StateIdle),     // no client attached
	)
	m.unread = map[string]bool{
		"focused": true, "side": true, "unfocused": true, "otherwin": true, "detached": true,
	}
	m.readOnScreen(m.all)
	for _, id := range []string{"focused", "side"} {
		if m.unread[id] {
			t.Errorf("%s has its Pane on screen: looking at it reads it", id)
		}
	}
	for _, id := range []string{"unfocused", "otherwin", "detached"} {
		if !m.unread[id] {
			t.Errorf("%s is not on screen: it stays unread until acted on", id)
		}
	}
}

func TestTurnFinishingOnScreenIsNeverUnread(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	m := barModel(t, srv,
		liveAgent("visible", "pi", "%18", StateIdle),    // side by side, on screen
		liveAgent("hidden", "claude", "%13", StateIdle), // zoomed away
	)
	// Both were mid-turn at the last poll, so this load sees them finish.
	m.seen = map[string]SessionState{"visible": StateRunning, "hidden": StateRunning}
	mm, _ := m.Update(sessionsLoadedMsg{sessions: m.all, tmux: srv})
	got := mm.(model)
	if got.unread["visible"] {
		t.Error("a turn that finished while its Pane was on screen is not unread")
	}
	if !got.unread["hidden"] {
		t.Error("a turn that finished off screen is unread until acted on")
	}
}

func TestNoServerFallsBackToActingOnly(t *testing.T) {
	m := barModel(t, nil, liveAgent("a", "pi", "%6", StateIdle))
	m.unread = map[string]bool{"a": true}
	m.readOnScreen(m.all)
	if !m.unread["a"] {
		t.Error("with no tmux server, only acting on a session reads it")
	}
	m.cursor = 0
	if _, _ = m.runCommand("true"); m.unread["a"] {
		t.Error("acting on a session through the TUI should still read it")
	}
}

func TestPollTmuxLiveOnScreen(t *testing.T) {
	srv := pollTmux()
	if srv == nil {
		t.Skip("no reachable tmux server")
	}
	for key := range srv.onScreen {
		if _, ok := srv.byPane[key]; !ok {
			t.Errorf("on-screen pane %q is not a pane on the server", key)
		}
	}
	// Inside tmux, an attached client shows its session's current Window, which
	// holds at least one pane. Outside tmux nothing is on screen.
	if srv.current != "" && len(srv.onScreen) == 0 {
		t.Error("inside tmux the current Window should have panes on screen")
	}
	if srv.current == "" && len(srv.onScreen) != 0 {
		t.Error("outside tmux, with no client, nothing can be on screen")
	}
}

// TestJumpLandsOnTheTargetPane runs the shipped Jump's select lines against a
// scratch tmux server, so the diversion is proved by tmux's own state instead
// of by reading the template. The scratch socket keeps it away from the server
// the user is on; the switch-client line needs a client, so it is not run.
func TestJumpLandsOnTheTargetPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	// The socket lives in the test's own temp dir, so the test leaves nothing
	// behind in /tmp/tmux-<uid> and cannot collide with another run.
	socket := filepath.Join(t.TempDir(), "tmux.sock")
	scratch := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-S", socket, "-f", "/dev/null"}, args...)...).Output()
		if err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	scratch("new-session", "-d", "-s", "jump-from")
	scratch("new-session", "-d", "-s", "jump-to")
	// The agent's pane is in a second Window, created detached, so landing there
	// has to move the session's current Window as well as focus the pane.
	target := scratch("new-window", "-d", "-t", "jump-to", "-P", "-F", "#{pane_id}")
	targetWindow := scratch("display-message", "-p", "-t", target, "#{window_id}")
	sessionID := scratch("display-message", "-p", "-t", "jump-to", "#{session_id}")
	socketPath := scratch("display-message", "-p", "#{socket_path}")
	defer exec.Command("tmux", "-S", socket, "kill-server").Run()

	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	jump := func(pane string) {
		t.Helper()
		vars := tmuxJumpVars(sessionID, pane)
		for _, line := range strings.Split(cfg.Commands["tmux"], "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "tmux select-") {
				continue
			}
			cmd := exec.Command("sh", "-c", expandCommand(line, vars))
			cmd.Env = append(os.Environ(), "TMUX="+socketPath)
			if out, err := cmd.CombinedOutput(); err != nil && pane != "" {
				t.Fatalf("the Jump's %q failed: %v: %s", line, err, out)
			}
		}
	}

	if got := scratch("display-message", "-p", "-t", "jump-to", "#{window_id}"); got == targetWindow {
		t.Fatalf("the fixture should start on another Window, got %q", got)
	}
	jump(target)
	if got := scratch("display-message", "-p", "-t", "jump-to", "#{window_id}"); got != targetWindow {
		t.Errorf("the Jump should make the agent's Window current, got %q want %q", got, targetWindow)
	}
	if got := scratch("display-message", "-p", "-t", target, "#{pane_active}"); got != "1" {
		t.Errorf("the Jump should focus the agent's pane, got pane_active=%q", got)
	}

	// With no Attention agent the target is empty: tmux keeps its own Window.
	scratch("select-window", "-t", "jump-to:0")
	jump("")
	if got := scratch("display-message", "-p", "-t", "jump-to", "#{window_index}"); got != "0" {
		t.Errorf("an empty target should leave tmux's own Window, got window %q", got)
	}
}

func TestHelpViewNamesTheJumpNotAKey(t *testing.T) {
	m := barModel(t, parseTmuxPanes(tmuxPanesFixture))
	m.height = 400 // tall enough to render every help line without scrolling
	help := m.helpView()
	if !strings.Contains(help, "jump to a tmux session") {
		t.Errorf("help should name the tmux picker key, got:\n%s", help)
	}
	if !strings.Contains(help, "[commands] tmux") {
		t.Error("help should name the Jump template")
	}
	// The Jump template is not a keystroke, so it must not be listed among the
	// [commands] keys: that would print a binding no key can press.
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "tmux ") {
			t.Errorf("help should not list tmux as a command key, got %q", line)
		}
	}
	for _, want := range []string{"{tmux-session}", "{tmux-target}"} {
		if !strings.Contains(help, want) {
			t.Errorf("help should document %s", want)
		}
	}
}

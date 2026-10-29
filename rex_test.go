package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// fakeRex writes an executable named rex at path.
func fakeRex(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindRexPrefersPath(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	fakeRex(t, filepath.Join(home, rexBundlePath))
	fakeRex(t, filepath.Join(bin, "rex"))

	got, ok := findRex()
	if !ok || got != filepath.Join(bin, "rex") {
		t.Errorf("findRex() = %q, %v; want the PATH binary", got, ok)
	}
}

func TestFindRexFallsBackToTheBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	fakeRex(t, filepath.Join(home, rexBundlePath))

	got, ok := findRex()
	if !ok || got != filepath.Join(home, rexBundlePath) {
		t.Errorf("findRex() = %q, %v; want the bundle binary", got, ok)
	}
}

func TestFindRexNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if got, ok := findRex(); ok {
		t.Errorf("findRex() = %q, want not found", got)
	}
}

func TestRexCommandNeverAutostartsAndDropsTargets(t *testing.T) {
	t.Setenv("REX_SESSION", "session:x")
	t.Setenv("REX_BLOCK", "block:x")
	t.Setenv("REX_CLIENT", "client:x")
	t.Setenv("REX_SERVER", "unix:///tmp/rex.sock")

	cmd := rexCLI{path: "/opt/rex"}.command(context.Background(), "do", "-")
	if want := []string{"/opt/rex", "--autostart=false", "do", "-"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	for _, kv := range cmd.Env {
		for _, name := range []string{"REX_SESSION=", "REX_BLOCK=", "REX_CLIENT="} {
			if strings.HasPrefix(kv, name) {
				t.Errorf("env keeps %s", kv)
			}
		}
	}
	// The server address is not a target: it says which Rex to ask.
	if !slices.Contains(cmd.Env, "REX_SERVER=unix:///tmp/rex.sock") {
		t.Error("env should keep REX_SERVER")
	}
}

func TestRexCommandTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := (rexCLI{path: path, timeout: 50 * time.Millisecond}).run("", "do", "-"); err == nil {
		t.Error("a hung rex should fail")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("run took %s, want it cut at the timeout", d)
	}
}

func TestRexModeWithoutTheCLINotices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.RexMode = true
	m := newModel(cfg)
	if !strings.Contains(m.notice, "rex") {
		t.Errorf("notice = %q, want a missing-rex notice", m.notice)
	}
	if m.rexMode() {
		t.Error("rex mode should be off when the CLI is missing")
	}
}

func TestRexPlaceholderExpandsToTheBinary(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	m := glyphModel()
	m.bgExec = true
	m.rexBin = "/Applications/Rex Beta.app/rex"
	m.sessions = []Session{{ID: "x", Modified: time.Now()}}
	_, cmd := m.runCommand("printf %s {rex} > " + shellQuote(out))
	if cmd == nil {
		t.Fatal("a {rex} template should run in rex mode")
	}
	if done := cmd().(execDoneMsg); done.err != nil {
		t.Fatal(done.err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != m.rexBin {
		t.Errorf("{rex} = %q, want %q", got, m.rexBin)
	}
}

func TestRexPlaceholderNeedsRexMode(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{{ID: "x", Modified: time.Now()}}
	after, cmd := m.runCommand("{rex} ls")
	if cmd != nil {
		t.Error("a {rex} template should not run without the CLI")
	}
	if !strings.Contains(after.(model).notice, "--rex") {
		t.Errorf("notice = %q, want a hint to use --rex", after.(model).notice)
	}
}

// rexPollFixture is a real poll result, trimmed to three blocks.
const rexPollFixture = `[
  {"block_id": "block:01a184a3-593c-7157-982c-882dcebbe33e", "child": 99978, "fg": 3651,
   "label": "floral-strait", "session_id": "session:01a184a3-593b-7dac-8d65-603a3e922048",
   "window_id": "window:01a184a3-5976-7dc3-a1e3-d64cb4392dc1"},
  {"block_id": "block:01a184b5-b6fc-7b13-b76f-e7c49c2ab6ea", "child": 13193, "fg": 13194,
   "label": "floral-strait", "session_id": "session:01a184a3-593b-7dac-8d65-603a3e922048",
   "window_id": "window:01a184b5-b73c-745d-95b4-8375899633ac"},
  {"block_id": "block:01a184d4-c3b3-76ce-80d4-90bc3a20686a", "child": 41110, "fg": 42070,
   "label": "bold-marsh", "session_id": "session:01a184b7-a108-77d0-94a7-3f8748bee391",
   "window_id": "window:01a184d4-c41b-7f26-9721-24f8f4cf8ce3"}
]`

func TestParseRexPoll(t *testing.T) {
	blocks := parseRexPoll([]byte(rexPollFixture))
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(blocks))
	}
	want := rexBlock{
		RexTarget: RexTarget{
			SessionID: "session:01a184b7-a108-77d0-94a7-3f8748bee391",
			WindowID:  "window:01a184d4-c41b-7f26-9721-24f8f4cf8ce3",
			BlockID:   "block:01a184d4-c3b3-76ce-80d4-90bc3a20686a",
			Label:     "bold-marsh",
		},
		FG:    42070,
		Child: 41110,
	}
	if blocks[2] != want {
		t.Errorf("block = %+v, want %+v", blocks[2], want)
	}
}

func TestParseRexPollEmpty(t *testing.T) {
	// Lua serialises an empty table as an object, not an array.
	for _, in := range []string{"{}", "[]", "", "not json"} {
		if got := parseRexPoll([]byte(in)); len(got) != 0 {
			t.Errorf("parseRexPoll(%q) = %+v, want none", in, got)
		}
	}
}

func TestRexTargetName(t *testing.T) {
	r := RexTarget{Label: "bold-marsh", BlockID: "block:01a184d4-c3b3-76ce-80d4-90bc3a20686a"}
	if got := r.Name(); got != "bold-marsh:3a20686a" {
		t.Errorf("Name() = %q, want label:shortid", got)
	}
}

// rexAdapter is a claude adapter whose Rex poll returns blocks and counts calls.
func rexAdapter(calls *int, blocks ...rexBlock) *claudeAdapter {
	a := newClaudeAdapter()
	a.rexPoll = func() []rexBlock {
		*calls++
		return blocks
	}
	return a
}

func rexBlockFixture(fg, child int) rexBlock {
	return rexBlock{
		RexTarget: RexTarget{SessionID: "session:s", WindowID: "window:w", BlockID: "block:b", Label: "bold-marsh"},
		FG:        fg,
		Child:     child,
	}
}

func TestClaudeLiveMapsARexBlockByPid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{"sessionId": "inter", "status": "idle", "kind": "interactive"})

	var calls int
	sessions := []Session{{ID: "inter", Source: "claude"}}
	rexAdapter(&calls, rexBlockFixture(os.Getpid(), 1)).Live(sessions)

	s := sessions[0]
	if s.Rex.BlockID != "block:b" || s.Rex.SessionID != "session:s" || s.Rex.WindowID != "window:w" {
		t.Errorf("Rex = %+v, want the block whose foreground pid is the registry pid", s.Rex)
	}
	if s.Pane != "" {
		t.Errorf("Pane = %q, want empty: Pane is tmux only", s.Pane)
	}
	if got := s.Mux(); got != "rex" {
		t.Errorf("Mux() = %q, want rex", got)
	}
}

func TestClaudeLiveMapsARexBlockThroughAWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{"sessionId": "inter", "status": "idle", "kind": "interactive"})

	// The block's shell is an ancestor; the foreground is some other process.
	var calls int
	sessions := []Session{{ID: "inter", Source: "claude"}}
	rexAdapter(&calls, rexBlockFixture(1<<30, os.Getppid())).Live(sessions)

	if sessions[0].Rex.BlockID != "block:b" {
		t.Errorf("Rex = %+v, want the block whose shell is an ancestor", sessions[0].Rex)
	}
}

func TestClaudeLiveGivesABackgroundJobNoRexBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{"sessionId": "job", "status": "idle", "kind": "bg", "jobId": "abc1"})

	// A bg job stays an OS child of the block that launched it.
	var calls int
	sessions := []Session{{ID: "job", Source: "claude"}}
	rexAdapter(&calls, rexBlockFixture(1<<30, os.Getppid())).Live(sessions)

	if sessions[0].Rex != (RexTarget{}) {
		t.Errorf("Rex = %+v, want none for a background job", sessions[0].Rex)
	}
}

func TestClaudeLivePollsRexOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{"sessionId": "a", "status": "idle", "kind": "interactive"})
	writeRegistry(t, home, map[string]any{"sessionId": "b", "status": "busy", "kind": "interactive"})

	var calls int
	sessions := []Session{{ID: "a", Source: "claude"}, {ID: "b", Source: "claude"}}
	rexAdapter(&calls, rexBlockFixture(os.Getpid(), 1)).Live(sessions)

	if calls != 1 {
		t.Errorf("polled Rex %d times, want once per refresh", calls)
	}
}

func TestClaudeLiveSkipsRexWithoutRexMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{"sessionId": "inter", "status": "idle", "kind": "interactive"})

	sessions := []Session{{ID: "inter", Source: "claude"}}
	newClaudeAdapter().Live(sessions)
	if sessions[0].Rex != (RexTarget{}) || sessions[0].Mux() != "" {
		t.Errorf("session = %+v, want no mux", sessions[0])
	}
}

func TestMux(t *testing.T) {
	rex := RexTarget{BlockID: "block:b", Label: "bold-marsh"}
	for _, tc := range []struct {
		name string
		s    Session
		want string
	}{
		{"tmux", Session{PID: 1, Pane: "%1"}, "tmux"},
		{"rex", Session{PID: 1, Rex: rex}, "rex"},
		{"loose", Session{PID: 1}, ""},
		{"dead pane", Session{Pane: "%1"}, ""},
		{"dead rex", Session{Rex: rex}, ""},
	} {
		if got := tc.s.Mux(); got != tc.want {
			t.Errorf("%s: Mux() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPaneColumnShowsTheRexBlock(t *testing.T) {
	rex := RexTarget{BlockID: "block:01a184d4-c3b3-76ce-80d4-90bc3a20686a", Label: "bold"}
	sessions := []Session{{ID: "a", Title: "in rex", Modified: time.Now(), PID: 1, Rex: rex}}
	m := testModel(previewColumn, sessions)
	m.colCfg.Pane.Max = 20 // the default 12 truncates it
	m.computeWidths()
	if out := m.View(); !strings.Contains(out, "bold:3a20686a") {
		t.Errorf("the pane column should show the Rex block, got:\n%s", out)
	}
}

func TestSearchFindsTheRexSession(t *testing.T) {
	s := Session{ID: "a", PID: 1, Rex: RexTarget{BlockID: "block:b", Label: "bold-marsh"}}
	if !s.matches("bold-marsh") {
		t.Error("search should match the Rex session label")
	}
}

func TestRexRowsStayOffTheTmuxBar(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	rex := Session{ID: "r", Source: "claude", PID: 1, State: StateWaiting, Rex: RexTarget{BlockID: "block:b"}}
	m := barModel(t, srv, rex)
	for _, c := range m.tmuxChips() {
		if len(c.Agents) != 0 {
			t.Errorf("chip %q holds %+v; a Rex row is in no tmux session", c.Name, c.Agents)
		}
	}
}

func TestRexGlyphKeepsRowsAligned(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{ID: "t", Title: "tmux", Modified: now, PID: 1, Pane: "%1"},
		{ID: "r", Title: "rex", Modified: now, PID: 2, Rex: RexTarget{BlockID: "block:b"}},
		{ID: "b", Title: "bg", Modified: now, PID: 3, Background: true},
		{ID: "n", Title: "none", Modified: now},
	}
	m := testModel(previewColumn, sessions)
	m.rexBin = "/opt/rex"
	m.tmuxGlyph, m.rexGlyph, m.bgGlyph = "⊟", "rex", "bg"

	want := map[string]string{"t": "⊟", "r": "rex", "b": "bg", "n": ""}
	for _, s := range sessions {
		cell := m.tmuxCell(s)
		if got := strings.TrimSpace(cell); got != want[s.ID] {
			t.Errorf("%s: cell = %q, want glyph %q", s.ID, cell, want[s.ID])
		}
		if w := lipgloss.Width(cell); w != 2+3 {
			t.Errorf("%s: cell width = %d, want 5: the widest glyph sets the slot", s.ID, w)
		}
	}
}

func TestRexGlyphTakesNoSpaceWithoutRexMode(t *testing.T) {
	s := Session{ID: "t", PID: 1, Pane: "%1"}
	m := testModel(previewColumn, []Session{s})
	m.rexBin = ""
	m.tmuxGlyph, m.rexGlyph, m.bgGlyph = "⊟", "rex", "bg"
	if got := m.tmuxCell(s); got != "  ⊟ " {
		t.Errorf("cell = %q, want the rex glyph to leave the slot width alone", got)
	}
}

func TestRexGlyphOnlyHidesItself(t *testing.T) {
	s := Session{ID: "r", PID: 2, Rex: RexTarget{BlockID: "block:b"}}
	m := testModel(previewColumn, []Session{s})
	m.rexBin = "/opt/rex"
	m.tmuxGlyph, m.rexGlyph, m.bgGlyph = "", "", ""
	if got := m.tmuxCell(s); got != "" {
		t.Errorf("all glyphs off: cell = %q, want no slot", got)
	}
	m.rexGlyph = "⊞"
	if got := m.tmuxCell(s); got != "  ⊞" {
		t.Errorf("only the rex glyph on: cell = %q, want it", got)
	}
}

func TestRexGlyphDefaultsWhenTheUserFileLacksIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[tmux]\nglyph = \"T\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tmux.Glyph != "T" {
		t.Fatalf("tmux glyph = %q, the user file was not read", cfg.Tmux.Glyph)
	}
	var def Config
	if _, err := toml.Decode(defaultConfigTOML, &def); err != nil {
		t.Fatal(err)
	}
	if def.Rex.Glyph == "" || cfg.Rex.Glyph != def.Rex.Glyph {
		t.Errorf("rex glyph = %q, want the default %q", cfg.Rex.Glyph, def.Rex.Glyph)
	}
}

// rexRow is a live claude session in a Rex block.
func rexRow() Session {
	return Session{
		ID: "r", Source: "claude", Title: "in rex", PID: 1, State: StateIdle, Modified: time.Now(),
		Rex: RexTarget{SessionID: "session:s", WindowID: "window:w", BlockID: "block:b", Label: "bold-marsh"},
	}
}

func TestRexPlaceholdersForALiveRexRow(t *testing.T) {
	vars := commandVars(rexRow())
	for k, want := range map[string]string{
		"rex-session": "session:s", "rex-session?": "session:s",
		"rex-window": "window:w", "rex-window?": "window:w",
		"rex-block": "block:b", "rex-block?": "block:b",
	} {
		if vars[k] != want {
			t.Errorf("{%s} = %q, want %q", k, vars[k], want)
		}
	}
}

func TestRexPlaceholdersEmptyForOtherRows(t *testing.T) {
	dead := rexRow()
	dead.PID = 0
	for name, s := range map[string]Session{
		"tmux":    {ID: "t", PID: 1, Pane: "%1"},
		"offline": {ID: "o"},
		"dead":    dead,
	} {
		vars := commandVars(s)
		for _, k := range []string{"rex-session", "rex-session?", "rex-window", "rex-window?", "rex-block", "rex-block?"} {
			if v, ok := vars[k]; !ok || v != "" {
				t.Errorf("%s: {%s} = %q (set %v), want empty", name, k, v, ok)
			}
		}
	}
}

func TestRexPlaceholderNeedsARexRow(t *testing.T) {
	for _, tmpl := range []string{"echo {rex-session}", "echo {rex-window}", "echo {rex-block}"} {
		m := glyphModel()
		m.sessions = []Session{{ID: "t", PID: 1, Pane: "%1", Modified: time.Now()}}
		after, cmd := m.runCommand(tmpl)
		if cmd != nil {
			t.Errorf("%q should not run for a tmux row", tmpl)
		}
		if !strings.Contains(after.(model).notice, "Rex block") {
			t.Errorf("%q: notice = %q, want a Rex block notice", tmpl, after.(model).notice)
		}
	}
}

func TestOptionalRexPlaceholderRunsForAnyRow(t *testing.T) {
	m := glyphModel()
	m.bgExec = true
	m.sessions = []Session{{ID: "t", PID: 1, Pane: "%1", Modified: time.Now()}}
	if _, cmd := m.runCommand("test -z {rex-block?}"); cmd == nil {
		t.Error("{rex-block?} should expand empty, not block")
	} else if done := cmd().(execDoneMsg); done.err != nil {
		t.Errorf("{rex-block?} should expand empty: %v", done.err)
	}
}

// enterModel is a Rex-mode model whose Enter commands write what ran to a file.
func enterModel(t *testing.T, s Session) (model, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	m := glyphModel()
	m.rexBin = "/opt/rex"
	m.rexEnter = "printf 'rex %s' {rex-block} > " + shellQuote(out)
	m.commands = map[string]string{"enter": "printf enter > " + shellQuote(out)}
	m.bgExec = true
	m.sessions = []Session{s}
	return m, out
}

// detachedOutput runs cmd as Bubble Tea would for a detached command and returns what it
// wrote. A takeover command (tea.Exec) fails the test.
func detachedOutput(t *testing.T, cmd tea.Cmd, out string) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command ran")
	}
	done, ok := cmd().(execDoneMsg)
	if !ok {
		t.Fatal("the command took over the terminal; want it detached")
	}
	if done.err != nil {
		t.Fatal(done.err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestEnterOnARexRowRunsRexEnterDetached(t *testing.T) {
	m, out := enterModel(t, rexRow())
	m.bgExec = false // [rex] enter only drives the app: never a takeover
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := detachedOutput(t, cmd, out); got != "rex block:b" {
		t.Errorf("Enter ran %q, want [rex] enter", got)
	}
}

func TestEnterOnATmuxRowKeepsTheEnterCommand(t *testing.T) {
	m, out := enterModel(t, Session{ID: "t", PID: 1, Pane: "%1", Modified: time.Now()})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := detachedOutput(t, cmd, out); got != "enter" {
		t.Errorf("Enter ran %q, want the enter command", got)
	}
}

func TestEnterIgnoresRexEnterWithoutRexMode(t *testing.T) {
	m, out := enterModel(t, rexRow())
	m.rexBin = ""
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := detachedOutput(t, cmd, out); got != "enter" {
		t.Errorf("Enter ran %q, want the enter command", got)
	}
}

func TestEmptyRexEnterKeepsTheEnterCommand(t *testing.T) {
	m, out := enterModel(t, rexRow())
	m.rexEnter = ""
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := detachedOutput(t, cmd, out); got != "enter" {
		t.Errorf("Enter ran %q, want the enter command", got)
	}
}

func TestRexEnterWinsOverTheSourceEnter(t *testing.T) {
	m, out := enterModel(t, rexRow())
	m.enterBySource = map[string]string{"claude": "printf source > " + shellQuote(out)}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := detachedOutput(t, cmd, out); got != "rex block:b" {
		t.Errorf("Enter ran %q, want [rex] enter", got)
	}
}

func TestDoubleClickOnARexRowRunsRexEnter(t *testing.T) {
	m, out := enterModel(t, rexRow())
	m.switchOnClick = true
	m.width, m.height = 120, 10
	_, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 1})
	if got := detachedOutput(t, cmd, out); got != "rex block:b" {
		t.Errorf("a click ran %q, want [rex] enter", got)
	}
}

func TestClickWithNothingToRunOnlySelects(t *testing.T) {
	m, _ := enterModel(t, rexRow())
	m.rexBin = "" // [rex] enter is set but not in use
	m.commands = nil
	m.switchOnClick = true
	m.width, m.height = 120, 10
	mm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 1})
	if cmd != nil {
		t.Error("a click with no enter command should run nothing")
	}
	if got := mm.(model).lastClickRow; got != 0 {
		t.Errorf("lastClickRow = %d, want the click kept for a double-click", got)
	}
}

func TestDefaultRexEnter(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	tmpl := cfg.Rex.Enter
	for _, want := range []string{"{rex} --autostart=false focus", "session.select", "{rex-session}", "{rex-window}", "{rex-block}", `open -a "Rex Beta"`} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("[rex] enter should contain %s, got %q", want, tmpl)
		}
	}
	vars := commandVars(rexRow())
	vars["rex"] = "/opt/rex"
	if line := expandCommand(tmpl, vars); strings.Contains(line, "{rex") {
		t.Errorf("expanded [rex] enter keeps a placeholder: %q", line)
	}
}

// runDefaultRexEnter runs the shipped [rex] enter for rexRow with fake rex,
// osascript and open, and returns the calls they logged, one per line.
func runDefaultRexEnter(t *testing.T, termProgram string, rexFails bool) ([]string, error) {
	t.Helper()
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	exit := "0"
	if rexFails {
		exit = "1"
	}
	for name, body := range map[string]string{
		"rex":       `echo "rex $2" >> "$CALLS"; exit ` + exit,
		"osascript": `cat > /dev/null; echo "osascript $2" >> "$CALLS"`,
		"open":      `echo "open $*" >> "$CALLS"`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CALLS", calls)
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("TERM_PROGRAM", termProgram)
	vars := commandVars(rexRow())
	vars["rex"] = filepath.Join(dir, "rex")
	err := execCmd(cfg.Rex.Enter, vars, true)().(execDoneMsg).err
	data, _ := os.ReadFile(calls)
	return strings.Split(strings.TrimSpace(string(data)), "\n"), err
}

func TestDefaultRexEnterHidesTheQuickTerminalFirst(t *testing.T) {
	calls, err := runDefaultRexEnter(t, "ghostty", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rex focus", "rex do", "osascript /dev/", "open -a Rex Beta"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	for i := range want {
		if !strings.HasPrefix(calls[i], want[i]) {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestDefaultRexEnterSkipsGhosttyOutsideGhostty(t *testing.T) {
	calls, err := runDefaultRexEnter(t, "rex", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rex focus", "rex do", "open -a Rex Beta"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

func TestDefaultRexEnterStopsWhenRexFails(t *testing.T) {
	calls, err := runDefaultRexEnter(t, "ghostty", true)
	if err == nil {
		t.Error("a failed rex call should fail the command")
	}
	if want := []string{"rex focus"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want only the failed focus", calls)
	}
}

func TestDetachedCommandOutputGoesToTheLog(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	done := execCmd("echo to-the-log; exit 3", nil, true)().(execDoneMsg)
	if done.err == nil {
		t.Error("exit 3 should be an error")
	}
	log, err := os.ReadFile(commandLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "to-the-log") {
		t.Errorf("command log = %q, want the command's output", log)
	}
}

func TestDetachedCommandErrorCarriesItsLastLine(t *testing.T) {
	done := execCmd("echo first; echo 'Turn on Remote Control' >&2; echo; exit 2", nil, true)().(execDoneMsg)
	if done.err == nil {
		t.Fatal("exit 2 should be an error")
	}
	if got := done.err.Error(); !strings.Contains(got, "Turn on Remote Control") || !strings.Contains(got, "exit status 2") {
		t.Errorf("err = %q, want the last output line and the exit status", got)
	}
}

func TestFailedDetachedCommandNoticeShowsWhy(t *testing.T) {
	m := glyphModel()
	after, _ := m.Update(execDoneMsg{fmt.Errorf("Turn on Remote Control (exit status 1)")})
	if got := after.(model).notice; !strings.HasPrefix(got, "command failed: Turn on Remote Control") {
		t.Errorf("notice = %q, want the reason first", got)
	}
}

// TestPollRexLive runs the real poll against a running Rex server. It skips
// when there is no rex CLI or no server.
func TestPollRexLive(t *testing.T) {
	bin, ok := findRex()
	if !ok {
		t.Skip("no rex CLI")
	}
	r := rexCLI{path: bin}
	if _, err := r.run("", "ls", "--json"); err != nil {
		t.Skip("no Rex server:", err)
	}
	blocks := r.poll()
	if len(blocks) == 0 {
		t.Skip("Rex has no terminal blocks")
	}
	for _, b := range blocks {
		if !strings.HasPrefix(b.BlockID, "block:") || !strings.HasPrefix(b.SessionID, "session:") ||
			!strings.HasPrefix(b.WindowID, "window:") {
			t.Errorf("block ids look wrong: %+v", b)
		}
	}
}

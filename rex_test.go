package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
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

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func testModel(mode previewMode, sessions []Session) model {
	// Real sessions' last activity tracks their mtime; only timestamp-less
	// stubs diverge. Mirror that so fixtures need only set Modified.
	for i := range sessions {
		if sessions[i].Activity.IsZero() {
			sessions[i].Activity = sessions[i].Modified
		}
	}
	m := model{
		previewMode:   mode,
		previewRecent: 5,
		previewWithin: 20 * time.Minute,
		showWords:     true,
		sessions:      sessions,
		width:         160,
		height:        40, // tall enough to show every row
	}
	m.clampOffset()
	return m
}

func TestActivityIgnoresTimestamplessLines(t *testing.T) {
	var s Session
	msgTS := "2026-07-02T19:08:41.637Z"
	absorb(&s, transcriptLine{
		Type: "assistant", Timestamp: msgTS,
		Message: &struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{Role: "assistant", Content: json.RawMessage(`"Done!"`)},
	})
	want, _ := time.Parse(time.RFC3339, msgTS)
	if !s.Activity.Equal(want) {
		t.Fatalf("message line should set Activity to %v, got %v", want, s.Activity)
	}
	// A later mode/permission-mode write carries no timestamp and must not
	// advance Activity — otherwise a stray mode change reorders the session.
	absorb(&s, transcriptLine{Type: "mode"})
	absorb(&s, transcriptLine{Type: "permission-mode"})
	if !s.Activity.Equal(want) {
		t.Errorf("timestamp-less lines must not change Activity, got %v", s.Activity)
	}
}

func TestRowModePreviews(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{ID: "a", Title: "Recent one", LastMsg: "Done!", Modified: now},
		{ID: "b", Title: "Recent two", LastMsg: "All finished", Modified: now.Add(-5 * time.Minute)},
		{ID: "c", Title: "Old", LastMsg: "ancient reply", Modified: now.Add(-3 * time.Hour)},
	}
	m := testModel(previewRow, sessions)
	out := m.View()

	// Recent sessions show their last message as a detail line...
	if !strings.Contains(out, "↳ Done!") {
		t.Errorf("expected recent session's preview line, got:\n%s", out)
	}
	if !strings.Contains(out, "↳ All finished") {
		t.Errorf("expected second recent preview line, got:\n%s", out)
	}
	// ...but a stale, non-selected session (cursor is on 0) does not.
	if strings.Contains(out, "ancient reply") {
		t.Errorf("stale session should not preview, got:\n%s", out)
	}
}

func TestCursorAlwaysPreviews(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{ID: "a", Title: "Recent", LastMsg: "Done!", Modified: now},
		{ID: "c", Title: "Old", LastMsg: "ancient reply", Modified: now.Add(-3 * time.Hour)},
	}
	m := testModel(previewRow, sessions)
	m.cursor = 1 // select the stale one
	m.clampOffset()
	out := m.View()
	if !strings.Contains(out, "↳ ancient reply") {
		t.Errorf("selected session should always preview, got:\n%s", out)
	}
}

func TestColumnMode(t *testing.T) {
	now := time.Now()
	sessions := []Session{{ID: "a", Title: "Recent", LastMsg: "Done!", Modified: now}}
	m := testModel(previewColumn, sessions)
	out := m.View()
	if strings.Contains(out, "↳") {
		t.Errorf("column mode should not emit detail lines, got:\n%s", out)
	}
	if !strings.Contains(out, "Done!") {
		t.Errorf("column mode should show the message inline, got:\n%s", out)
	}
	// Title is kept alongside the message.
	if !strings.Contains(out, "Recent") {
		t.Errorf("column mode should keep the subject, got:\n%s", out)
	}
}

func TestOffModeNoPreview(t *testing.T) {
	now := time.Now()
	sessions := []Session{{ID: "a", Title: "Recent", LastMsg: "Done!", Modified: now}}
	m := testModel(previewOff, sessions)
	out := m.View()
	if strings.Contains(out, "Done!") || strings.Contains(out, "↳") {
		t.Errorf("off mode should show no last message, got:\n%s", out)
	}
}

func TestScrollShowsCursorAndDetail(t *testing.T) {
	now := time.Now()
	var sessions []Session
	for i := 0; i < 50; i++ {
		sessions = append(sessions, Session{
			ID:       string(rune('a' + i)),
			Title:    "Session",
			LastMsg:  "reply here",
			Modified: now.Add(-time.Duration(i) * time.Hour), // only #0 is recent
		})
	}
	m := testModel(previewRow, sessions)
	m.height = 12 // force scrolling
	m.cursor = 40
	m.clampOffset()
	out := m.View()
	// The selected far-down session and its preview must both be on screen.
	if !strings.Contains(out, "  41 ") {
		t.Errorf("cursor row 41 should be visible after scroll, got:\n%s", out)
	}
	if strings.Count(out, "↳ reply here") == 0 {
		t.Errorf("cursor's preview line should be visible, got:\n%s", out)
	}
}

func TestRealDataRenders(t *testing.T) {
	sessions, err := newClaudeAdapter().Sessions()
	if err != nil || len(sessions) == 0 {
		t.Skip("no real claude sessions available")
	}
	for _, s := range sessions {
		if s.Source != "claude" {
			t.Errorf("claude adapter must tag Source=%q, got %q", "claude", s.Source)
		}
	}
	m := testModel(previewRow, sessions)
	m.height = 40
	m.clampOffset()
	out := m.View()
	if !strings.Contains(out, "↳ ") {
		t.Errorf("expected at least one preview line from real data")
	}
}

func TestPiRealDataRenders(t *testing.T) {
	sessions, err := newPiAdapter("").Sessions()
	if err != nil || len(sessions) == 0 {
		t.Skip("no real pi sessions available")
	}
	for _, s := range sessions {
		if s.Source != "pi" {
			t.Errorf("pi adapter must tag Source=%q, got %q", "pi", s.Source)
		}
		// A session's ID is always recoverable (UUID in the filename), even for
		// stubs. CWD is best-effort: it comes from the `session` header line, which
		// most transcripts start with -- but resumed/forked/ephemeral sessions can
		// omit it entirely, leaving CWD empty (the session then shows "(empty
		// session)" and project "?", like Claude's empty transcripts). The dir name
		// encodes the cwd too, but pi's `/`->`-` encoding is ambiguous with literal
		// hyphens in segment names, so we don't decode it. So: assert ID always,
		// but don't require CWD on every non-empty file.
		if s.ID == "" {
			t.Errorf("pi session %q has no ID (should fall back to filename UUID)", s.File)
		}
	}
}

// TestMultiLoaderMergeAndRender wires both adapters through the multiLoader
// and the UI, the way the real app does, and checks the merged list renders
// with the source-agnostic status bar. Skips when no real sessions exist.
func TestMultiLoaderMergeAndRender(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	ml := newMultiLoader([]Adapter{newClaudeAdapter(), newPiAdapter(cfg.Sources.Pi.SessionDir)}, cfg.SortDims())
	sessions, err := ml.Load()
	if err != nil {
		t.Fatalf("multiLoader.Load: %v", err)
	}
	if len(sessions) == 0 {
		t.Skip("no real sessions available")
	}
	sources := map[string]bool{}
	for _, s := range sessions {
		if s.Source == "" {
			t.Error("merged session has empty Source")
		}
		sources[s.Source] = true
	}
	if !sources["pi"] {
		t.Errorf("expected pi sessions in the merge, got sources %v", sources)
	}

	m := newModel(cfg)
	m.all = sessions
	m.sessions = sessions
	m.width, m.height = 160, 40
	m.applyFilter()
	m.clampOffset()
	out := m.View()
	if !strings.Contains(out, "Sessions:") {
		t.Errorf("status bar should be source-agnostic 'Sessions:', got:\n%s", out)
	}
}

func TestDirNameOnly(t *testing.T) {
	now := time.Now()
	s := Session{ID: "a", Title: "x", CWD: "/tmp/outer/inner", Modified: now}
	m := testModel(previewColumn, []Session{s})

	full := m.View()
	if !strings.Contains(full, "/tmp/outer/inner") {
		t.Fatalf("full path expected by default, got:\n%s", full)
	}

	m.dirNameOnly = true
	name := m.View()
	if strings.Contains(name, "outer") {
		t.Errorf("dir-name mode should not show parent segments, got:\n%s", name)
	}
	if !strings.Contains(name, "inner") {
		t.Errorf("dir-name mode should show the final segment, got:\n%s", name)
	}
}

func TestColumnIcons(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{ID: "a", Title: "x", CWD: "/tmp/proj", Branch: "main", Modified: now},
		{ID: "b", Title: "y", CWD: "/tmp/proj", Modified: now}, // no branch
	}
	m := testModel(previewColumn, sessions)
	m.dirIcon = "D"
	m.branchIcon = "B"
	out := m.View()

	if !strings.Contains(out, "D /tmp/proj") {
		t.Errorf("dir icon should precede the path, got:\n%s", out)
	}
	if !strings.Contains(out, "B main") {
		t.Errorf("branch icon should precede the branch, got:\n%s", out)
	}
	// The icon slot is still reserved when the value is empty (alignment),
	// but the icon itself is not drawn for the branchless session.
	if got := m.iconCell("B", "", colBranch, m.styles.branch, true); strings.Contains(got, "B") {
		t.Errorf("empty branch should not draw its icon, got %q", got)
	}
	if lipgloss.Width(m.iconCell("B", "main", colBranch, m.styles.branch, true)) !=
		lipgloss.Width(m.iconCell("B", "", colBranch, m.styles.branch, true)) {
		t.Errorf("icon cell width should match whether or not the value is empty")
	}
}

func TestSelectionColorsKeepStyle(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor) // force colour: test output isn't a TTY
	defer lipgloss.SetColorProfile(prev)

	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Selection.Colors = true
	m := newModel(cfg)
	now := time.Now()
	m.all = []Session{{ID: "r", Title: "busy", PID: 1, State: StateRunning, Modified: now, Activity: now}}
	m.sessions = m.all
	m.width, m.height = 160, 10

	normal := m.renderRow(0, true, lipgloss.NewStyle(), false)                // unselected row
	hi := m.renderRow(0, true, lipgloss.NewStyle().Background(m.selBG), true) // coloured cursor row

	// The cursor row keeps colours (unlike reverse video, which drops them):
	// it still carries ANSI styling and differs from the normal row only by
	// the added background highlight.
	if !strings.Contains(normal, "\x1b[") {
		t.Fatalf("coloured row should carry ANSI styling, got %q", normal)
	}
	if hi == normal {
		t.Errorf("highlighted row should differ from normal (background)")
	}
	if !strings.Contains(hi, "48;") { // a background-colour SGR introducer
		t.Errorf("highlighted row should set a background, got %q", hi)
	}
}

func TestReverseSelectionKeepsBold(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	m := newModel(cfg) // running style is bold; selected style is reverse
	now := time.Now()
	m.all = []Session{{ID: "r", Title: "busy", PID: 1, State: StateRunning, Modified: now, Activity: now}}
	m.sessions = m.all
	m.width, m.height = 120, 10

	// The default reverse-video cursor row: colours dropped, but the running
	// session's bold survives (bold and reverse are independent SGR attributes).
	rev := m.renderRow(0, false, m.styles.selected, true)
	if !strings.Contains(rev, "7") { // reverse attribute present
		t.Errorf("reverse row should carry the reverse attribute, got %q", rev)
	}
	if !boldAndReverse(rev) {
		t.Errorf("running session should stay bold under reverse video, got %q", rev)
	}
}

// boldAndReverse reports whether the string contains an SGR sequence enabling
// both bold (1) and reverse (7), in either order, e.g. "\x1b[1;7m".
func boldAndReverse(s string) bool {
	return strings.Contains(s, "1;7") || strings.Contains(s, "7;1")
}

func TestReverseStatusColor(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sess := []Session{{ID: "i", Title: "idle", PID: 1, State: StateIdle, Modified: now, Activity: now}}
	const cyanBg = "46" // [styles.idle] fg = "6" put on the bg channel -> ANSI bg cyan

	// Off: the idle marker is inverted with the rest of the reverse bar, no
	// colour applied at all.
	off := newModel(cfg)
	off.all, off.sessions = sess, sess
	off.width, off.height = 80, 8
	if strings.Contains(off.renderRow(0, false, off.styles.selected, true), cyanBg) {
		t.Errorf("without statuscolor the idle marker should be plain reverse, not coloured")
	}

	// On: the status colour goes on the background channel with reverse, so the
	// terminal's swap renders it as coloured *text* on the bar's own background
	// ("7;46") — same background as the rest of the row, which stays plain
	// reverse.
	cfg.Selection.StatusColor = true
	on := newModel(cfg)
	on.all, on.sessions = sess, sess
	on.width, on.height = 80, 8
	out := on.renderRow(0, false, on.styles.selected, true)
	if !strings.Contains(out, "7;"+cyanBg) {
		t.Errorf("statuscolor should render the idle marker as reversed bg colour, got %q", out)
	}
	if !strings.Contains(out, "\x1b[7m") {
		t.Errorf("the rest of the row should still be plain reverse, got %q", out)
	}

	// An override replaces the idle colour just for the reversed row.
	cfg.Selection.StatusColors.Idle = "4" // blue -> reversed bg "44"
	ov := newModel(cfg)
	ov.all, ov.sessions = sess, sess
	ov.width, ov.height = 80, 8
	out = ov.renderRow(0, false, ov.styles.selected, true)
	if strings.Contains(out, "7;"+cyanBg) {
		t.Errorf("override should replace the default idle colour, got %q", out)
	}
	if !strings.Contains(out, "7;44") {
		t.Errorf("override should apply the configured colour (blue bg 44), got %q", out)
	}
}

func TestCursorHiddenRevealsOnKeyAndFocus(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{{ID: "x", Title: "s", PID: 1, State: StateIdle, Modified: time.Now()}}

	m.cursorHidden = true
	afterKey, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if afterKey.(model).cursorHidden {
		t.Error("a keystroke should reveal the cursor again")
	}

	m.cursorHidden = true
	afterFocus, _ := m.Update(tea.FocusMsg{})
	if afterFocus.(model).cursorHidden {
		t.Error("regaining focus should reveal the cursor")
	}
}

func TestEnterHidesCursor(t *testing.T) {
	m := glyphModel()
	m.commands = map[string]string{"enter": "true"} // no placeholders, so it launches for any session
	m.sessions = []Session{{ID: "x", Title: "s", PID: 1, State: StateIdle, Modified: time.Now()}}
	m.cursor = 0
	after, _ := m.runCommand(m.commands["enter"])
	if !after.(model).cursorHidden {
		t.Error("running a command should hide the cursor highlight")
	}
}

func TestTmuxGlyph(t *testing.T) {
	now := time.Now()
	sessions := []Session{
		{ID: "a", Title: "attached", LastMsg: "hi", Modified: now, PID: 100, Pane: "%27"},
		{ID: "b", Title: "loose", LastMsg: "hi", Modified: now, PID: 200}, // live, no pane
		{ID: "c", Title: "dead", LastMsg: "hi", Modified: now},            // not live
	}
	m := testModel(previewColumn, sessions) // column mode: one line per session
	m.tmuxGlyph = "⊟"
	out := m.View()
	if !strings.Contains(out, "⊟") {
		t.Errorf("attachable session should show the glyph, got:\n%s", out)
	}
	// A live-but-loose session and a dead one must not be marked.
	if strings.Count(out, "⊟") != 1 {
		t.Errorf("exactly one session should be marked, got %d:\n%s", strings.Count(out, "⊟"), out)
	}
	// Rows stay column-aligned: the blank slot is the glyph's display width.
	if got := m.tmuxCell(sessions[1]); got != "   " { // "  " + one space
		t.Errorf("loose session cell = %q, want three spaces", got)
	}
}

func TestTmuxGlyphDisabled(t *testing.T) {
	now := time.Now()
	sessions := []Session{{ID: "a", Title: "x", Modified: now, PID: 100, Pane: "%1"}}
	m := testModel(previewColumn, sessions)
	m.tmuxGlyph = ""
	if got := m.tmuxCell(sessions[0]); got != "" {
		t.Errorf("disabled glyph should yield no slot, got %q", got)
	}
}

func glyphModel() model {
	m := model{
		glyphs: map[marker]string{
			markerRunning: spinnerSentinel,
			markerWaiting: "!",
			markerIdle:    "·",
			markerUnread:  "●",
			markerOffline: " ",
		},
		showWords: true,
		unread:    map[string]bool{},
		seen:      map[string]SessionState{},
		width:     160,
		height:    40,
	}
	m.colGlyph = glyphWidth(m.glyphs)
	return m
}

func TestIconOnlyMode(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{{ID: "a", Title: "x", PID: 1, State: StateIdle, Modified: time.Now()}}

	withWords := m.View()
	if !strings.Contains(withWords, "idle") {
		t.Fatalf("expected state word with showWords=true, got:\n%s", withWords)
	}

	m.showWords = false
	iconOnly := m.View()
	if strings.Contains(iconOnly, "idle") {
		t.Errorf("state word should be hidden with showWords=false, got:\n%s", iconOnly)
	}
	// The glyph still shows.
	if !strings.Contains(iconOnly, "·") {
		t.Errorf("glyph should still render in icon-only mode, got:\n%s", iconOnly)
	}
}

func TestDetectUnreadTransition(t *testing.T) {
	m := glyphModel()
	// First observation: running.
	m.detectUnread([]Session{{ID: "x", PID: 1, State: StateRunning}})
	if m.unread["x"] {
		t.Fatal("running session should not be unread")
	}
	// Turn finishes -> idle: now unread.
	m.detectUnread([]Session{{ID: "x", PID: 1, State: StateIdle}})
	if !m.unread["x"] {
		t.Fatal("running->idle should mark unread")
	}
	// A fresh turn clears it.
	m.detectUnread([]Session{{ID: "x", PID: 1, State: StateRunning}})
	if m.unread["x"] {
		t.Fatal("new turn should clear unread")
	}
}

func TestUnreadNeedsPriorRunning(t *testing.T) {
	m := glyphModel()
	// A session first seen as idle (we never saw it run) is not unread.
	m.detectUnread([]Session{{ID: "y", PID: 1, State: StateIdle}})
	if m.unread["y"] {
		t.Fatal("idle without a prior running observation should not be unread")
	}
}

func TestUnreadDroppedWhenGone(t *testing.T) {
	m := glyphModel()
	m.detectUnread([]Session{{ID: "z", PID: 1, State: StateRunning}})
	m.detectUnread([]Session{{ID: "z", PID: 1, State: StateIdle}})
	m.detectUnread(nil) // session disappeared
	if m.unread["z"] {
		t.Fatal("vanished session should be forgotten")
	}
}

func TestMarkerAndGlyphRender(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{
		{ID: "u", Title: "finished", PID: 1, State: StateIdle, Modified: time.Now()},
	}
	m.unread["u"] = true
	if got := m.markerFor(m.sessions[0]); got != markerUnread {
		t.Fatalf("unread idle session marker = %v, want markerUnread", got)
	}
	out := m.View()
	if !strings.Contains(out, "●") {
		t.Errorf("unread glyph ● should render, got:\n%s", out)
	}
}

func TestSpinnerFrameAdvances(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{{ID: "r", PID: 1, State: StateRunning, Modified: time.Now()}}
	a := m.statusCell(markerRunning)
	m.spin++
	b := m.statusCell(markerRunning)
	if a == b {
		t.Errorf("spinner frame should change with m.spin: %q == %q", a, b)
	}
	if !m.anyRunning() {
		t.Error("anyRunning should be true with a running session")
	}
}

func TestTmuxPaneForCWD(t *testing.T) {
	panes := map[string]string{
		"/home/chris/code/foo": "%5",
		"/home/chris/code/bar": "%6",
	}
	if p, ok := tmuxPaneForCWD("/home/chris/code/foo", panes); !ok || p != "%5" {
		t.Errorf("matching cwd: got %q ok=%v, want %%5", p, ok)
	}
	if _, ok := tmuxPaneForCWD("/nope", panes); ok {
		t.Error("non-matching cwd should return ok=false")
	}
	if _, ok := tmuxPaneForCWD("", panes); ok {
		t.Error("empty cwd should not match")
	}
}

// TestEnterPaneWithoutLive checks the guard decoupling: a pi session that is
// NOT live (no PID -- pi has no registry) but IS in a tmux pane (found by cwd
// match) can still run a {pane}-based enter command. Previously {pane} hard-
// required Live(), which blocked every pi session.
func TestEnterPaneWithoutLive(t *testing.T) {
	m := glyphModel()
	// Pane set, PID 0 -> not Live, but InTmux. A {pane} template must still
	// run (substituting the pane) rather than be hard-blocked for lacking a PID.
	m.sessions = []Session{{ID: "x", Title: "s", CWD: "/p", Pane: "%9", Modified: time.Now()}}
	m.cursor = 0

	after, cmd := m.runCommand("tmux select-pane -t {pane}")
	mm := after.(model)
	if mm.notice != "" {
		t.Errorf("non-live but in-tmux session should run, got notice %q", mm.notice)
	}
	if cmd == nil {
		t.Error("expected a command to be issued for an in-tmux non-live session")
	}
}

// TestEnterPidBlockedWithoutLive checks that {pid} still requires a live
// session (a PID of 0 is meaningless to substitute), so pi sessions -- which
// have no PID -- can't use a {pid} template.
func TestEnterPidBlockedWithoutLive(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{{ID: "x", Title: "s", Pane: "%9", Modified: time.Now()}} // PID 0
	m.cursor = 0
	after, _ := m.runCommand("kill {pid}")
	if after.(model).notice == "" {
		t.Error("a {pid} template on a non-live session should be blocked with a notice")
	}
}

// TestDefaultConfigPiEnterOverride verifies the shipped default config
// provides a [sources.pi]enter override and that newModel picks it up.
func TestDefaultConfigPiEnterOverride(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Sources.Pi.Enter, "pi --session") {
		t.Errorf("default [sources.pi]enter should resume pi, got: %s", cfg.Sources.Pi.Enter)
	}
	if strings.Contains(cfg.Sources.Pi.Enter, "claude --resume") {
		t.Errorf("default [sources.pi]enter should not mention claude --resume: %s", cfg.Sources.Pi.Enter)
	}
	m := newModel(cfg)
	if got := m.enterBySource["pi"]; got != cfg.Sources.Pi.Enter {
		t.Errorf("newModel did not populate enterBySource[pi], got %q", got)
	}
}

// TestSourceEnterTemplate checks that per-source overrides win, unknown
// sources keep the global template, and pi sessions without an override fall
// back to the pi-specific built-in instead of the global claude template.
func TestSourceEnterTemplate(t *testing.T) {
	overrides := map[string]string{"pi": "pi-override"}
	if got := sourceEnterTemplate("pi", "global", overrides); got != "pi-override" {
		t.Errorf("per-source override should win, got %q", got)
	}
	if got := sourceEnterTemplate("claude", "global", overrides); got != "global" {
		t.Errorf("claude should keep global template, got %q", got)
	}
	if got := sourceEnterTemplate("pi", "global", map[string]string{}); got != piEnterBuiltin {
		t.Errorf("pi without override should use built-in fallback, got %q", got)
	}
}

// TestPiEnterBuiltinNoClaudeResume renders the pi fallback for a session
// with no tmux pane and confirms it does not silently run claude --resume.
func TestPiEnterBuiltinNoClaudeResume(t *testing.T) {
	vars := map[string]string{
		"id":    "pi-uuid",
		"cwd":   "/tmp/proj",
		"pane":  "",
		"pane?": "",
		"pid":   "0",
		"pid?":  "",
		"file":  "/tmp/proj/pi-uuid.jsonl",
		"state": string(StateIdle),
	}
	line := expandCommand(sourceEnterTemplate("pi", "global", map[string]string{}), vars)
	if strings.Contains(line, "claude --resume") {
		t.Errorf("pi enter rendered with claude --resume: %s", line)
	}
	if !strings.Contains(line, "pi --session") {
		t.Errorf("pi enter should resume pi, got: %s", line)
	}
}

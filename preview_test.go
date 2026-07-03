package main

import (
	"strings"
	"testing"
	"time"
)

func testModel(mode previewMode, sessions []Session) model {
	m := model{
		previewMode:   mode,
		previewRecent: 5,
		previewWithin: 20 * time.Minute,
		sessions:      sessions,
		width:         160,
		height:        40, // tall enough to show every row
	}
	m.clampOffset()
	return m
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
	sessions, err := newLoader().Load()
	if err != nil || len(sessions) == 0 {
		t.Skip("no real sessions available")
	}
	m := testModel(previewRow, sessions)
	m.height = 40
	m.clampOffset()
	out := m.View()
	if !strings.Contains(out, "↳ ") {
		t.Errorf("expected at least one preview line from real data")
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
		unread: map[string]bool{},
		seen:   map[string]SessionState{},
		width:  160,
		height: 40,
	}
	m.colGlyph = glyphWidth(m.glyphs)
	return m
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

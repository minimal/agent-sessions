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

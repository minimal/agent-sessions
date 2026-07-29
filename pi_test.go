package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPiLiveMarker(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}

	a := newPiAdapter(sessionsDir)

	// Two pi sessions; only sid-1 has a marker. Use the real test PID so the
	// live-liveness check (pidAlive) considers the marker current.
	sessions := []Session{
		{ID: "sid-1", Source: "pi", CWD: "/home/chris/code/foo"},
		{ID: "sid-2", Source: "pi", CWD: "/home/chris/code/bar"},
	}

	marker := piLiveMarker{
		SID:       "sid-1",
		PID:       os.Getpid(),
		Pane:      "%5",
		CWD:       "/home/chris/code/foo",
		Status:    "running",
		UpdatedAt: time.Now(),
	}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(filepath.Join(liveDir, "sid-1.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	a.Live(sessions)

	if !sessions[0].Live() {
		t.Errorf("sid-1 should be live")
	}
	if sessions[0].PID != os.Getpid() {
		t.Errorf("sid-1 PID = %d, want %d", sessions[0].PID, os.Getpid())
	}
	if sessions[0].Pane != "%5" {
		t.Errorf("sid-1 Pane = %q, want %%5", sessions[0].Pane)
	}
	if sessions[0].State != StateRunning {
		t.Errorf("sid-1 State = %q, want running", sessions[0].State)
	}

	if sessions[1].Live() {
		t.Errorf("sid-2 should not be live")
	}
	if sessions[1].Pane != "" {
		t.Errorf("sid-2 Pane should be empty when there is no cwd match and no tmux")
	}
}

func TestPiLiveMarkerFallsBackToTimestampUUID(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}

	a := newPiAdapter(sessionsDir)

	// Go id is "<timestamp>_<uuid>" but the extension writes the uuid only.
	sessions := []Session{{ID: "20260101_abc123", Source: "pi", CWD: "/p"}}
	marker := piLiveMarker{SID: "abc123", PID: os.Getpid(), Status: "idle", UpdatedAt: time.Now()}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(filepath.Join(liveDir, "abc123.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	a.Live(sessions)

	if !sessions[0].Live() || sessions[0].PID != os.Getpid() {
		t.Errorf("marker should match by uuid suffix: got PID=%d live=%v", sessions[0].PID, sessions[0].Live())
	}
}

func TestPiLiveMarkerStaleCleanup(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}

	a := newPiAdapter(sessionsDir)

	stale := piLiveMarker{
		SID:       "orphan",
		PID:       1,
		Status:    "running",
		UpdatedAt: time.Now().Add(-2 * time.Hour),
	}
	data, _ := json.Marshal(stale)
	stalePath := filepath.Join(liveDir, "orphan.json")
	if err := os.WriteFile(stalePath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// No sessions at all -> orphan is stale and should be removed.
	a.Live(nil)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale orphan marker should have been deleted")
	}
}

func TestPiLiveMarkerDeadPID(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}
	a := newPiAdapter(sessionsDir)

	// pidAlive(0) and dead PIDs return false; the live self-pid is true.
	if pidAlive(0) {
		t.Error("pid 0 should be considered dead")
	}
	if !pidAlive(os.Getpid()) {
		t.Errorf("current pid %d should be alive", os.Getpid())
	}

	// 1<<30 is well beyond any reasonable pid_max (default 4194304), so
	// signal 0 against it is guaranteed to fail -> pidAlive is false. This
	// exercises the "PID is no longer alive" cleanup path deterministically
	// (no spawn/reap, no PID-reuse race).
	deadPID := 1 << 30
	marker := piLiveMarker{
		SID:       "dead",
		PID:       deadPID,
		Status:    "idle",
		UpdatedAt: time.Now(),
	}
	data, _ := json.Marshal(marker)
	markerPath := filepath.Join(liveDir, "dead.json")
	if err := os.WriteFile(markerPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	sessions := []Session{{ID: "dead", Source: "pi", CWD: "/p"}}
	a.Live(sessions)

	if sessions[0].Live() {
		t.Errorf("session with dead-PID marker should not be live (PID=%d, State=%q)", sessions[0].PID, sessions[0].State)
	}
	if sessions[0].State == StateIdle {
		t.Errorf("session with dead-PID marker must not inherit status=idle")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("dead-PID marker should be cleaned up; stat err = %v", err)
	}
}

func TestPiLiveMarkerPreservesYoungOrphan(t *testing.T) {
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}

	a := newPiAdapter(sessionsDir)

	orphan := piLiveMarker{
		SID:       "fresh",
		PID:       1,
		Status:    "running",
		UpdatedAt: time.Now().Add(-5 * time.Minute),
	}
	data, _ := json.Marshal(orphan)
	orphanPath := filepath.Join(liveDir, "fresh.json")
	if err := os.WriteFile(orphanPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// No matching session, but marker is young -> preserve.
	a.Live(nil)

	if _, err := os.Stat(orphanPath); os.IsNotExist(err) {
		t.Errorf("young orphan marker should be preserved")
	}
}

func TestPiIDVariants(t *testing.T) {
	if got := piIDVariants("abc"); len(got) != 1 || got[0] != "abc" {
		t.Errorf("plain id variants = %v", got)
	}
	got := piIDVariants("2026_abc")
	if len(got) != 2 || got[0] != "2026_abc" || got[1] != "abc" {
		t.Errorf("timestamp_uuid variants = %v", got)
	}
}

func TestParsePiMarkerStatus(t *testing.T) {
	cases := map[string]SessionState{
		"running": StateRunning,
		"waiting": StateWaiting,
		"idle":    StateIdle,
		"":        StateUnknown,
		"boom":    StateUnknown,
	}
	for in, want := range cases {
		if got := parsePiMarkerStatus(in); got != want {
			t.Errorf("parsePiMarkerStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPiModelTracking(t *testing.T) {
	var s Session
	absorbPi(&s, piLine{Type: "model_change", ModelID: "claude-haiku-4.5"})
	if s.Model != "claude-haiku-4.5" {
		t.Fatalf("model_change Model = %q", s.Model)
	}

	absorbPi(&s, piLine{
		Type: "message",
		Message: &piMessage{
			Role: "assistant", Model: "claude-sonnet-4.6",
		},
	})
	if s.Model != "claude-sonnet-4.6" {
		t.Fatalf("assistant model Model = %q", s.Model)
	}

	absorbPi(&s, piLine{
		Type: "message",
		Message: &piMessage{
			Role: "assistant", ModelID: "claude-opus-4.8",
		},
	})
	if s.Model != "claude-opus-4.8" {
		t.Fatalf("assistant modelId Model = %q", s.Model)
	}

	absorbPi(&s, piLine{Type: "message", Message: &piMessage{Role: "assistant"}})
	if s.Model != "claude-opus-4.8" {
		t.Errorf("empty assistant model must not erase Model, got %q", s.Model)
	}
	if !s.Activity.IsZero() {
		t.Errorf("model-only events must not advance Activity, got %v", s.Activity)
	}
}

func TestPiLiveMarkerISO8601Timestamp(t *testing.T) {
	// The extension writes new Date().toISOString(). Ensure Go parses it.
	jsonBlob := []byte(`{"sid":"x","pid":1,"status":"running","updated_at":"2026-07-05T12:34:56.789Z"}`)
	var m piLiveMarker
	if err := json.Unmarshal(jsonBlob, &m); err != nil {
		t.Fatalf("unmarshal ISO8601 marker: %v", err)
	}
	if m.UpdatedAt.IsZero() || m.UpdatedAt.Year() != 2026 {
		t.Errorf("parsed UpdatedAt = %v", m.UpdatedAt)
	}
}

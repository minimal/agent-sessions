package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeRegistry drops a ~/.claude/sessions/<pid>.json file for this test
// process, so liveStates sees a pid that is genuinely alive and whose start
// time matches.
func writeRegistry(t *testing.T, home string, extra map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	rec := map[string]any{"pid": pid, "startedAt": procStartTime(pid)}
	for k, v := range extra {
		rec[k] = v
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%d.%v.json", pid, extra["sessionId"])
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLiveStatesSkipsSpare(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{
		"sessionId": "real", "status": "idle", "kind": "bg", "jobId": "abc1",
	})
	writeRegistry(t, home, map[string]any{
		"sessionId": "warm", "status": "idle", "kind": "bg", "jobId": "def2", "spare": true,
	})

	live := liveStates()
	if _, ok := live["real"]; !ok {
		t.Error("a real background session should be listed")
	}
	if _, ok := live["warm"]; ok {
		t.Error("a pre-warmed bg-spare process should not be listed")
	}
}

func TestLiveStatesReadsRegistryPane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRegistry(t, home, map[string]any{
		"sessionId": "inter", "status": "busy", "kind": "interactive",
		"tmux": "dotfiles:@57.%58",
	})
	writeRegistry(t, home, map[string]any{
		"sessionId": "job", "status": "idle", "kind": "bg", "jobId": "abc1",
	})

	live := liveStates()
	if got := live["inter"].Pane; got != "dotfiles:@57.%58" {
		t.Errorf("interactive pane: got %q, want the registry tmux target", got)
	}
	if got := live["inter"].State; got != StateRunning {
		t.Errorf("interactive state: got %q, want %q", got, StateRunning)
	}
	if live["inter"].Background {
		t.Error("an interactive session should not be flagged Background")
	}
	if got := live["job"].Pane; got != "" {
		t.Errorf("a background job has no pane, got %q", got)
	}
	if !live["job"].Background || live["job"].JobID != "abc1" {
		t.Errorf("background job: got %+v", live["job"])
	}
}

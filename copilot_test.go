package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type copilotUsageRow struct {
	sessionID        string
	agentID          any
	inputTokens      any
	outputTokens     any
	cacheReadTokens  any
	cacheWriteTokens any
}

func writeCopilotUsageDB(t *testing.T, path string, rows []copilotUsageRow) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE assistant_usage_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			agent_id TEXT,
			input_tokens INTEGER,
			output_tokens INTEGER,
			cache_read_tokens INTEGER,
			cache_write_tokens INTEGER
		)
	`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		_, err := db.Exec(`
			INSERT INTO assistant_usage_events (
				session_id, agent_id, input_tokens, output_tokens,
				cache_read_tokens, cache_write_tokens
			) VALUES (?, ?, ?, ?, ?, ?)
		`, row.sessionID, row.agentID, row.inputTokens, row.outputTokens,
			row.cacheReadTokens, row.cacheWriteTokens)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// writeCopilotSession creates a session-state dir <root>/<sid>/ with the given
// events.jsonl lines and (optionally) a workspace.yaml, returning nothing. It
// mirrors the on-disk layout the Copilot CLI produces.
func writeCopilotSession(t *testing.T, root, sid string, events []string, workspace string) {
	t.Helper()
	dir := filepath.Join(root, sid)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var body string
	for _, e := range events {
		body += e + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if workspace != "" {
		if err := os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(workspace), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCopilotSessionsParse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	sid := "935ca4e7-60b5-4632-a109-4d242467eab1"
	writeCopilotSession(t, root, sid, []string{
		`{"type":"session.start","timestamp":"2026-07-02T15:03:27.361Z","data":{"sessionId":"935ca4e7-60b5-4632-a109-4d242467eab1","context":{"cwd":"/home/chris/code/foo","branch":"my-branch"}}}`,
		`{"type":"user.message","timestamp":"2026-07-02T15:04:00.000Z","data":{"content":"Do the thing\nsecond line"}}`,
		`{"type":"assistant.message","timestamp":"2026-07-02T15:04:10.000Z","data":{"model":"gpt-5.4","content":"","toolRequests":[{"name":"shell"}]}}`,
		`{"type":"assistant.message","timestamp":"2026-07-02T15:04:20.000Z","data":{"model":"gpt-5.5","content":"Here is the answer"}}`,
		`{"type":"assistant.message","timestamp":"2026-07-02T15:04:21.000Z","data":{"content":""}}`,
		// A late hook event must NOT advance Activity (hooks fire constantly).
		`{"type":"hook.start","timestamp":"2026-07-02T18:00:00.000Z","data":{}}`,
	}, "id: 935ca4e7-60b5-4632-a109-4d242467eab1\nname: My Session Title\nuser_named: false\n")

	sessions, err := newCopilotAdapter(root).Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.Source != "copilot" {
		t.Errorf("Source = %q, want copilot", s.Source)
	}
	if s.ID != sid {
		t.Errorf("ID = %q, want %q (the session-state dir name)", s.ID, sid)
	}
	if s.CWD != "/home/chris/code/foo" {
		t.Errorf("CWD = %q", s.CWD)
	}
	// Not a real git repo, so gitBranch("") fails and we fall back to the branch
	// recorded in session.start.
	if s.Branch != "my-branch" {
		t.Errorf("Branch = %q, want my-branch (session.start fallback)", s.Branch)
	}
	if s.Title != "My Session Title" {
		t.Errorf("Title = %q, want the workspace.yaml name", s.Title)
	}
	if s.LastMsg != "Here is the answer" {
		t.Errorf("LastMsg = %q, want the last non-empty assistant text", s.LastMsg)
	}
	if s.Model != "gpt-5.5" {
		t.Errorf("Model = %q, want the last non-empty assistant model", s.Model)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-02T15:04:21.000Z")
	if !s.Activity.Equal(want) {
		t.Errorf("Activity = %v, want %v (hook.start must be ignored)", s.Activity, want)
	}
}

func TestCopilotTitleFallsBackToPrompt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// No workspace.yaml -> Title comes from the first user prompt's first line.
	writeCopilotSession(t, root, sid, []string{
		`{"type":"session.start","timestamp":"2026-07-02T15:03:27.361Z","data":{"sessionId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","context":{"cwd":"/tmp/x"}}}`,
		`{"type":"user.message","timestamp":"2026-07-02T15:04:00.000Z","data":{"content":"Fix the parser\nand add tests"}}`,
	}, "")

	sessions, err := newCopilotAdapter(root).Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Title != "Fix the parser" {
		t.Errorf("Title = %q, want the first line of the first user prompt", sessions[0].Title)
	}
}

func TestCopilotPathsRespectCopilotHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COPILOT_HOME", home)
	adapter := newCopilotAdapter("")
	if got, want := adapter.root(), filepath.Join(home, "session-state"); got != want {
		t.Errorf("root() = %q, want %q", got, want)
	}
	if got, want := adapter.usageDB, filepath.Join(home, "session-store.db"); got != want {
		t.Errorf("usageDB = %q, want %q", got, want)
	}
}

func TestCopilotSessionsLoadLatestRootUsage(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "session-state")
	for _, sid := range []string{"session-a", "session-b", "session-without-usage"} {
		writeCopilotSession(t, root, sid, []string{
			`{"type":"session.start","timestamp":"2026-07-02T15:03:27.361Z","data":{"sessionId":"` + sid + `","context":{"cwd":"/tmp"}}}`,
		}, "")
	}

	usageDB := filepath.Join(tmp, "session-store.db")
	writeCopilotUsageDB(t, usageDB, []copilotUsageRow{
		{sessionID: "session-a", inputTokens: 10, outputTokens: 1},
		{sessionID: "session-a", inputTokens: 100, outputTokens: 10, cacheReadTokens: 40, cacheWriteTokens: 50},
		{sessionID: "session-a", agentID: "subagent-1", inputTokens: 900, outputTokens: 90},
		{sessionID: "session-b", agentID: "subagent-2", inputTokens: 800, outputTokens: 80},
		{sessionID: "session-b", inputTokens: 200, outputTokens: 20, cacheReadTokens: 60, cacheWriteTokens: 70},
		// Regression for the "ctx includes output tokens" bug: cache_read /
		// cache_write columns must never contribute to CtxTokens. Copilot folds
		// cached input into input_tokens itself (per copilotContextTokens),
		// and the ctx column is meant to be the context-window occupancy
		// (input-side), matching Claude. If a future change re-adds these
		// columns, the expected 100 / 200 below will surface it.
		{sessionID: "unloaded-session", inputTokens: 700, outputTokens: 70},
	})

	adapter := newCopilotAdapter(root)
	adapter.usageDB = usageDB
	sessions, err := adapter.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int, len(sessions))
	for _, session := range sessions {
		got[session.ID] = session.CtxTokens
	}
	want := map[string]int{
		"session-a":             100,
		"session-b":             200,
		"session-without-usage": 0,
	}
	for sid, wantTokens := range want {
		if got[sid] != wantTokens {
			t.Errorf("%s CtxTokens = %d, want %d", sid, got[sid], wantTokens)
		}
	}
}

func TestCopilotSessionsFailsOpenOnCorruptUsageDB(t *testing.T) {
	// Regression: a usage-DB read failure (any failure, not just missing
	// file/table) must not blank the entire session list. A live Copilot write
	// that holds the -wal lock, a corrupt DB, or a permission error all hit
	// this path; multiLoader.Load() used to propagate the first adapter error
	// and discard the already-loaded Claude/pi sessions, so the TUI showed
	// "Error: ..." and rendered nothing.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "session-state")
	writeCopilotSession(t, root, "session-a", []string{
		`{"type":"session.start","timestamp":"2026-07-02T15:03:27.361Z","data":{"sessionId":"session-a","context":{"cwd":"/tmp"}}}`,
	}, "")

	// A file that is a valid SQLite header tail but the body is garbage, so
	// opening it for a query fails (not a missing-file or missing-table
	// failure, both of which were already tolerated).
	usageDB := filepath.Join(tmp, "session-store.db")
	if err := os.WriteFile(usageDB, []byte("not a real sqlite database"), 0644); err != nil {
		t.Fatal(err)
	}

	adapter := newCopilotAdapter(root)
	adapter.usageDB = usageDB
	sessions, err := adapter.Sessions()
	if err != nil {
		t.Fatalf("Sessions() should fail open on a corrupt usage DB, got %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (the copilot session must still load)", len(sessions))
	}
	if sessions[0].CtxTokens != 0 {
		t.Errorf("CtxTokens = %d, want 0 (fail-open leaves the ctx column blank)", sessions[0].CtxTokens)
	}
}

func TestCopilotSessionsWithoutUsageSchema(t *testing.T) {
	for _, tc := range []struct {
		name  string
		oldDB bool
	}{
		{name: "missing database"},
		{name: "database from older Copilot", oldDB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "session-state")
			writeCopilotSession(t, root, "session-a", []string{
				`{"type":"session.start","timestamp":"2026-07-02T15:03:27.361Z","data":{"sessionId":"session-a","context":{"cwd":"/tmp"}}}`,
			}, "")

			usageDB := filepath.Join(tmp, "session-store.db")
			if tc.oldDB {
				db, err := sql.Open("sqlite", usageDB)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}

			adapter := newCopilotAdapter(root)
			adapter.usageDB = usageDB
			sessions, err := adapter.Sessions()
			if err != nil {
				t.Fatalf("Sessions() should tolerate unavailable usage data: %v", err)
			}
			if len(sessions) != 1 {
				t.Fatalf("got %d sessions, want 1", len(sessions))
			}
			if sessions[0].CtxTokens != 0 {
				t.Errorf("CtxTokens = %d, want 0", sessions[0].CtxTokens)
			}
		})
	}
}

func TestDiscoverCopilotSkipsSessionsWithoutEvents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	// A freshly created session dir with no events.jsonl yet contributes nothing.
	if err := os.MkdirAll(filepath.Join(root, "empty-one"), 0755); err != nil {
		t.Fatal(err)
	}
	writeCopilotSession(t, root, "real-one", []string{
		`{"type":"session.start","timestamp":"2026-07-02T15:03:27.361Z","data":{"sessionId":"real-one","context":{"cwd":"/tmp/y"}}}`,
	}, "")

	paths, err := discoverCopilot(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("discoverCopilot returned %d paths, want 1 (dir without events.jsonl skipped)", len(paths))
	}
}

func TestDiscoverCopilotMissingRoot(t *testing.T) {
	// Copilot not installed: an absent root contributes nothing, not an error.
	paths, err := discoverCopilot(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("missing root should yield no paths, got %d", len(paths))
	}
}

func TestCopilotWorkspaceName(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"name: Plain Title\nuser_named: false\n": "Plain Title",
		"name: \"Quoted Title\"\n":               "Quoted Title",
		"name: 'Single Quoted'\n":                "Single Quoted",
		"id: x\ncwd: /p\n":                       "", // no name key
		"name:\n":                                "", // empty value
	}
	for content, want := range cases {
		if err := os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if got := copilotWorkspaceName(dir); got != want {
			t.Errorf("copilotWorkspaceName(%q) = %q, want %q", content, got, want)
		}
	}
	// Missing file -> "".
	if got := copilotWorkspaceName(filepath.Join(dir, "does-not-exist")); got != "" {
		t.Errorf("missing workspace.yaml should yield \"\", got %q", got)
	}
}

func TestCopilotLiveMarker(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "session-state")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}

	a := newCopilotAdapter(root)

	sessions := []Session{
		{ID: "sid-1", Source: "copilot", CWD: "/home/chris/code/foo"},
		{ID: "sid-2", Source: "copilot", CWD: "/home/chris/code/bar"},
	}

	marker := copilotLiveMarker{
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
		t.Errorf("sid-2 Pane should be empty with no cwd match and no tmux")
	}
}

func TestCopilotLiveMarkerDeadPID(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "session-state")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}
	a := newCopilotAdapter(root)

	// 1<<30 is well beyond any reasonable pid_max, so it is guaranteed dead.
	marker := copilotLiveMarker{SID: "dead", PID: 1 << 30, Status: "idle", UpdatedAt: time.Now()}
	data, _ := json.Marshal(marker)
	markerPath := filepath.Join(liveDir, "dead.json")
	if err := os.WriteFile(markerPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	sessions := []Session{{ID: "dead", Source: "copilot", CWD: "/p"}}
	a.Live(sessions)

	if sessions[0].Live() {
		t.Errorf("session with dead-PID marker should not be live")
	}
	if sessions[0].State == StateIdle {
		t.Errorf("session with dead-PID marker must not inherit status=idle")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("dead-PID marker should be cleaned up; stat err = %v", err)
	}
}

func TestCopilotLiveInUseLockWithoutMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	sid := "sid-without-hook"
	sessionDir := filepath.Join(root, sid)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(sessionDir, "inuse."+strconv.Itoa(os.Getpid())+".lock")
	if err := os.WriteFile(lock, nil, 0644); err != nil {
		t.Fatal(err)
	}

	sessions := []Session{{ID: sid, Source: "copilot", CWD: "/p"}}
	newCopilotAdapter(root).Live(sessions)

	if !sessions[0].Live() {
		t.Fatal("session with a live inuse lock should be live without a hook marker")
	}
	if sessions[0].PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", sessions[0].PID, os.Getpid())
	}
	if sessions[0].State != StateUnknown {
		t.Errorf("State = %q, want unknown without a hook status", sessions[0].State)
	}

	m := model{sessions: sessions}
	updated, _ := m.Update(key("d"))
	got := updated.(model)
	if got.deleting != nil {
		t.Error("delete confirmation should not open for an active Copilot session")
	}
	if got.notice != "Won't move a session with a running agent process to Trash." {
		t.Errorf("notice = %q", got.notice)
	}
}

func TestCopilotDeadInUseLockIsNotLive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	sid := "stale-lock"
	sessionDir := filepath.Join(root, sid)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "inuse.1073741824.lock"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	sessions := []Session{{ID: sid, Source: "copilot", CWD: "/p"}}
	newCopilotAdapter(root).Live(sessions)

	if sessions[0].Live() {
		t.Errorf("session with a dead-PID inuse lock should not be live (PID=%d)", sessions[0].PID)
	}
}

func TestCopilotLiveMarkerStaleOrphanCleanup(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "session-state")
	liveDir := filepath.Join(tmp, "live")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}
	a := newCopilotAdapter(root)

	// PID 1 is alive but there is no matching session; older than an hour -> orphan.
	stale := copilotLiveMarker{SID: "orphan", PID: 1, Status: "running", UpdatedAt: time.Now().Add(-2 * time.Hour)}
	data, _ := json.Marshal(stale)
	stalePath := filepath.Join(liveDir, "orphan.json")
	if err := os.WriteFile(stalePath, data, 0644); err != nil {
		t.Fatal(err)
	}
	// A young orphan is preserved (its session may not be loaded yet).
	fresh := copilotLiveMarker{SID: "fresh", PID: 1, Status: "running", UpdatedAt: time.Now().Add(-5 * time.Minute)}
	data, _ = json.Marshal(fresh)
	freshPath := filepath.Join(liveDir, "fresh.json")
	if err := os.WriteFile(freshPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	a.Live(nil)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale orphan marker should have been deleted")
	}
	if _, err := os.Stat(freshPath); os.IsNotExist(err) {
		t.Errorf("young orphan marker should be preserved")
	}
}

func TestParseCopilotMarkerStatus(t *testing.T) {
	cases := map[string]SessionState{
		"running": StateRunning,
		"waiting": StateWaiting,
		"idle":    StateIdle,
		"":        StateUnknown,
		"boom":    StateUnknown,
	}
	for in, want := range cases {
		if got := parseCopilotMarkerStatus(in); got != want {
			t.Errorf("parseCopilotMarkerStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

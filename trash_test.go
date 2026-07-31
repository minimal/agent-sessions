package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
)

func fallbackTrasher(dir string) trasher {
	return trasher{
		lookPath: func(string) (string, error) {
			return "", exec.ErrNotFound
		},
		run: func(string, ...string) error {
			return errors.New("system Trash command should not run")
		},
		fallbackDir: func() (string, error) {
			return dir, nil
		},
	}
}

func onlyTrashEntry(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("fallback Trash contains %d entries, want one directory", len(entries))
	}
	return filepath.Join(root, entries[0].Name())
}

func TestFallbackTrashMovesClaudeTranscriptAndSidecar(t *testing.T) {
	source := t.TempDir()
	file := filepath.Join(source, "session-id.jsonl")
	sidecar := filepath.Join(source, "session-id")
	if err := os.WriteFile(file, []byte("transcript"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sidecar, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "tool-result"), []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}

	paths, err := newClaudeAdapter().TrashPaths(Session{
		ID:     "session-id",
		File:   file,
		Source: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	trashRoot := filepath.Join(t.TempDir(), "trash")
	if err := fallbackTrasher(trashRoot).Put(paths...); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("transcript still exists after trashing: %v", err)
	}
	if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sidecar still exists after trashing: %v", err)
	}
	entry := onlyTrashEntry(t, trashRoot)
	if got, err := os.ReadFile(filepath.Join(entry, "session-id.jsonl")); err != nil || string(got) != "transcript" {
		t.Errorf("trashed transcript = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(entry, "session-id", "tool-result")); err != nil || string(got) != "result" {
		t.Errorf("trashed sidecar = %q, %v", got, err)
	}
}

func TestClaudeTrashRejectsSidecarParent(t *testing.T) {
	session := Session{
		File:   filepath.Join(t.TempDir(), ".jsonl"),
		Source: "claude",
	}
	if _, err := newClaudeAdapter().TrashPaths(session); err == nil {
		t.Fatal("Claude TrashPaths accepted an empty id whose sidecar would be the parent directory")
	}
}

func TestTrashCommandSelection(t *testing.T) {
	file := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var lookedUp []string
	var ranPath string
	var ranArgs []string
	tr := trasher{
		lookPath: func(name string) (string, error) {
			lookedUp = append(lookedUp, name)
			if name == "trash-put" {
				return "/test/trash-put", nil
			}
			return "", exec.ErrNotFound
		},
		run: func(path string, args ...string) error {
			ranPath = path
			ranArgs = append([]string{}, args...)
			return nil
		},
		fallbackDir: func() (string, error) {
			return "", errors.New("fallback should not run")
		},
	}

	if err := tr.Put(file); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lookedUp, []string{"trash", "trash-put"}) {
		t.Errorf("looked up %v", lookedUp)
	}
	if ranPath != "/test/trash-put" || !slices.Equal(ranArgs, []string{file}) {
		t.Errorf("ran %q with %v", ranPath, ranArgs)
	}
}

func TestCopilotTrashMovesWholeSessionDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	sid := "session-to-trash"
	sibling := "session-to-keep"
	writeCopilotSession(t, root, sid, []string{
		`{"type":"session.start","data":{"sessionId":"session-to-trash"}}`,
	}, "name: Trashed session\n")
	writeCopilotSession(t, root, sibling, []string{
		`{"type":"session.start","data":{"sessionId":"session-to-keep"}}`,
	}, "name: Kept session\n")
	if err := os.WriteFile(filepath.Join(root, sid, "session.db"), []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := newCopilotAdapter(root)
	sessions, err := adapter.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	var session Session
	for _, candidate := range sessions {
		if candidate.ID == sid {
			session = candidate
			break
		}
	}
	if session.ID == "" {
		t.Fatal("Copilot session was not discovered")
	}

	trashRoot := filepath.Join(t.TempDir(), "trash")
	loader := newMultiLoader([]Adapter{adapter}, nil)
	loader.trasher = fallbackTrasher(trashRoot)
	m := model{loader: loader, deleting: &trashConfirmation{session: session}}
	updated, _ := m.Update(key("y"))
	got := updated.(model)
	if got.notice != `Moved "Trashed session" to Trash.` {
		t.Errorf("notice = %q", got.notice)
	}

	if _, err := os.Stat(filepath.Join(root, sid)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Copilot session directory still exists after trashing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, sibling, "events.jsonl")); err != nil {
		t.Errorf("neighbouring Copilot session was affected: %v", err)
	}
	entry := onlyTrashEntry(t, trashRoot)
	if got, err := os.ReadFile(filepath.Join(entry, sid, "session.db")); err != nil || string(got) != "database" {
		t.Errorf("trashed Copilot sidecar = %q, %v", got, err)
	}
}

func TestCopilotTrashRejectsDirectoryOutsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	outside := filepath.Join(t.TempDir(), "outside")
	session := Session{
		ID:     "outside",
		File:   filepath.Join(outside, "events.jsonl"),
		Source: "copilot",
	}
	if _, err := newCopilotAdapter(root).TrashPaths(session); err == nil {
		t.Fatal("Copilot TrashPaths accepted a session directory outside its configured root")
	}
}

func trashConfirmationModel(t *testing.T, size, threshold int64) (model, string) {
	t.Helper()
	source := t.TempDir()
	file := filepath.Join(source, "session-id.jsonl")
	if err := os.WriteFile(file, []byte("transcript"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := Session{
		ID:     "session-id",
		File:   file,
		Title:  "Session",
		Size:   size,
		Source: "claude",
	}
	loader := newMultiLoader([]Adapter{newClaudeAdapter()}, nil)
	loader.trasher = fallbackTrasher(filepath.Join(t.TempDir(), "trash"))
	return model{
		loader:        loader,
		sessions:      []Session{session},
		quickTrashMax: threshold,
	}, file
}

func TestQuickTrashUsesYNConfirmation(t *testing.T) {
	m, file := trashConfirmationModel(t, 8192, 8192)

	updated, _ := m.Update(key("d"))
	got := updated.(model)
	if got.deleting == nil || got.deleting.typed {
		t.Fatal("session at the threshold should use the quick y/n confirmation")
	}
	updated, _ = got.Update(key("y"))
	got = updated.(model)
	if got.notice != `Moved "Session" to Trash.` {
		t.Errorf("notice = %q", got.notice)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("quick-confirmed session still exists: %v", err)
	}
}

func TestLargeTrashRequiresTypedYes(t *testing.T) {
	m, file := trashConfirmationModel(t, 8193, 8192)

	updated, _ := m.Update(key("d"))
	got := updated.(model)
	if got.deleting == nil || !got.deleting.typed {
		t.Fatal("session over the threshold should require typed confirmation")
	}
	updated, _ = got.Update(key("y"))
	got = updated.(model)
	if got.deleting == nil {
		t.Fatal("a single y should only edit the typed confirmation")
	}
	updated, _ = got.Update(key("enter"))
	got = updated.(model)
	if got.notice != `Trash cancelled: confirmation must be exactly "yes".` {
		t.Errorf("notice = %q", got.notice)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("session was trashed after confirming only y: %v", err)
	}

	updated, _ = got.Update(key("d"))
	got = updated.(model)
	updated, _ = got.Update(key("yes"))
	got = updated.(model)
	updated, _ = got.Update(key("enter"))
	got = updated.(model)
	if got.notice != `Moved "Session" to Trash.` {
		t.Errorf("notice = %q", got.notice)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session still exists after typing yes: %v", err)
	}
}

func TestTrashPromptsCapLongSubject(t *testing.T) {
	const width = 180
	longSubject := strings.Repeat("s", 200)
	for _, tc := range []struct {
		name string
		size int64
	}{
		{name: "quick", size: 8192},
		{name: "typed", size: 8193},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := trashConfirmationModel(t, tc.size, 8192)
			m.sessions[0].Title = longSubject
			m.width = width
			m.height = 3

			updated, _ := m.Update(key("d"))
			out := updated.(model).View()
			status := out[strings.LastIndex(out, "\n")+1:]
			if strings.Contains(status, longSubject) {
				t.Errorf("Trash prompt contains the uncapped subject: %q", status)
			}
			if !strings.Contains(status, "…") {
				t.Errorf("Trash prompt does not show a truncated subject: %q", status)
			}
			if got := lipgloss.Width(status); got > width {
				t.Errorf("Trash prompt width = %d, want at most %d", got, width)
			}
		})
	}
}

func TestQuickTrashThresholdDefault(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.QuickTrashThresholdBytes != 8192 {
		t.Errorf("QuickTrashThresholdBytes = %d, want 8192", cfg.QuickTrashThresholdBytes)
	}
}

func TestDisplaySizeMakesThresholdDifferenceVisible(t *testing.T) {
	if got := displaySize(8192); got != "8 KiB" {
		t.Errorf("displaySize(8192) = %q", got)
	}
	if got := displaySize(8193); got != "8.001 KiB" {
		t.Errorf("displaySize(8193) = %q", got)
	}
}

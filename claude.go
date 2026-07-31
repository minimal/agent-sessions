package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claudeDir returns the path of a directory under ~/.claude.
func claudeDir(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home, ".claude"}, elem...)...), nil
}

// transcriptLine covers the JSONL fields we care about across Claude Code
// transcript entry types.
type transcriptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model"`
}

type transcriptLine struct {
	Type      string             `json:"type"`
	Timestamp string             `json:"timestamp"`
	AITitle   string             `json:"aiTitle"`
	CWD       string             `json:"cwd"`
	GitBranch string             `json:"gitBranch"`
	Slug      string             `json:"slug"`
	IsMeta    bool               `json:"isMeta"`
	Message   *transcriptMessage `json:"message"`
}

// claudeAdapter reads Claude Code transcripts under ~/.claude/projects and the
// live-process registry under ~/.claude/sessions. It is the existing behaviour
// extracted behind the Adapter interface — no logic changed, just moved.
type claudeAdapter struct {
	cache *transcriptCache
}

func newClaudeAdapter() *claudeAdapter {
	return &claudeAdapter{cache: newTranscriptCache(discoverClaude, parseClaude)}
}

func (a *claudeAdapter) Name() string { return "claude" }

func (a *claudeAdapter) TrashPaths(s Session) ([]string, error) {
	if s.Source != a.Name() {
		return nil, fmt.Errorf("trash session %q: source is %q, want %q", s.ID, s.Source, a.Name())
	}
	if !strings.HasSuffix(s.File, ".jsonl") {
		return nil, fmt.Errorf("trash Claude session %q: transcript is not a .jsonl file", s.ID)
	}
	id := strings.TrimSuffix(filepath.Base(s.File), ".jsonl")
	if id == "" || id != s.ID {
		return nil, fmt.Errorf("trash Claude session %q: transcript filename does not match the session id", s.ID)
	}
	return []string{s.File, filepath.Join(filepath.Dir(s.File), id)}, nil
}

func (a *claudeAdapter) Sessions() ([]Session, error) {
	sessions, err := a.cache.load()
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].Source = "claude"
	}
	return sessions, nil
}

// Live attaches the state reported by running Claude Code processes and the
// tmux pane each live process sits in. Claude maintains a per-process registry
// (~/.claude/sessions/<pid>.json) carrying a real PID, so we can walk the
// process tree to the hosting tmux pane — a precision path unavailable to
// sources like pi whose process model hides the PID.
func (a *claudeAdapter) Live(sessions []Session) {
	live := liveStates()
	var panes map[int]paneInfo
	loaded := false
	for i := range sessions {
		if sessions[i].Source != "claude" {
			continue
		}
		info, ok := live[sessions[i].ID]
		if !ok {
			continue
		}
		sessions[i].State = info.State
		sessions[i].PID = info.PID
		if !loaded {
			panes, loaded = tmuxPanes(), true
		}
		if p, ok := paneFor(panes, info.PID); ok {
			sessions[i].Pane = p.Name // session:window.pane, the format runCommand expects
		}
	}
}

// discoverClaude lists every Claude Code transcript under ~/.claude/projects.
func discoverClaude() ([]string, error) {
	root, err := claudeDir("projects")
	if err != nil {
		return nil, err
	}
	projectDirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // Claude not installed/used on this machine: contribute nothing
		}
		return nil, err
	}
	var paths []string
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		dir := filepath.Join(root, pd.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			paths = append(paths, filepath.Join(dir, f.Name()))
		}
	}
	return paths, nil
}

// parseClaude builds a Session from one Claude transcript file's metadata.
func parseClaude(path string, info fs.FileInfo) Session {
	s := Session{
		ID:       strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		File:     path,
		Modified: info.ModTime(),
		Size:     info.Size(),
	}
	parseTranscript(&s)
	return s
}

// parseTranscript fills metadata by scanning the head of the file (for the
// first user prompt and title) and the tail (for the latest branch/cwd).
func parseTranscript(s *Session) {
	f, err := os.Open(s.File)
	if err != nil {
		return
	}
	defer f.Close()

	var firstPrompt string
	scanJSONL[transcriptLine](io.LimitReader(f, headScanBytes), func(l transcriptLine) {
		absorb(s, l)
		if firstPrompt == "" {
			firstPrompt = userPrompt(l)
		}
	})

	if s.Size > headScanBytes {
		// Rescan the end of the file (overlapping the head scan is harmless:
		// absorb lets later lines win) so lastEv reflects the final entry.
		off := max(s.Size-tailScanBytes, 0)
		if _, err := f.Seek(off, io.SeekStart); err == nil {
			r := bufio.NewReader(f)
			r.ReadString('\n') // drop partial first line
			scanJSONL[transcriptLine](r, func(l transcriptLine) { absorb(s, l) })
		}
	}

	if s.Title == "" {
		s.Title = firstPrompt
	}
}

// absorb copies metadata fields from a transcript line, later lines winning.
func absorb(s *Session, l transcriptLine) {
	if l.AITitle != "" {
		s.Title = l.AITitle
	}
	if l.CWD != "" {
		s.CWD = l.CWD
	}
	if l.GitBranch != "" {
		s.Branch = l.GitBranch
	}
	if l.Slug != "" {
		s.Slug = l.Slug
	}
	if l.Type == "assistant" && l.Message != nil && l.Message.Role == "assistant" && l.Message.Model != "" {
		s.Model = l.Message.Model
	}
	if txt := assistantText(l); txt != "" {
		s.LastMsg = txt
	}
	// Track the newest entry that carries a timestamp. Mode/permission-mode
	// records have none, so they never advance Activity — that keeps sessions
	// ordered by real conversation activity rather than by file mtime, which a
	// stray mode write bumps without anything actually happening.
	if l.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, l.Timestamp); err == nil && t.After(s.Activity) {
			s.Activity = t
		}
	}
}

// assistantText returns the collapsed text of an assistant message, or "".
// Tool-call preambles and pure tool-use turns yield no text and are skipped,
// so LastMsg tracks the last thing Claude actually said.
func assistantText(l transcriptLine) string {
	if l.Type != "assistant" || l.Message == nil || l.Message.Role != "assistant" {
		return ""
	}
	text := strings.TrimSpace(contentText(l.Message.Content))
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

// userPrompt extracts human-typed text from a user line, or "".
func userPrompt(l transcriptLine) string {
	if l.Type != "user" || l.IsMeta || l.Message == nil || l.Message.Role != "user" {
		return ""
	}
	text := strings.TrimSpace(contentText(l.Message.Content))
	// Skip slash-command wrappers, hook output, and injected reminders.
	if text == "" || strings.HasPrefix(text, "<") {
		return ""
	}
	text, _, _ = strings.Cut(text, "\n")
	return text
}

// registryStates translates the Claude registry's status vocabulary to ours.
var registryStates = map[string]SessionState{
	"busy":    StateRunning,
	"waiting": StateWaiting,
	"idle":    StateIdle,
}

// registrySession mirrors ~/.claude/sessions/<pid>.json, the per-process status
// file each running Claude Code instance maintains.
type registrySession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	StartedAt int64  `json:"startedAt"` // milliseconds since the epoch
	Status    string `json:"status"`
}

// procStartTolerance is how far a process's start time may sit from the
// registry's startedAt stamp and still count as the same process. The CLI
// records startedAt a second or two into its boot; anything further off
// means the pid was recycled by an unrelated process.
const procStartTolerance = 15 * time.Second

// liveInfo is what the registry tells us about one running session.
type liveInfo struct {
	State SessionState
	PID   int
}

// liveStates reads the session registry and returns sessionID -> liveInfo for
// sessions whose process is still alive. Registry files of crashed sessions
// can linger, so each pid is checked against its process start time.
func liveStates() map[string]liveInfo {
	live := map[string]liveInfo{}
	dir, err := claudeDir("sessions")
	if err != nil {
		return live
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return live
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r registrySession
		if err := json.Unmarshal(data, &r); err != nil || r.SessionID == "" {
			continue
		}
		start := procStartTime(r.PID)
		delta := time.Duration(r.StartedAt-start) * time.Millisecond
		if start == 0 || delta.Abs() > procStartTolerance {
			continue // stale file: pid dead or recycled
		}
		state, ok := registryStates[r.Status]
		if !ok {
			state = StateUnknown
		}
		live[r.SessionID] = liveInfo{State: state, PID: r.PID}
	}
	return live
}

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
	Usage   *Usage          `json:"usage"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
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
// (~/.claude/sessions/<pid>.json) recording both the session's status and the
// pane it runs in — a precision path unavailable to sources like pi, which
// keep no per-process state and have to match panes by cwd.
//
// A session started with `claude --background` carries no "tmux" field, which
// is correct: a background job has no pane of its own. Its process is still an
// OS child of whatever shell launched it, so the old process-tree walk used to
// invent that launcher's pane for it. Background and JobID flag the case so the
// enter command can offer `claude attach <jobid>` instead of a `claude
// --resume` that Claude itself refuses (the running job already owns the
// session).
//
// The registry's "status" is live for background jobs as much as interactive
// ones -- see the note on registryStates before reaching for the CLI.
func (a *claudeAdapter) Live(sessions []Session) {
	live := liveStates()
	var panes map[string]paneInfo
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
		sessions[i].Background = info.Background
		sessions[i].JobID = info.JobID
		if info.Pane == "" {
			continue // the registry records no pane: nothing to jump to
		}
		if !loaded {
			panes, loaded = tmuxPaneTargets(), true
		}
		if p, ok := panes[info.Pane]; ok {
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
	if l.Type == "assistant" && l.Message != nil && l.Message.Role == "assistant" {
		if l.Message.Model != "" {
			s.Model = l.Message.Model
		}
		// Keep the last known CtxTokens when this assistant line has no Usage
		// block. A partial write mid-session, a tool-only turn, or a schema
		// drift would otherwise zero the cell even though the previous
		// assistant turn recorded a real value a few lines up. A later
		// assistant line with Usage still overwrites the cell.
		if u := l.Message.Usage; u != nil {
			s.CtxTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		}
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
//
// This is the live status for background jobs too, not just interactive ones.
// `claude agents --json` also reports a "state" (working|blocked|done|failed|
// stopped), which looks like a richer signal and is not: it is the job's
// lifecycle, so it sits on "working" for a job that never finished cleanly and
// on "blocked" for a parked session. Measured against a live job, "status"
// tracks the turn exactly -- busy while working, waiting while the job sits on
// a permission prompt or a question -- and costs no subprocess.
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
	Kind      string `json:"kind"`  // "interactive" (has a tty/pane) or "bg" (claude --background)
	JobID     string `json:"jobId"` // short id `claude attach`/`claude stop` take; set when Kind is "bg"
	Spare     bool   `json:"spare"` // a pre-warmed pool process, not a session the user started
	Tmux      string `json:"tmux"`  // "<session>:@<window>.%<pane>" the process runs in; interactive only
}

// procStartTolerance is how far a process's start time may sit from the
// registry's startedAt stamp and still count as the same process. The CLI
// records startedAt a second or two into its boot; anything further off
// means the pid was recycled by an unrelated process.
const procStartTolerance = 15 * time.Second

// liveInfo is what the registry tells us about one running session.
type liveInfo struct {
	State      SessionState
	PID        int
	Background bool   // true when Kind is "bg": running detached, no pane to jump to
	JobID      string // set when Background; the id `claude attach`/`claude stop` take
	Pane       string // the registry's own "<session>:@<window>.%<pane>"; "" when it records none
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
		if r.Spare {
			// A pre-warmed pool process. It writes a registry file like any
			// other bg job (kind "bg", a jobId, a name equal to the jobId) but
			// hosts no conversation, so listing it would offer an `attach` to
			// an empty session. `claude agents --json` filters these out too.
			continue
		}
		state, ok := registryStates[r.Status]
		if !ok {
			state = StateUnknown
		}
		live[r.SessionID] = liveInfo{
			State:      state,
			PID:        r.PID,
			Background: r.Kind == "bg",
			JobID:      r.JobID,
			Pane:       r.Tmux,
		}
	}
	return live
}

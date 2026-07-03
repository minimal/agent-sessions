package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionState describes what a live session is doing, as reported by the
// Claude Code process itself via ~/.claude/sessions/<pid>.json.
type SessionState string

const (
	StateRunning SessionState = "running" // Claude's turn is in progress
	StateWaiting SessionState = "waiting" // blocked on the user, e.g. a permission prompt
	StateIdle    SessionState = "idle"    // waiting for the next prompt
	StateUnknown SessionState = "unknown" // the registry reported a status we don't know
)

// sessionStates is the canonical display order for live-state summaries.
var sessionStates = []SessionState{StateRunning, StateWaiting, StateIdle}

// registryStates translates the registry's status vocabulary to ours.
var registryStates = map[string]SessionState{
	"busy":    StateRunning,
	"waiting": StateWaiting,
	"idle":    StateIdle,
}

// Session is one Claude Code session transcript found on this machine.
type Session struct {
	ID       string
	File     string
	CWD      string
	Branch   string
	Slug     string
	Title    string
	LastMsg  string // most recent assistant text, collapsed to one line
	Modified time.Time
	Size     int64
	State    SessionState // empty unless Live
	PID      int          // the running claude process; 0 unless Live
	Pane     string       // session:window.pane hosting the process, if any
}

// Live reports whether a running claude process is attached to the session.
func (s Session) Live() bool {
	return s.PID != 0
}

// Project returns a short display name for the session's working directory.
func (s Session) Project() string {
	if s.CWD == "" {
		return "?"
	}
	return displayPath(s.CWD)
}

// displayPath shortens a path under the user's home directory to ~.
func displayPath(path string) string {
	if home, _ := os.UserHomeDir(); home != "" {
		if rest, ok := strings.CutPrefix(path, home); ok {
			return "~" + rest
		}
	}
	return path
}

// Delete removes the session's transcript and its sidecar directory
// (subagent transcripts, tool results).
func (s Session) Delete() error {
	if err := os.Remove(s.File); err != nil {
		return err
	}
	return os.RemoveAll(strings.TrimSuffix(s.File, ".jsonl"))
}

// matches reports whether the lowercase query appears in any of the
// session's searchable fields.
func (s Session) matches(q string) bool {
	for _, f := range []string{s.Subject(), s.Project(), s.Branch, s.ID, s.CWD, s.Pane} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// Subject is the line shown in the index: AI title, else first prompt, else slug.
func (s Session) Subject() string {
	if s.Title != "" {
		return s.Title
	}
	if s.Slug != "" {
		return "(" + s.Slug + ")"
	}
	return "(empty session)"
}

// transcriptLine covers the JSONL fields we care about across entry types.
type transcriptLine struct {
	Type      string `json:"type"`
	AITitle   string `json:"aiTitle"`
	CWD       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Slug      string `json:"slug"`
	IsMeta    bool   `json:"isMeta"`
	Message   *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

const (
	headScanBytes = 256 * 1024
	tailScanBytes = 64 * 1024
)

// claudeDir returns the path of a directory under ~/.claude.
func claudeDir(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home, ".claude"}, elem...)...), nil
}

// loader loads sessions, caching parsed transcript metadata between calls so
// the periodic refresh only re-parses files that actually changed.
type loader struct {
	cache map[string]Session // by path; entries hold no live state
}

func newLoader() *loader {
	return &loader{cache: map[string]Session{}}
}

// Load scans ~/.claude/projects for session transcripts, newest first, with
// live processes marked. Not safe for concurrent calls.
func (ld *loader) Load() ([]Session, error) {
	root, err := claudeDir("projects")
	if err != nil {
		return nil, err
	}
	projectDirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var sessions []Session
	fresh := make(map[string]Session, len(ld.cache))
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
			info, err := f.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(dir, f.Name())
			s, ok := ld.cache[path]
			if !ok || !s.Modified.Equal(info.ModTime()) || s.Size != info.Size() {
				s = Session{
					ID:       strings.TrimSuffix(f.Name(), ".jsonl"),
					File:     path,
					Modified: info.ModTime(),
					Size:     info.Size(),
				}
				parseTranscript(&s)
			}
			fresh[path] = s
			sessions = append(sessions, s)
		}
	}
	ld.cache = fresh // also drops entries for deleted files

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Modified.After(sessions[j].Modified)
	})
	markLive(sessions)
	return sessions, nil
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
	scan(io.LimitReader(f, headScanBytes), func(l transcriptLine) {
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
			scan(r, func(l transcriptLine) { absorb(s, l) })
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
	if txt := assistantText(l); txt != "" {
		s.LastMsg = txt
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

// contentText returns the first text found in a message content value,
// which is either a plain string or an array of typed blocks.
func contentText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

// scan parses JSONL lines from r, ignoring anything malformed or oversized.
func scan(r io.Reader, fn func(transcriptLine)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc.Scan() {
		var l transcriptLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		fn(l)
	}
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

// registrySession mirrors ~/.claude/sessions/<pid>.json, the per-process
// status file each running Claude Code instance maintains.
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

// markLive attaches the live state reported by running Claude Code
// processes and locates the tmux pane hosting each live session.
func markLive(sessions []Session) {
	live := liveStates()
	var panes map[int]paneInfo
	loaded := false
	for i := range sessions {
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
			sessions[i].Pane = p.Name
		}
	}
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

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copilotLine covers the JSONL fields the GitHub Copilot CLI writes to a
// session's events.jsonl that we care about. Copilot's transcript is a stream
// of typed events (session.start/user.message/assistant.message/tool.*/
// permission.*/hook.*/session.shutdown, ...). Metadata is nested under `data`:
// the session.start event carries the cwd/branch in data.context, and message
// events carry their text in data.content (a plain string). There is no inline
// title — the AI-generated name lives in the sibling workspace.yaml, and the
// subject falls back to the first user prompt.
type copilotLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Data      *struct {
		SessionID string          `json:"sessionId"`
		Content   json.RawMessage `json:"content"`
		Model     string          `json:"model"`
		Context   *struct {
			CWD    string `json:"cwd"`
			Branch string `json:"branch"`
		} `json:"context"`
	} `json:"data"`
}

// copilotAdapter reads GitHub Copilot CLI sessions under
// ~/.copilot/session-state (overridable via [sources.copilot] session_dir or
// $COPILOT_HOME). Each session is a directory named by its uuid holding an
// events.jsonl transcript and a workspace.yaml header. Copilot keeps no
// live-process registry (unlike Claude's ~/.claude/sessions/<pid>.json), so —
// like pi — authoritative live state comes from marker files written by the
// bundled hook (extensions/copilot-live); without it Live falls back to a
// cwd-based tmux pane match, and sessions surface as offline but still sort by
// activity/mtime.
type copilotAdapter struct {
	dir   string // override; "" = env/default
	cache *transcriptCache
}

func newCopilotAdapter(dir string) *copilotAdapter {
	a := &copilotAdapter{dir: dir}
	a.cache = newTranscriptCache(
		func() ([]string, error) { return discoverCopilot(a.root()) },
		parseCopilot,
	)
	return a
}

func (a *copilotAdapter) Name() string { return "copilot" }

// root resolves the session-state directory: explicit config -> $COPILOT_HOME
// -> default ~/.copilot/session-state.
func (a *copilotAdapter) root() string {
	if a.dir != "" {
		return a.dir
	}
	if env := os.Getenv("COPILOT_HOME"); env != "" {
		return filepath.Join(env, "session-state")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot", "session-state")
}

func (a *copilotAdapter) Sessions() ([]Session, error) {
	sessions, err := a.cache.load()
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].Source = "copilot"
	}
	return sessions, nil
}

// copilotLiveMarker is the JSON file written by the copilot-live hook (see
// extensions/copilot-live). It mirrors piLiveMarker: it provides authoritative
// live state without a process-tree walk. Fields a crashed/older hook may omit
// are handled gracefully.
type copilotLiveMarker struct {
	SID       string    `json:"sid"`
	PID       int       `json:"pid"`
	Pane      string    `json:"pane"`
	CWD       string    `json:"cwd"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// liveDir returns ~/.copilot/live (<session_dir>/../live), where the
// copilot-live hook writes marker files.
func (a *copilotAdapter) liveDir() string {
	root := a.root()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "..", "live")
}

// readCopilotLiveMarkers loads all marker files from the live dir, keyed by sid.
// Invalid files are skipped silently; the TUI must not break because a hook
// wrote bad JSON.
func (a *copilotAdapter) readCopilotLiveMarkers() map[string]copilotLiveMarker {
	markers := map[string]copilotLiveMarker{}
	dir := a.liveDir()
	if dir == "" {
		return markers
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return markers
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m copilotLiveMarker
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		// The filename is the canonical sid; use it if the JSON field is empty
		// (defensive against older/broken markers).
		if m.SID == "" {
			m.SID = strings.TrimSuffix(e.Name(), ".json")
		}
		markers[m.SID] = m
	}
	return markers
}

// parseCopilotMarkerStatus maps a hook status string to the shared
// SessionState vocabulary. Unknown values become StateUnknown rather than empty
// so the UI still treats the session as live.
func parseCopilotMarkerStatus(status string) SessionState {
	switch status {
	case "running":
		return StateRunning
	case "waiting":
		return StateWaiting
	case "idle":
		return StateIdle
	default:
		return StateUnknown
	}
}

// Live attaches live state to copilot sessions when the copilot-live hook has
// written marker files. A marker gives us PID, tmux pane, cwd, and status;
// sessions with a live marker become Live() and show the corresponding state.
// Sessions without a usable marker fall back to a cwd-based pane match, so the
// TUI stays useful when the hook isn't installed. The Copilot session id is
// always the plain session uuid (the session-state dir name), so no id-variant
// matching is needed.
//
// Stale markers are cleaned up: a marker whose PID is dead is removed
// regardless of age (a resumed copilot would have overwritten it with the new
// PID), and an orphan with no matching session is removed once older than an
// hour.
func (a *copilotAdapter) Live(sessions []Session) {
	markers := a.readCopilotLiveMarkers()
	panes := tmuxPanesByCWD()

	sessionIDs := map[string]bool{}
	for _, s := range sessions {
		if s.Source == "copilot" {
			sessionIDs[s.ID] = true
		}
	}

	// verified tracks marker sids whose PID we already confirmed alive in the
	// apply pass below, so the cleanup pass doesn't re-probe them.
	verified := map[string]bool{}

	for i := range sessions {
		if sessions[i].Source != "copilot" {
			continue
		}
		// A marker whose PID is no longer alive is stale (copilot crashed, was
		// killed, or tmux was restarted) — skip it so the session falls through
		// to the cwd-pane match instead of showing whatever status it last had.
		if m, ok := markers[sessions[i].ID]; ok && pidAlive(m.PID) {
			verified[m.SID] = true
			sessions[i].PID = m.PID
			sessions[i].State = parseCopilotMarkerStatus(m.Status)
			sessions[i].Pane = m.Pane
			if sessions[i].Pane == "" { // hook ran outside tmux; try cwd match
				if pane, ok := tmuxPaneForCWD(sessions[i].CWD, panes); ok {
					sessions[i].Pane = pane
				}
			}
		} else if pane, ok := tmuxPaneForCWD(sessions[i].CWD, panes); ok {
			sessions[i].Pane = pane
		}
	}

	const staleThreshold = time.Hour
	dir := a.liveDir()
	for sid, marker := range markers {
		if verified[sid] {
			continue
		}
		if !pidAlive(marker.PID) {
			_ = os.Remove(filepath.Join(dir, sid+".json"))
			continue
		}
		if sessionIDs[sid] {
			continue
		}
		if time.Since(marker.UpdatedAt) < staleThreshold {
			continue
		}
		_ = os.Remove(filepath.Join(dir, sid+".json"))
	}
}

// discoverCopilot lists every session's events.jsonl under root. Each session
// is a directory named by its uuid; the transcript is that dir's events.jsonl.
// Sessions with no events.jsonl yet (just created) contribute nothing.
func discoverCopilot(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	sessionDirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // session dir absent: contribute nothing
		}
		return nil, err
	}
	var paths []string
	for _, sd := range sessionDirs {
		if !sd.IsDir() {
			continue
		}
		events := filepath.Join(root, sd.Name(), "events.jsonl")
		if _, err := os.Stat(events); err != nil {
			continue
		}
		paths = append(paths, events)
	}
	return paths, nil
}

// parseCopilot builds a Session from one Copilot events.jsonl file's metadata.
func parseCopilot(path string, info fs.FileInfo) Session {
	s := Session{
		File:     path,
		Modified: info.ModTime(),
		Size:     info.Size(),
		Source:   "copilot",
		// The session uuid is the transcript's parent directory name.
		ID: filepath.Base(filepath.Dir(path)),
	}
	parseCopilotTranscript(&s)
	return s
}

// parseCopilotTranscript fills metadata by scanning the head (session.start
// context + first user prompt) and tail (last assistant text, newest activity),
// then reads the sibling workspace.yaml for the AI-generated title. Mirrors
// parsePiTranscript's windowing.
func parseCopilotTranscript(s *Session) {
	f, err := os.Open(s.File)
	if err != nil {
		return
	}
	defer f.Close()

	var firstPrompt, contextBranch string
	scanJSONL[copilotLine](io.LimitReader(f, headScanBytes), func(l copilotLine) {
		absorbCopilot(s, l, &contextBranch)
		if firstPrompt == "" {
			firstPrompt = copilotFirstUserPrompt(l)
		}
	})

	if s.Size > headScanBytes {
		off := max(s.Size-tailScanBytes, 0)
		if _, err := f.Seek(off, io.SeekStart); err == nil {
			r := bufio.NewReader(f)
			r.ReadString('\n') // drop partial first line
			scanJSONL[copilotLine](r, func(l copilotLine) { absorbCopilot(s, l, &contextBranch) })
		}
	}

	// Branch: prefer the repo's current branch (fresh), fall back to the branch
	// recorded in session.start when cwd isn't a readable git repo.
	s.Branch = gitBranch(s.CWD)
	if s.Branch == "" {
		s.Branch = contextBranch
	}

	// Title: the AI-generated name from workspace.yaml, else the first prompt.
	if name := copilotWorkspaceName(filepath.Dir(s.File)); name != "" {
		s.Title = name
	} else {
		s.Title = firstPrompt
	}
}

// absorbCopilot copies metadata from a Copilot transcript line, later lines
// winning. Activity advances on any real event timestamp; hook.* events (which
// fire constantly — dozens per session) and session.info notifications aren't
// conversation, so they must not float an untouched session to the top, the
// same rule pi's parser applies to model_change writes.
func absorbCopilot(s *Session, l copilotLine, contextBranch *string) {
	if l.Type == "session.start" && l.Data != nil && l.Data.Context != nil {
		if l.Data.Context.CWD != "" {
			s.CWD = l.Data.Context.CWD
		}
		if l.Data.Context.Branch != "" {
			*contextBranch = l.Data.Context.Branch
		}
	}
	if txt := copilotAssistantText(l); txt != "" {
		s.LastMsg = txt
	}
	if l.Timestamp != "" && !strings.HasPrefix(l.Type, "hook.") && l.Type != "session.info" {
		if t, err := time.Parse(time.RFC3339, l.Timestamp); err == nil && t.After(s.Activity) {
			s.Activity = t
		}
	}
}

// copilotAssistantText returns the collapsed text of a Copilot assistant
// message, or "". Assistant content is a plain string but is frequently empty
// on tool-only turns, so LastMsg tracks the last thing the agent actually said.
func copilotAssistantText(l copilotLine) string {
	if l.Type != "assistant.message" || l.Data == nil {
		return ""
	}
	text := strings.TrimSpace(contentText(l.Data.Content))
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

// copilotFirstUserPrompt extracts the first human-typed prompt from a user
// message. Copilot user content is a plain string (data.content); we take its
// first line as the subject fallback.
func copilotFirstUserPrompt(l copilotLine) string {
	if l.Type != "user.message" || l.Data == nil {
		return ""
	}
	t := strings.TrimSpace(contentText(l.Data.Content))
	if t == "" {
		return ""
	}
	t, _, _ = strings.Cut(t, "\n")
	return t
}

// copilotWorkspaceName reads the AI-generated session name from a session dir's
// workspace.yaml. The file is a small flat "key: value" document, so a tiny
// line scanner avoids pulling in a YAML dependency for one field. Returns ""
// when the file is absent or has no name.
func copilotWorkspaceName(sessionDir string) string {
	data, err := os.ReadFile(filepath.Join(sessionDir, "workspace.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "name" {
			continue
		}
		val = strings.TrimSpace(val)
		// Strip optional surrounding quotes (YAML allows either).
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		return val
	}
	return ""
}

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
	"syscall"
	"time"
)

// piLine covers the JSONL fields pi writes that we care about. Pi's transcript
// schema differs from Claude's: entries are typed (session/message/model_change/
// thinking_level_change/custom/compaction), messages carry role+content blocks,
// and there is no inline aiTitle/gitBranch/slug — cwd comes from the session
// line, branch from the repo, and the subject from the first user prompt (or a
// session named with `pi --name`).
type piLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Name      string `json:"name"`
	Message   *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// piAdapter reads pi transcripts under ~/.pi/agent/sessions (overridable via
// [sources.pi] session_dir or $PI_CODING_AGENT_SESSION_DIR). pi keeps no live-process
// registry (unlike Claude's ~/.claude/sessions/<pid>.json), so with no extension
// writing one there's no authoritative live signal, and Live is a no-op for now:
// sessions surface as offline but still sort to the top by activity/mtime. The
// process itself is a normal Linux process (here, a WSL pnpm install on nix node),
// visible to gopsutil/tmux on the host — a future phase can find its pane by cwd
// match (tmuxPaneForCWD) and, with a small pi extension writing a marker file on
// the session_start/agent_start/agent_end/session_shutdown hooks, get real
// running/waiting/idle state.
type piAdapter struct {
	dir   string // override; "" = env/default
	cache *transcriptCache
}

func newPiAdapter(dir string) *piAdapter {
	a := &piAdapter{dir: dir}
	a.cache = newTranscriptCache(
		func() ([]string, error) { return discoverPi(a.root()) },
		parsePi,
	)
	return a
}

func (a *piAdapter) Name() string { return "pi" }

// root resolves the session directory: explicit config -> env -> default.
func (a *piAdapter) root() string {
	if a.dir != "" {
		return a.dir
	}
	if env := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

func (a *piAdapter) Sessions() ([]Session, error) {
	sessions, err := a.cache.load()
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].Source = "pi"
	}
	return sessions, nil
}

// piLiveMarker is the JSON file written by the agent-sessions-pi-live pi
// extension. It provides authoritative live state without requiring a
// process-tree walk. Fields an older/crashed extension may omit are handled
// gracefully.
type piLiveMarker struct {
	SID       string    `json:"sid"`
	PID       int       `json:"pid"`
	Pane      string    `json:"pane"`
	CWD       string    `json:"cwd"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// liveDir returns ~/.pi/agent/live (or <session_dir>/../live). This is where
// the agent-sessions-pi-live extension writes marker files.
func (a *piAdapter) liveDir() string {
	root := a.root()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "..", "live")
}

// readLiveMarkers loads all marker files from the live dir, keyed by sid.
// Invalid files are skipped silently; the TUI should not break because an
// extension wrote bad JSON.
func (a *piAdapter) readLiveMarkers() map[string]piLiveMarker {
	markers := map[string]piLiveMarker{}
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
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m piLiveMarker
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

// pidAlive reports whether pid is currently a live process we can observe. It
// uses signal 0, which probes for existence and permission without disturbing
// the target. A nil error means alive; EPERM means the process exists but we
// lack permission to signal it (treat as alive); anything else means the PID
// is gone. This is what lets us tell apart a real running pi from a stale
// marker left behind after tmux was restarted and the panes — and the pi
// processes inside them — were killed.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	return true
}

// piIDVariants returns possible marker keys for a session id. The extension
// uses the session uuid as sid, but Go's fallback id parsing may keep a
// "<timestamp>_<uuid>" form when the transcript's session line lacks an id.
func piIDVariants(id string) []string {
	variants := []string{id}
	if _, after, ok := strings.Cut(id, "_"); ok && after != "" {
		variants = append(variants, after)
	}
	return variants
}

// parsePiMarkerStatus maps an extension status string to the shared SessionState
// vocabulary. Unknown values become StateUnknown rather than empty so the UI
// still treats the session as live.
func parsePiMarkerStatus(status string) SessionState {
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

// Live attaches live state to pi sessions when the agent-sessions-pi-live
// extension has written marker files. A marker gives us PID, tmux pane, cwd,
// and status; sessions with a marker become Live() and show the corresponding
// state. Sessions without a marker fall back to the cwd-based pane match from
// phase 2, so the TUI stays useful even when the extension isn't installed.
//
// Stale markers (no matching session and older than one hour) are cleaned up
// to recover from pi crashes where session_shutdown didn't run.
func (a *piAdapter) Live(sessions []Session) {
	markers := a.readLiveMarkers()
	panes := tmuxPanesByCWD()

	// Build a set of known session ids, including both the full id and the uuid
	// suffix. The extension uses the session uuid as sid, but Go's fallback id
	// parsing may keep a "<timestamp>_<uuid>" form when the session line lacks
	// an id field.
	sessionIDs := map[string]bool{}
	for _, s := range sessions {
		if s.Source != "pi" {
			continue
		}
		sessionIDs[s.ID] = true
		if _, after, ok := strings.Cut(s.ID, "_"); ok {
			sessionIDs[after] = true
		}
	}

	// verified tracks marker sids whose PID we already confirmed alive in the
	// apply pass below, so the cleanup pass doesn't re-probe them.
	verified := map[string]bool{}

	for i := range sessions {
		if sessions[i].Source != "pi" {
			continue
		}

		// Try the session id, then the uuid suffix, then give up. A marker
		// whose PID is no longer a live process is stale (e.g. tmux was
		// restarted and the pi inside the pane was killed) — skip it so the
		// session falls through to the cwd-pane match instead of being shown
		// as live with whatever status it last had.
		var marker piLiveMarker
		var hasMarker bool
		for _, v := range piIDVariants(sessions[i].ID) {
			if m, ok := markers[v]; ok {
				if !pidAlive(m.PID) {
					continue
				}
				marker = m
				hasMarker = true
				verified[v] = true
				break
			}
		}

		if hasMarker {
			sessions[i].PID = marker.PID
			sessions[i].Pane = marker.Pane
			sessions[i].State = parsePiMarkerStatus(marker.Status)
		} else if pane, ok := tmuxPaneForCWD(sessions[i].CWD, panes); ok {
			sessions[i].Pane = pane
		}
	}

	// Clean up stale markers:
	//   1. PID is no longer alive -> the process is gone (crash, killed, tmux
	//      restart). Delete regardless of age or whether a session matches:
	//      a fresh session_start from a resumed pi would have overwritten the
	//      marker with the new PID, so a dead PID here is definitively stale.
	//   2. No matching session and older than an hour -> orphan. We preserve
	//      young orphans in case a session exists but hasn't been loaded by
	//      the cache yet.
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

// discoverPi lists every pi transcript under root. Each project's transcripts
// live in a directory whose name encodes the cwd (path separators -> '-'); that
// encoding is a shard key only, so the real cwd is read from the session line
// rather than decoded from the dir name.
func discoverPi(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	projectDirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // session dir absent: contribute nothing
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

// parsePi builds a Session from one pi transcript file's metadata.
func parsePi(path string, info fs.FileInfo) Session {
	s := Session{
		File:     path,
		Modified: info.ModTime(),
		Size:     info.Size(),
		Source:   "pi",
	}
	parsePiTranscript(&s)
	return s
}

// parsePiTranscript fills metadata by scanning the head (session line + first
// user prompt) and tail (latest cwd/name, last assistant text, newest message
// timestamp). Mirrors parseTranscript's windowing.
func parsePiTranscript(s *Session) {
	f, err := os.Open(s.File)
	if err != nil {
		return
	}
	defer f.Close()

	var firstPrompt string
	scanJSONL[piLine](io.LimitReader(f, headScanBytes), func(l piLine) {
		absorbPi(s, l)
		if firstPrompt == "" {
			firstPrompt = piFirstUserPrompt(l)
		}
	})

	if s.Size > headScanBytes {
		off := max(s.Size-tailScanBytes, 0)
		if _, err := f.Seek(off, io.SeekStart); err == nil {
			r := bufio.NewReader(f)
			r.ReadString('\n') // drop partial first line
			scanJSONL[piLine](r, func(l piLine) { absorbPi(s, l) })
		}
	}

	if s.ID == "" {
		// Fall back to the UUID in the filename (<timestamp>_<uuid>.jsonl).
		base := strings.TrimSuffix(filepath.Base(s.File), ".jsonl")
		if _, after, ok := strings.Cut(base, "_"); ok {
			s.ID = after
		} else {
			s.ID = base
		}
	}
	s.Branch = gitBranch(s.CWD)
	if s.Title == "" {
		s.Title = firstPrompt
	}
}

// absorbPi copies metadata from a pi transcript line, later lines winning.
// Activity advances only on message timestamps: model_change and
// thinking_level_change carry timestamps but aren't conversation, so a stray
// setting toggle must not float an untouched session to the top — the same rule
// Claude's parser applies to its mode writes.
func absorbPi(s *Session, l piLine) {
	if l.Type == "session" {
		if l.ID != "" {
			s.ID = l.ID
		}
		if l.CWD != "" {
			s.CWD = l.CWD
		}
		if l.Name != "" {
			s.Title = l.Name
		}
	}
	if txt := piAssistantText(l); txt != "" {
		s.LastMsg = txt
	}
	if l.Type == "message" && l.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, l.Timestamp); err == nil && t.After(s.Activity) {
			s.Activity = t
		}
	}
}

// piAssistantText returns the collapsed text of a pi assistant message, or "".
// Pi assistant content is an array of blocks (thinking/text/toolCall); we take
// the first text block, so pure-thinking or tool-call turns yield nothing and
// LastMsg tracks the last thing the agent actually said.
func piAssistantText(l piLine) string {
	if l.Type != "message" || l.Message == nil || l.Message.Role != "assistant" {
		return ""
	}
	text := strings.TrimSpace(contentText(l.Message.Content))
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

// piFirstUserPrompt extracts the first human-typed text block from a user
// message, skipping injected context blocks (those starting with '<'). Pi user
// content is an array of blocks, so we scan for the first text block that looks
// like a real prompt rather than just the first block.
func piFirstUserPrompt(l piLine) string {
	if l.Type != "message" || l.Message == nil || l.Message.Role != "user" {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		t := strings.TrimSpace(b.Text)
		if t == "" || strings.HasPrefix(t, "<") {
			continue
		}
		t, _, _ = strings.Cut(t, "\n")
		return t
	}
	return ""
}

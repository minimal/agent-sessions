package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
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
// live-process registry (unlike Claude's ~/.claude/sessions/<pid>.json).
// Authoritative state comes from marker files written by the bundled hook
// (extensions/copilot-live); without it, Copilot's inuse.<pid>.lock files still
// identify active sessions, while a cwd-based tmux match supplies the pane.
type copilotAdapter struct {
	dir     string // override; "" = env/default
	usageDB string
	cache   *transcriptCache
}

func newCopilotAdapter(dir string) *copilotAdapter {
	a := &copilotAdapter{dir: dir, usageDB: copilotUsageDB()}
	a.cache = newTranscriptCache(
		func() ([]string, error) { return discoverCopilot(a.root()) },
		parseCopilot,
	)
	return a
}

func (a *copilotAdapter) Name() string { return "copilot" }

func (a *copilotAdapter) TrashPaths(s Session) ([]string, error) {
	if s.Source != a.Name() {
		return nil, fmt.Errorf("trash session %q: source is %q, want %q", s.ID, s.Source, a.Name())
	}
	if filepath.Base(s.File) != "events.jsonl" {
		return nil, fmt.Errorf("trash Copilot session %q: transcript is not events.jsonl", s.ID)
	}
	root, err := filepath.Abs(a.root())
	if err != nil {
		return nil, fmt.Errorf("trash Copilot session %q: resolve session root: %w", s.ID, err)
	}
	sessionDir, err := filepath.Abs(filepath.Dir(s.File))
	if err != nil {
		return nil, fmt.Errorf("trash Copilot session %q: resolve session directory: %w", s.ID, err)
	}
	if filepath.Dir(sessionDir) != root || filepath.Base(sessionDir) != s.ID {
		return nil, fmt.Errorf("trash Copilot session %q: session directory is outside the configured root", s.ID)
	}
	return []string{sessionDir}, nil
}

// copilotHome resolves $COPILOT_HOME, falling back to ~/.copilot.
func copilotHome() string {
	if env := os.Getenv("COPILOT_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot")
}

func copilotUsageDB() string {
	home := copilotHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "session-store.db")
}

// root resolves the session-state directory: explicit config -> Copilot home.
func (a *copilotAdapter) root() string {
	if a.dir != "" {
		return a.dir
	}
	home := copilotHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "session-state")
}

func (a *copilotAdapter) Sessions() ([]Session, error) {
	sessions, err := a.cache.load()
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].Source = "copilot"
	}
	tokens, err := copilotContextTokens(a.usageDB, sessions)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].CtxTokens = tokens[sessions[i].ID]
	}
	return sessions, nil
}

// copilotContextTokens reads the latest root-agent usage row for each loaded
// Copilot session. Copilot stores cached input inside input_tokens already, so
// cache_read_tokens and cache_write_tokens must not be added again.
//
// Fails open: any error talking to the usage DB (missing file, missing table,
// SQLITE_BUSY from a concurrent Copilot write, corrupt DB, schema drift) is
// logged and surfaces as an empty token map. The non-Copilot sessions must
// still load, so multiLoader.Load() can keep merging Claude/pi/etc. on top
// of a degraded Copilot read. The ctx column is the only thing that goes
// blank; the rest of the session list is unaffected.
func copilotContextTokens(path string, sessions []Session) (map[string]int, error) {
	tokens := make(map[string]int, len(sessions))
	if path == "" || len(sessions) == 0 {
		return tokens, nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tokens, nil
		}
		log.Printf("copilot usage db: stat %s: %v (ctx column will be blank)", path, err)
		return tokens, nil
	}

	db, err := sql.Open("sqlite", copilotSQLiteDSN(path))
	if err != nil {
		log.Printf("copilot usage db: open %s: %v (ctx column will be blank)", path, err)
		return tokens, nil
	}
	defer db.Close()

	var tableExists int
	err = db.QueryRow(`
		SELECT 1
		FROM sqlite_master
		WHERE type = 'table' AND name = 'assistant_usage_events'
	`).Scan(&tableExists)
	if errors.Is(err, sql.ErrNoRows) {
		return tokens, nil
	}
	if err != nil {
		log.Printf("copilot usage db: inspect schema: %v (ctx column will be blank)", err)
		return tokens, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sessions)), ",")
	args := make([]any, len(sessions))
	for i := range sessions {
		args[i] = sessions[i].ID
	}
	query := `
		SELECT usage.session_id,
		       COALESCE(usage.input_tokens, 0)
		FROM assistant_usage_events AS usage
		JOIN (
			SELECT session_id, MAX(id) AS id
			FROM assistant_usage_events
			WHERE agent_id IS NULL AND session_id IN (` + placeholders + `)
			GROUP BY session_id
		) AS latest ON latest.id = usage.id
	`
	rows, err := db.Query(query, args...)
	if err != nil {
		log.Printf("copilot usage db: query: %v (ctx column will be blank)", err)
		return tokens, nil
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var total int
		if err := rows.Scan(&sessionID, &total); err != nil {
			log.Printf("copilot usage db: scan: %v (ctx column will be blank)", err)
			return tokens, nil
		}
		tokens[sessionID] = total
	}
	if err := rows.Err(); err != nil {
		log.Printf("copilot usage db: read: %v (ctx column will be blank)", err)
		return tokens, nil
	}
	return tokens, nil
}

func copilotSQLiteDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "true")
	// Tolerate a concurrent Copilot write (5s is enough for any normal commit;
	// past that we'd rather blank the ctx column than block the whole session
	// list, which fail-open would do anyway).
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()
	return u.String()
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

// copilotInUsePID returns the first live PID named by an inuse.<pid>.lock file
// in sessionDir. Copilot creates these locks itself, so they remain available
// when the optional agent-sessions live hook is not installed.
func copilotInUsePID(sessionDir string) int {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		pidText, ok := strings.CutPrefix(entry.Name(), "inuse.")
		if !ok {
			continue
		}
		pidText, ok = strings.CutSuffix(pidText, ".lock")
		if !ok || pidText == "" {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err == nil && pidAlive(pid) {
			return pid
		}
	}
	return 0
}

// Live attaches live state to copilot sessions when the copilot-live hook has
// written marker files. A marker gives us PID, tmux pane, cwd, and status;
// sessions with a live marker become Live() and show the corresponding state.
// Sessions without a usable marker use Copilot's inuse.<pid>.lock file to
// detect a live process, then fall back to a cwd-based pane match. The Copilot
// session id is always the plain session uuid (the session-state dir name), so
// no id-variant matching is needed.
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
			continue
		}
		if pid := copilotInUsePID(filepath.Join(a.root(), sessions[i].ID)); pid != 0 {
			sessions[i].PID = pid
			sessions[i].State = StateUnknown
		}
		if pane, ok := tmuxPaneForCWD(sessions[i].CWD, panes); ok {
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
	if l.Type == "assistant.message" && l.Data != nil && l.Data.Model != "" {
		s.Model = l.Data.Model
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

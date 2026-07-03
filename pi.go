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

// Live attaches a best-effort tmux pane to pi sessions by matching the pane's
// current working directory to the session's launch cwd. pi has no live-process
// registry, so there's no PID or running/waiting/idle state to attach — sessions
// stay "offline" for status purposes — but the pane match lets Enter jump to
// the terminal running pi, and the tmux glyph marks which sessions are in a
// pane. The pane's current path is a Linux path equal to the session cwd
// regardless of where the pi process lives, so this works even when a
// process-tree walk (used by Claude) wouldn't. See ADAPTERS.md for the
// extension-marker follow-up that would add real live state.
func (a *piAdapter) Live(sessions []Session) {
	panes := tmuxPanesByCWD()
	for i := range sessions {
		if sessions[i].Source != "pi" {
			continue
		}
		if pane, ok := tmuxPaneForCWD(sessions[i].CWD, panes); ok {
			sessions[i].Pane = pane
		}
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

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Adapter is one source of agent sessions (Claude Code, pi, Copilot CLI, ...).
// Add a source by implementing this interface and registering an instance in
// newModel — the core never touches a source's on-disk layout directly.
type Adapter interface {
	// Name is a short id used for per-source config (the [commands.per_source]
	// key) and to tag which sessions belong to this source. e.g. "claude", "pi".
	Name() string

	// Sessions discovers and parses this source's transcripts, freshest-first
	// by activity, with NO live state attached. Implementations should cache
	// parsed metadata between calls (mtime+size keyed) so the periodic refresh
	// is cheap — transcriptCache does this for adapters built on JSONL files.
	Sessions() ([]Session, error)

	// Live attaches running-process state (State, PID, Pane) in place,
	// best-effort. Sources with no usable live signal (e.g. pi without a
	// marker-writing extension, which has no per-process registry) implement
	// a no-op; their sessions surface as offline but still sort to the top by
	// activity/mtime. Each adapter only touches sessions it produced (by Source).
	Live(sessions []Session)
}

// headScanBytes/tailScanBytes are the byte windows JSONL adapters scan: the
// head for header/first-prompt metadata, the tail for the latest entry. Shared
// so every transcript adapter gets the same large-file behaviour.
const (
	headScanBytes = 256 * 1024
	tailScanBytes = 64 * 1024
)

// transcriptCache caches parsed Session metadata keyed by file path, re-parsing
// only files whose mtime or size changed, and dropping entries for deleted
// files. Adapters built on JSONL transcripts supply where they live (discover)
// and how to parse one (parse); the cache handles the rest.
type transcriptCache struct {
	cache    map[string]Session
	discover func() ([]string, error)
	parse    func(path string, info fs.FileInfo) Session
}

func newTranscriptCache(discover func() ([]string, error), parse func(string, fs.FileInfo) Session) *transcriptCache {
	return &transcriptCache{cache: map[string]Session{}, discover: discover, parse: parse}
}

// load scans the discovered transcripts, freshest-first by activity. It is the
// generic core of every adapter's Sessions(): the only thing that varies is
// discover (paths) and parse (one file -> Session).
func (c *transcriptCache) load() ([]Session, error) {
	paths, err := c.discover()
	if err != nil {
		return nil, err
	}
	var sessions []Session
	fresh := make(map[string]Session, len(c.cache))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		s, ok := c.cache[path]
		if !ok || !s.Modified.Equal(info.ModTime()) || s.Size != info.Size() {
			s = c.parse(path, info)
		}
		fresh[path] = s
		sessions = append(sessions, s)
	}
	c.cache = fresh // also drops entries for deleted files
	sort.Slice(sessions, func(i, j int) bool {
		a, b := sessions[i].Activity, sessions[j].Activity
		if !a.Equal(b) {
			return a.After(b)
		}
		return sessions[i].Modified.After(sessions[j].Modified)
	})
	return sessions, nil
}

// scanJSONL parses JSONL lines from r into values of type T, ignoring lines
// that are malformed or oversized. Each adapter passes its own line struct, so
// the scanner is shared but the schema isn't.
func scanJSONL[T any](r io.Reader, fn func(T)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			continue
		}
		fn(v)
	}
}

// contentText returns the first text found in a message content value, which
// is either a plain string or an array of typed blocks ({type:"text",...}).
// Shared by adapters whose message content follows this shape (Claude, pi);
// thinking/tool-call blocks are skipped, so it yields the prose.
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

// gitBranch returns the current branch of the repo at cwd, or "" if none. For
// sources that don't record the branch inline (pi, Copilot CLI, ...); Claude
// stores it in the transcript and doesn't need this. Computed at parse time, so
// it's cached with the transcript and only refreshed when the file changes — a
// branch switch with no new message shows stale until the next write. That's
// acceptable for a source with no live signal; live adapters can refresh it.
//
// In a linked worktree, <cwd>/.git is a file whose first line is
// "gitdir: <path>" — the .git directory for that worktree, typically
// "<main>/.git/worktrees/<name>" and reached via the main repo's gitdir. We
// read HEAD from the resolved worktree gitdir instead of the .git file itself;
// otherwise the file's contents ("gitdir: ...") would be parsed as the branch.
func gitBranch(cwd string) string {
	if cwd == "" {
		return ""
	}
	gitPath := filepath.Join(cwd, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	headDir := gitPath
	if !fi.IsDir() {
		headDir = worktreeGitDir(cwd, gitPath)
		if headDir == "" {
			return ""
		}
	}
	data, err := os.ReadFile(filepath.Join(headDir, "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
		return ref
	}
	return s // detached HEAD: fall back to the sha
}

// worktreeGitDir resolves a linked worktree's ".git" pointer file to the
// gitdir it names, re-anchoring a relative path against base (the worktree's
// cwd, which is the directory holding the .git file). Returns "" if the file
// can't be read or holds no "gitdir:" line.
func worktreeGitDir(base, gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	gitdir, ok := strings.CutPrefix(line, "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(base, gitdir)
	}
	return gitdir
}

// multiLoader runs every enabled adapter and merges their sessions into one
// freshest-first list with live state attached. It stands in for the old
// single-source loader: the UI calls Load() and stays source-agnostic. The
// sort dimensions (group by repo, float live, etc.) are configured by the UI
// and applied here so every source benefits from the same ordering.
type multiLoader struct {
	adapters []Adapter
	sortDims []sortDim
}

func newMultiLoader(adapters []Adapter, sortDims []sortDim) *multiLoader {
	return &multiLoader{adapters: adapters, sortDims: sortDims}
}

func (ml *multiLoader) Load() ([]Session, error) {
	var all []Session
	for _, a := range ml.adapters {
		ss, err := a.Sessions()
		if err != nil {
			return nil, err
		}
		all = append(all, ss...)
	}
	for _, a := range ml.adapters {
		a.Live(all) // each adapter ignores sessions not its own (by Source)
	}
	// Resolve each session's git repo key (shared across worktrees) so the
	// dimRepo sort can cluster a repo's sessions. Memoised within one load.
	repos := map[string]string{}
	for i := range all {
		all[i].Repo = repoKeyCached(all[i].CWD, repos)
	}
	sortSessions(all, ml.sortDims)
	return all, nil
}

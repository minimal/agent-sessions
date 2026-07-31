package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SessionState describes what a live session is doing, as reported by its
// agent's live-process signal (where one exists). The vocabulary is shared
// across adapters; each translates its source's own terms into these.
type SessionState string

const (
	StateRunning SessionState = "running" // the agent's turn is in progress
	StateWaiting SessionState = "waiting" // blocked on the user, e.g. a permission prompt
	StateIdle    SessionState = "idle"    // waiting for the next prompt
	StateUnknown SessionState = "unknown" // the source reported a status we don't know
)

// sessionStates is the canonical display order for live-state summaries.
var sessionStates = []SessionState{StateRunning, StateWaiting, StateIdle}

// Session is one agent session transcript found on this machine, from any
// source. Source identifies the adapter that produced it; the rest is generic.
// Fields an adapter can't fill (e.g. pi has no Slug, and no live PID) stay zero.
type Session struct {
	ID       string
	File     string
	CWD      string
	Branch   string
	Model    string
	Repo     string // git common dir shared by a repo's worktrees; "" if none
	Slug     string
	Title    string
	LastMsg  string    // most recent assistant text, collapsed to one line
	Modified time.Time // transcript file mtime
	Activity time.Time // timestamp of the last real entry; drives sort order
	Size     int64
	State    SessionState // empty unless Live
	PID      int          // the running agent process; 0 unless Live
	Pane     string       // session:window.pane hosting the process, if any
	Worktree bool         // true if CWD is a linked git worktree (not the main repo)
	Source   string       // which adapter produced this ("claude", "pi", ...)
}

// When is the time shown for the session: its last real activity, falling
// back to the file mtime for stubs that have no timestamped entries.
func (s Session) When() time.Time {
	if s.Activity.IsZero() {
		return s.Modified
	}
	return s.Activity
}

// Live reports whether a running agent process is attached to the session.
func (s Session) Live() bool {
	return s.PID != 0
}

// InTmux reports whether the session's process sits in a tmux pane, i.e. the
// default Enter command can jump to it without attaching a new terminal.
func (s Session) InTmux() bool {
	return s.Pane != ""
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

func displaySize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	value := float64(size)
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == units[len(units)-1] {
			precision := 1
			if value < 10 {
				precision = 3
			} else if value < 100 {
				precision = 2
			}
			number := strconv.FormatFloat(value, 'f', precision, 64)
			number = strings.TrimRight(strings.TrimRight(number, "0"), ".")
			return number + " " + unit
		}
	}
	return fmt.Sprintf("%d B", size)
}

// Dir is the working directory's final path segment, e.g. "agent-sessions"
// for "~/code/scratch/agent-sessions". Used when the project column is
// configured to show just the name rather than the full path.
func (s Session) Dir() string {
	if s.CWD == "" {
		return "?"
	}
	return filepath.Base(s.CWD)
}

// matches reports whether the lowercase query appears in any of the
// session's searchable fields (including Source, so "/pi" or "/claude"
// filters by adapter, and Pane, so a pane id narrows to sessions in it).
func (s Session) matches(q string) bool {
	for _, f := range []string{s.Subject(), s.Project(), s.Branch, s.Model, s.ID, s.CWD, s.Pane, s.Source} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// Subject is the line shown in the index: AI title (or named session), else
// first prompt, else slug.
func (s Session) Subject() string {
	if s.Title != "" {
		return s.Title
	}
	if s.Slug != "" {
		return "(" + s.Slug + ")"
	}
	return "(empty session)"
}

// sortDim is one dimension of the index ordering. Dimensions are applied in
// configured order (most significant first), with recency as the final key.
type sortDim int

const (
	dimActive sortDim = iota // live sessions (a running process) ahead of the rest
	dimRepo                  // cluster sessions of one git repo (across worktrees)
)

// parseSortDims reads the [sort] group setting: a comma-separated, ordered list
// of dimension names. Unknown names (including "activity" and "") are ignored,
// leaving the plain recency order. Duplicates collapse to their first mention.
func parseSortDims(group string) []sortDim {
	var dims []sortDim
	seen := map[sortDim]bool{}
	for _, tok := range strings.Split(group, ",") {
		var d sortDim
		switch strings.TrimSpace(strings.ToLower(tok)) {
		case "active", "live":
			d = dimActive
		case "repo":
			d = dimRepo
		default:
			continue
		}
		if !seen[d] {
			dims = append(dims, d)
			seen[d] = true
		}
	}
	return dims
}

// byRecency orders two sessions newest-first: live ahead of the rest, then by
// last real activity, with mtime breaking ties among the stubs that have none.
func byRecency(a, b Session) bool {
	if la, lb := a.Live(), b.Live(); la != lb {
		return la
	}
	if !a.Activity.Equal(b.Activity) {
		return a.Activity.After(b.Activity)
	}
	return a.Modified.After(b.Modified)
}

// rankRepos assigns each repo a sort position: repos holding a live session
// first, then by their most recent activity, with the key breaking ties so the
// order is stable across loads.
func rankRepos(sessions []Session) map[string]int {
	type agg struct {
		hasLive bool
		newest  time.Time
	}
	m := map[string]*agg{}
	for _, s := range sessions {
		a := m[s.Repo]
		if a == nil {
			a = &agg{}
			m[s.Repo] = a
		}
		a.hasLive = a.hasLive || s.Live()
		if w := s.When(); w.After(a.newest) {
			a.newest = w
		}
	}
	repos := make([]string, 0, len(m))
	for r := range m {
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool {
		ai, aj := m[repos[i]], m[repos[j]]
		if ai.hasLive != aj.hasLive {
			return ai.hasLive
		}
		if !ai.newest.Equal(aj.newest) {
			return ai.newest.After(aj.newest)
		}
		return repos[i] < repos[j]
	})
	rank := make(map[string]int, len(repos))
	for i, r := range repos {
		rank[r] = i
	}
	return rank
}

// sortSessions orders the index by the configured dimensions (most significant
// first), falling back to recency. dimActive floats live sessions ahead of the
// rest; dimRepo clusters a repo's sessions (across its worktrees) into a block.
// The order of the two is what matters: "active,repo" surfaces every live
// session first and only groups by repo within, so a repo's pile of finished
// sessions can't bury another repo's live one; "repo" (or "repo,active") keeps
// whole repos together, live-first inside each block. With no known dimension
// the plain recency order (live floated to the top) is used.
func sortSessions(sessions []Session, dims []sortDim) {
	var repoRank map[string]int
	for _, d := range dims {
		if d == dimRepo {
			repoRank = rankRepos(sessions)
			break
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		si, sj := sessions[i], sessions[j]
		for _, d := range dims {
			switch d {
			case dimActive:
				if la, lb := si.Live(), sj.Live(); la != lb {
					return la
				}
			case dimRepo:
				if si.Repo != sj.Repo {
					return repoRank[si.Repo] < repoRank[sj.Repo]
				}
			}
		}
		return byRecency(si, sj)
	})
}

// repoKeyCached resolves cwd to its repo key, memoising within one load so a
// directory shared by several sessions is walked only once.
func repoKeyCached(cwd string, cache map[string]string) string {
	if cwd == "" {
		return ""
	}
	if k, ok := cache[cwd]; ok {
		return k
	}
	k := repoKey(cwd)
	cache[cwd] = k
	return k
}

// repoKey returns a stable identifier for the git repository containing cwd,
// shared by all of that repo's worktrees, or "" if cwd is not in a repo. The
// key is the repo's common git dir, which every linked worktree points back to.
func repoKey(cwd string) string {
	dir := cwd
	for {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			if fi.IsDir() {
				return filepath.Clean(gitPath) // the main worktree; itself the common dir
			}
			return commonGitDir(dir, gitPath) // a linked worktree (or submodule)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached the filesystem root without finding .git
		}
		dir = parent
	}
}

// commonGitDir follows a ".git" file (as used by linked worktrees) to the
// repository's shared common dir, so sibling worktrees resolve to one key.
func commonGitDir(base, gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if gitdir == "" {
		return ""
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(base, gitdir)
	}
	// A worktree's gitdir holds a "commondir" pointing at the shared .git.
	if cd, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		common := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitdir, common)
		}
		return filepath.Clean(common)
	}
	return filepath.Clean(gitdir)
}

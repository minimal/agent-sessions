package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// paneInfo describes one tmux pane: its target id (%N) and its
// human-readable session:window.pane name.
type paneInfo struct {
	ID   string
	Name string
}

// tmuxPane is one pane on the server, in the forms the app needs: the id form a
// Jump targets, the session:window.pane form an adapter may have recorded, and
// the Tmux Session the pane belongs to.
type tmuxPane struct {
	id        string // %N
	name      string // session:window.pane
	session   string // the pane's Tmux Session (name)
	sessionID string // the pane's Tmux Session id ($N), to resolve $TMUX_PANE
}

// tmuxSession is one Tmux Session on the server, as the Tmux Bar sees it.
type tmuxSession struct {
	id      string // $N
	name    string
	windows int
}

// tmuxServer is one poll of the tmux server the Tmux Bar maps: its sessions in
// tmux's own order, the session the attached client is in, a lookup from each
// pane's recorded forms to its pane, and the panes an attached client has on
// screen.
//
// byPane is the attribution map. Session.Pane holds one of two forms: pi's
// marker writes $TMUX_PANE (the %N id form) and claude writes
// session:window.pane. tmux allows ':' and '.' in session names, so a stored
// string is never split back into its parts: both forms are listed as keys and
// matched exactly.
//
// onScreen answers "has the user seen this?" for read tracking: the pane is in
// its session's current Window, and the Window is not zoomed or the pane is the
// focused one (docs/adr/0001-read-means-seen.md). Both forms are keys, as in
// byPane.
type tmuxServer struct {
	sessions []tmuxSession
	current  string              // $N of the attached client's session; "" outside tmux
	byPane   map[string]tmuxPane // %N and session:window.pane -> pane
	onScreen map[string]bool     // %N and session:window.pane -> on screen
}

// pollTmux reads the server in one list-panes call: every pane carries its
// session's id, name and Window count, so a session's first appearance gives
// tmux's own session order, and the pane's two recorded forms give attribution.
// Returns nil when no server is reachable, which is what hides the Tmux Bar and
// falls read tracking back to acting on a session through the TUI.
func pollTmux() *tmuxServer {
	out, err := tmuxListPanes().Output()
	if err != nil {
		return nil
	}
	srv := parseTmuxPanes(string(out))
	srv.current = currentTmuxSession(srv)
	return srv
}

// listPanesFormat is the one field list every tmux poll asks for. It is a
// constant because a mistyped tmux variable would silently empty the Bar.
const listPanesFormat = "#{session_id}\t#{session_name}\t#{session_windows}\t#{pane_id}\t" +
	"#{session_name}:#{window_index}.#{pane_index}\t" +
	"#{session_attached}\t#{window_active}\t#{window_zoomed_flag}\t#{pane_active}"

// tmuxListPanes is the one tmux invocation the Bar and read tracking share.
//
// The socket is taken from $TMUX explicitly rather than left to tmux's own
// default: inside tmux a bare `tmux` happens to follow $TMUX, but that is an
// accident of the environment rather than a property we hold. Naming the
// socket makes "the Bar shows the server this TUI runs in" an invariant, which
// matters to a user with more than one server. Outside tmux there is no socket
// to name, so the default path is used.
func tmuxListPanes() *exec.Cmd {
	sock, _, _ := strings.Cut(os.Getenv("TMUX"), ",")
	if sock != "" {
		return exec.Command("tmux", "-S", sock, "list-panes", "-a", "-F", listPanesFormat)
	}
	return exec.Command("tmux", "list-panes", "-a", "-F", listPanesFormat)
}

// parseTmuxPanes builds the server snapshot from a `tmux list-panes -a` result.
func parseTmuxPanes(out string) *tmuxServer {
	srv := &tmuxServer{byPane: map[string]tmuxPane{}, onScreen: map[string]bool{}}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.SplitN(line, "\t", 9)
		if len(fields) != 9 {
			continue
		}
		id, name, windows, paneID, paneName := fields[0], fields[1], fields[2], fields[3], fields[4]
		attached, active, zoomed, focused := fields[5], fields[6], fields[7], fields[8]
		if !seen[id] {
			seen[id] = true
			n, _ := strconv.Atoi(windows)
			srv.sessions = append(srv.sessions, tmuxSession{id: id, name: name, windows: n})
		}
		pane := tmuxPane{id: paneID, name: paneName, session: name, sessionID: id}
		srv.byPane[paneID] = pane
		srv.byPane[paneName] = pane
		// An attached client sees its session's current Window, which shows every
		// pane unless the Window is zoomed -- and a zoomed Window shows only the
		// focused pane.
		clients, _ := strconv.Atoi(attached)
		if clients > 0 && active == "1" && (zoomed != "1" || focused == "1") {
			srv.onScreen[paneID] = true
			srv.onScreen[paneName] = true
		}
	}
	return srv
}

// tmuxCurrentSession returns the id ($N) of the Tmux Session the attached
// client is in. $TMUX holds the server's socket path, its pid and the client's
// session id, comma-separated. Empty when the TUI runs outside tmux, or when
// $TMUX is not the three-field form tmux writes.
func tmuxCurrentSession() string {
	parts := strings.Split(os.Getenv("TMUX"), ",")
	if len(parts) != 3 || parts[2] == "" {
		return ""
	}
	return "$" + parts[2]
}

// currentTmuxSession resolves the Tmux Session this process is running in, for
// the Bar to mark as "you are here". It returns "" outside tmux, which is the
// honest answer: there is no current session to mark, and the Bar renders every
// session unmarked rather than guessing one.
//
// $TMUX's session id is authoritative and is used whenever it is usable. When
// it is not -- $TMUX scrubbed or malformed, but $TMUX_PANE still exported --
// the pane is resolved against the same poll through byPane, so one lost
// variable cannot silently blank the marker.
func currentTmuxSession(srv *tmuxServer) string {
	if id := tmuxCurrentSession(); id != "" {
		return id
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" || srv == nil {
		return ""
	}
	return srv.byPane[pane].sessionID
}

// tmuxPanes returns pane root pid -> pane for every pane on the server.
func tmuxPanes() map[int]paneInfo {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_pid}\t#{pane_id}\t#{session_name}:#{window_index}.#{pane_index}").Output()
	if err != nil {
		return nil
	}
	panes := map[int]paneInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			panes[pid] = paneInfo{ID: fields[1], Name: fields[2]}
		}
	}
	return panes
}

// tmuxPaneTargets returns target -> pane for every pane on the server, keyed by
// "<session>:@<window>.%<pane>" — the form Claude Code records in its registry.
// Resolving a registry target through this map both yields the pane's
// human-readable name and proves the pane is still alive.
//
// Only the server $TMUX points at is visible, as with tmuxPanes: a target on
// another server simply doesn't resolve.
func tmuxPaneTargets() map[string]paneInfo {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}:#{window_id}.#{pane_id}\t#{pane_id}\t#{session_name}:#{window_index}.#{pane_index}").Output()
	if err != nil {
		return nil
	}
	panes := map[string]paneInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		panes[fields[0]] = paneInfo{ID: fields[1], Name: fields[2]}
	}
	return panes
}

// paneFor finds the pane that pid runs in, walking pid's ancestor chain
// until it hits a pane's root process.
func paneFor(panes map[int]paneInfo, pid int) (paneInfo, bool) {
	for p := pid; p > 1; p = parentPID(p) {
		if info, ok := panes[p]; ok {
			return info, true
		}
	}
	return paneInfo{}, false
}

// tmuxPaneFor returns the id of the tmux pane that pid runs in.
func tmuxPaneFor(pid int) (string, bool) {
	info, ok := paneFor(tmuxPanes(), pid)
	return info.ID, ok
}

// tmuxPanesByCWD maps each tmux pane's current working directory to its pane
// id, first pane winning on duplicate cwds. Empty when tmux isn't running.
//
// Unlike tmuxPanes (keyed by the pane's root-process pid, which then needs a
// process-tree walk from a known PID), this is keyed by the pane's current
// path — a Linux path that matches a session's launch cwd regardless of where
// the agent process lives. That makes it work for sources like pi, which keep
// no per-process registry (no PID to walk from) but whose session cwd equals
// the pane's current path. agent-sessions' own pane ($TMUX_PANE) is skipped so
// a session sharing this process's cwd doesn't match the pane we're running in.
func tmuxPanesByCWD() map[string]string {
	panes := map[string]string{}
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id} #{pane_current_path}").Output()
	if err != nil {
		return panes
	}
	self := os.Getenv("TMUX_PANE") // agent-sessions' own pane; never jump to it
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, path, ok := strings.Cut(line, " ")
		if !ok || id == self {
			continue
		}
		if _, dup := panes[path]; !dup {
			panes[path] = id // first wins on duplicate cwds
		}
	}
	return panes
}

// tmuxPaneForCWD returns the id of a tmux pane whose current working directory
// is cwd, or false if none. Used by adapters that can't walk a process tree to
// find a pane (pi has no PID to walk from) but can match by the session's
// launch directory, which equals the pane's current path.
func tmuxPaneForCWD(cwd string, panes map[string]string) (string, bool) {
	id, ok := panes[cwd]
	return id, ok
}

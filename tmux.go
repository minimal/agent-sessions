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

package main

import (
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

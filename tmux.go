package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// tmuxPaneFor returns the id of the tmux pane that pid runs in, found by
// walking pid's ancestor chain until it hits a pane's root process.
func tmuxPaneFor(pid int) (string, bool) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_pid} #{pane_id}").Output()
	if err != nil {
		return "", false
	}
	panes := map[int]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		panePID, id, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(panePID); err == nil {
			panes[n] = id
		}
	}
	for p := pid; p > 1; p = parentPID(p) {
		if id, ok := panes[p]; ok {
			return id, true
		}
	}
	return "", false
}

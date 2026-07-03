package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// tmuxPanes maps each pane's root-process pid to its pane id. It is empty
// when tmux isn't running, so callers degrade to "no pane" cleanly.
func tmuxPanes() map[int]string {
	panes := map[int]string{}
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_pid} #{pane_id}").Output()
	if err != nil {
		return panes
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		panePID, id, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(panePID); err == nil {
			panes[n] = id
		}
	}
	return panes
}

// paneForPID returns the id of the pane pid runs in, found by walking pid's
// ancestor chain until it hits a pane's root process.
func paneForPID(pid int, panes map[int]string) (string, bool) {
	for p := pid; p > 1; p = parentPID(p) {
		if id, ok := panes[p]; ok {
			return id, true
		}
	}
	return "", false
}

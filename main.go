package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-sessions:", err)
		os.Exit(1)
	}
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithReportFocus()}
	if cfg.Mouse.Enabled {
		// Capturing the mouse overrides the terminal's / tmux's own mouse
		// handling (text selection, scrollback), so it's opt-in.
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(newModel(cfg), opts...)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-sessions:", err)
		os.Exit(1)
	}
}

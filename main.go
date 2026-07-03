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
	p := tea.NewProgram(newModel(cfg), tea.WithAltScreen(), tea.WithReportFocus())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-sessions:", err)
		os.Exit(1)
	}
}

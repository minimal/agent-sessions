package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// options holds the command-line flags.
type options struct {
	rex bool // drive the Rex app: poll its blocks for live rows
}

// parseFlags reads the command-line flags. Usage and errors go to out.
func parseFlags(args []string, out io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("agent-sessions", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.BoolVar(&opts.rex, "rex", false, "Rex mode: find the Rex block of each live Claude session")
	err := fs.Parse(args)
	return opts, err
}

func main() {
	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		os.Exit(2)
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-sessions:", err)
		os.Exit(1)
	}
	cfg.RexMode = opts.rex
	progOpts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithReportFocus()}
	if cfg.Mouse.Enabled {
		// Capturing the mouse overrides the terminal's / tmux's own mouse
		// handling (text selection, scrollback), so it's opt-in.
		progOpts = append(progOpts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(newModel(cfg), progOpts...)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-sessions:", err)
		os.Exit(1)
	}
}

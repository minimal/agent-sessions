package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"golang.org/x/term"
)

// commandLogPath returns the XDG state location of the command log.
func commandLogPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "agent-sessions", "commands.log")
}

// execCmd runs an expanded command template. With bg it runs detached — no
// alt-screen handoff (which flashes the app closed) and no output to corrupt
// the display, suiting commands that only orchestrate tmux. Otherwise the
// command takes over the terminal so interactive ones (an editor,
// claude --resume) work, teeing everything it prints to the command log.
func execCmd(tmpl string, vars map[string]string, bg bool) tea.Cmd {
	line := expandCommand(tmpl, vars)
	if bg {
		return func() tea.Msg {
			return execDoneMsg{exec.Command("sh", "-c", line).Run()}
		}
	}
	return tea.Exec(&loggedCommand{line: line},
		func(err error) tea.Msg { return execDoneMsg{err} })
}

// loggedCommand is a tea.ExecCommand that gives `sh -c line` the real
// terminal for input — so the command is exactly as interactive as it
// would be from a shell — while routing its output through a pty that is
// mirrored to both the terminal and the command log.
type loggedCommand struct {
	line   string
	stdin  io.Reader
	stdout io.Writer
}

func (c *loggedCommand) SetStdin(r io.Reader)  { c.stdin = r }
func (c *loggedCommand) SetStdout(w io.Writer) { c.stdout = w }
func (c *loggedCommand) SetStderr(io.Writer)   {}

func (c *loggedCommand) Run() error {
	var logf *os.File
	if path := commandLogPath(); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			logf, _ = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		}
	}
	start := time.Now()
	if logf != nil {
		defer logf.Close()
		fmt.Fprintf(logf, "=== %s run: %s\n", start.Format(time.DateTime), c.line)
	}
	err := c.run(logf)
	if logf != nil {
		status := "exit 0"
		if err != nil {
			status = err.Error()
		}
		fmt.Fprintf(logf, "=== %s %s (%s)\n\n",
			time.Now().Format(time.DateTime), status, time.Since(start).Round(time.Millisecond))
	}
	return err
}

func (c *loggedCommand) run(logf *os.File) error {
	cmd := exec.Command("sh", "-c", c.line)
	tty, isTTY := c.stdin.(*os.File)
	ptmx, pts, err := pty.Open()
	if err != nil || !isTTY || !term.IsTerminal(int(tty.Fd())) {
		// No pty to capture through: run plainly, output unlogged.
		cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, os.Stderr
		return cmd.Run()
	}
	defer ptmx.Close()

	// Keep the capture pty the same size as the real terminal.
	pty.InheritSize(tty, ptmx)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			pty.InheritSize(tty, ptmx)
		}
	}()

	cmd.Stdin = tty // the real terminal: input is untouched by us
	cmd.Stdout = pts
	cmd.Stderr = pts
	if err := cmd.Start(); err != nil {
		pts.Close()
		return err
	}
	pts.Close() // the child holds its own copy now

	out := c.stdout
	if logf != nil {
		out = io.MultiWriter(c.stdout, logf)
	}
	copied := make(chan struct{})
	go func() {
		io.Copy(out, ptmx) // ends when the last pty holder exits
		close(copied)
	}()
	err = cmd.Wait()
	select {
	case <-copied:
	case <-time.After(500 * time.Millisecond):
		// A background child kept the pty open; don't hold the TUI hostage.
	}
	return err
}

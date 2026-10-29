package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// rexBundlePath is where the Rex app keeps its CLI, relative to $HOME. Outside
// Rex (the Ghostty quick terminal, a login shell) `rex` is not on PATH, so this
// is the usual hit.
const rexBundlePath = "Library/Application Support/com.superlogical.rex/RexServer/Versions/Current/Rex Server.app/Contents/MacOS/rex"

// rexTimeout caps one rex call. A poll takes about 50 ms; a hung server must
// not hold up the 2 s refresh.
const rexTimeout = time.Second

// findRex returns the rex CLI: PATH first, then the app bundle.
func findRex() (string, bool) {
	if p, err := exec.LookPath("rex"); err == nil {
		return p, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	p := filepath.Join(home, rexBundlePath)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
		return p, true
	}
	return "", false
}

// rexCLI runs the rex CLI from outside Rex. Every Rex call goes through it.
type rexCLI struct {
	path    string
	timeout time.Duration // 0 = rexTimeout
}

// rexTargetEnv are the variables Rex sets in each of its terminals. Inside Rex
// they make the caller's own block the default target, so they are dropped.
var rexTargetEnv = []string{"REX_SESSION=", "REX_BLOCK=", "REX_CLIENT="}

// command builds one rex call. --autostart=false: a poll must never start a
// Rex server.
func (r rexCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.path, append([]string{"--autostart=false"}, args...)...)
	for _, kv := range os.Environ() {
		if !hasAnyPrefix(kv, rexTargetEnv) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd
}

// run calls rex with stdin and returns its stdout.
func (r rexCLI) run(stdin string, args ...string) ([]byte, error) {
	timeout := r.timeout
	if timeout == 0 {
		timeout = rexTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := r.command(ctx, args...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Output()
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

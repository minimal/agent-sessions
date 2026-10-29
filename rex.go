package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// rexBundlePath is where the Rex app keeps its CLI, relative to $HOME. Outside
// Rex (the Ghostty quick terminal, a login shell) `rex` is not on PATH, so
// findRex usually finds it here.
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
	cmd.WaitDelay = timeout // a child that keeps stdout open can't outlast the kill
	return cmd.Output()
}

// rexPollLua lists every terminal block in one rex call.
//
//go:embed rex_poll.lua
var rexPollLua string

// RexTarget names the Rex block hosting a live session.
type RexTarget struct {
	SessionID string `json:"session_id"`
	WindowID  string `json:"window_id"`
	BlockID   string `json:"block_id"`
	Label     string `json:"label"` // the Rex session's label
}

// Name is the display form, label:shortid. Block ids are too long to show.
func (r RexTarget) Name() string {
	short := r.BlockID
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	return r.Label + ":" + short
}

// rexBlock is one terminal block from a poll.
type rexBlock struct {
	RexTarget
	FG    int `json:"fg"`    // the foreground process; a Claude registry pid matches it
	Child int `json:"child"` // the block's own process, often the shell
}

// poll lists every terminal block. Any error gives none: Rex rows then show
// as live rows with no mux.
func (r rexCLI) poll() []rexBlock {
	out, err := r.run(rexPollLua, "do", "-", "--json")
	if err != nil {
		return nil
	}
	return parseRexPoll(out)
}

// parseRexPoll reads a poll result. Lua writes an empty table as {}, which
// fails to decode as an array and so also gives none.
func parseRexPoll(data []byte) []rexBlock {
	var blocks []rexBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil
	}
	return slices.DeleteFunc(blocks, func(b rexBlock) bool { return b.BlockID == "" })
}

// rexBlockFor finds the block that pid runs in. The foreground pid usually
// matches. Otherwise walk pid's ancestors to a block's foreground or child,
// for an agent started under a wrapper.
func rexBlockFor(blocks []rexBlock, pid int) (rexBlock, bool) {
	byPID := map[int]rexBlock{}
	for _, b := range blocks {
		if b.Child != 0 {
			byPID[b.Child] = b
		}
	}
	for _, b := range blocks {
		if b.FG != 0 {
			byPID[b.FG] = b // a foreground pid wins over another block's child
		}
	}
	for p := pid; p > 1; p = parentPID(p) {
		if b, ok := byPID[p]; ok {
			return b, true
		}
	}
	return rexBlock{}, false
}

// hasAnyPrefix reports whether s starts with one of prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

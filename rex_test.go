package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// fakeRex writes an executable named rex at path.
func fakeRex(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindRexPrefersPath(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	fakeRex(t, filepath.Join(home, rexBundlePath))
	fakeRex(t, filepath.Join(bin, "rex"))

	got, ok := findRex()
	if !ok || got != filepath.Join(bin, "rex") {
		t.Errorf("findRex() = %q, %v; want the PATH binary", got, ok)
	}
}

func TestFindRexFallsBackToTheBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	fakeRex(t, filepath.Join(home, rexBundlePath))

	got, ok := findRex()
	if !ok || got != filepath.Join(home, rexBundlePath) {
		t.Errorf("findRex() = %q, %v; want the bundle binary", got, ok)
	}
}

func TestFindRexNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if got, ok := findRex(); ok {
		t.Errorf("findRex() = %q, want not found", got)
	}
}

func TestRexCommandNeverAutostartsAndDropsTargets(t *testing.T) {
	t.Setenv("REX_SESSION", "session:x")
	t.Setenv("REX_BLOCK", "block:x")
	t.Setenv("REX_CLIENT", "client:x")
	t.Setenv("REX_SERVER", "unix:///tmp/rex.sock")

	cmd := rexCLI{path: "/opt/rex"}.command(context.Background(), "do", "-")
	if want := []string{"/opt/rex", "--autostart=false", "do", "-"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	for _, kv := range cmd.Env {
		for _, name := range []string{"REX_SESSION=", "REX_BLOCK=", "REX_CLIENT="} {
			if strings.HasPrefix(kv, name) {
				t.Errorf("env keeps %s", kv)
			}
		}
	}
	// The server address is not a target: it says which Rex to ask.
	if !slices.Contains(cmd.Env, "REX_SERVER=unix:///tmp/rex.sock") {
		t.Error("env should keep REX_SERVER")
	}
}

func TestRexCommandTimesOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := (rexCLI{path: path, timeout: 50 * time.Millisecond}).run("", "do", "-"); err == nil {
		t.Error("a hung rex should fail")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("run took %s, want it cut at the timeout", d)
	}
}

func TestRexModeWithoutTheCLINotices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.RexMode = true
	m := newModel(cfg)
	if !strings.Contains(m.notice, "rex") {
		t.Errorf("notice = %q, want a missing-rex notice", m.notice)
	}
	if m.rexMode {
		t.Error("rex mode should be off when the CLI is missing")
	}
}

func TestRexPlaceholderExpandsToTheBinary(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	m := glyphModel()
	m.bgExec = true
	m.rexBin = "/Applications/Rex Beta.app/rex"
	m.sessions = []Session{{ID: "x", Modified: time.Now()}}
	_, cmd := m.runCommand("printf %s {rex} > " + shellQuote(out))
	if cmd == nil {
		t.Fatal("a {rex} template should run in rex mode")
	}
	if done := cmd().(execDoneMsg); done.err != nil {
		t.Fatal(done.err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != m.rexBin {
		t.Errorf("{rex} = %q, want %q", got, m.rexBin)
	}
}

func TestRexPlaceholderNeedsRexMode(t *testing.T) {
	m := glyphModel()
	m.sessions = []Session{{ID: "x", Modified: time.Now()}}
	after, cmd := m.runCommand("{rex} ls")
	if cmd != nil {
		t.Error("a {rex} template should not run without the CLI")
	}
	if !strings.Contains(after.(model).notice, "--rex") {
		t.Errorf("notice = %q, want a hint to use --rex", after.(model).notice)
	}
}

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestMain keeps the command log of detached test commands out of the
// user's state directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-sessions-state")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestParseFlagsRexDefaultsOff(t *testing.T) {
	opts, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.rex {
		t.Error("rex mode should be off without --rex")
	}
}

func TestParseFlagsRex(t *testing.T) {
	for _, arg := range []string{"--rex", "-rex"} {
		opts, err := parseFlags([]string{arg}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if !opts.rex {
			t.Errorf("%s should turn rex mode on", arg)
		}
	}
}

func TestParseFlagsHelpListsRex(t *testing.T) {
	var out bytes.Buffer
	if _, err := parseFlags([]string{"--help"}, &out); err == nil {
		t.Fatal("--help should stop the program")
	}
	if !strings.Contains(out.String(), "-rex") {
		t.Errorf("--help should list -rex, got:\n%s", out.String())
	}
}

func TestParseFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseFlags([]string{"--nope"}, &bytes.Buffer{}); err == nil {
		t.Error("an unknown flag should be an error")
	}
}

func TestStatusShowsRexMode(t *testing.T) {
	m := testModel(previewColumn, nil)
	m.applyFilter()
	if strings.Contains(m.status, "rex") {
		t.Errorf("status without rex mode = %q, want no rex", m.status)
	}
	m.rexBin = "/opt/rex"
	m.applyFilter()
	if !strings.Contains(m.status, "rex") {
		t.Errorf("status in rex mode = %q, want rex", m.status)
	}
}

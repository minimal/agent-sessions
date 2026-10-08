package main

import (
	"testing"
)

// tmuxPanesFixture mirrors a real `tmux list-panes -a` result: sessions in
// tmux's order, several panes per session, and a session whose name contains
// the ':' and '.' separators the pane names use.
const tmuxPanesFixture = "\n" +
	"$0\t_home\t1\t%0\t_home:0.0\n" +
	"$4\tagent-sessions\t5\t%6\tagent-sessions:0.0\n" +
	"$4\tagent-sessions\t5\t%13\tagent-sessions:1.0\n" +
	"$4\tagent-sessions\t5\t%7\tagent-sessions:2.0\n" +
	"$5\tagent-sessions_latest\t2\t%8\tagent-sessions_latest:0.0\n" +
	"$5\tagent-sessions_latest\t2\t%12\tagent-sessions_latest:3.0\n" +
	"$6\tx:y\t1\t%19\tx:y:0.0\n" +
	"$7\tx.z\t1\t%20\tx.z:0.0\n"

func TestParseTmuxPanesKeepsTmuxOrderAndWindows(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	want := []tmuxSession{
		{id: "$0", name: "_home", windows: 1},
		{id: "$4", name: "agent-sessions", windows: 5},
		{id: "$5", name: "agent-sessions_latest", windows: 2},
		{id: "$6", name: "x:y", windows: 1},
		{id: "$7", name: "x.z", windows: 1},
	}
	if len(srv.sessions) != len(want) {
		t.Fatalf("got %d sessions, want %d: %+v", len(srv.sessions), len(want), srv.sessions)
	}
	for i, w := range want {
		if got := srv.sessions[i]; got != w {
			t.Errorf("session %d = %+v, want %+v (tmux's own order, one entry per session)", i, got, w)
		}
	}
}

func TestParseTmuxPanesKeysBothPaneFormsToSessionName(t *testing.T) {
	srv := parseTmuxPanes(tmuxPanesFixture)
	cases := map[string]string{
		"%6":                        "agent-sessions", // pi's marker form
		"agent-sessions:1.0":        "agent-sessions", // claude's registry form
		"%12":                       "agent-sessions_latest",
		"agent-sessions_latest:3.0": "agent-sessions_latest",
		"x:y:0.0":                   "x:y", // a ':' in the session name is not a separator
		"x.z:0.0":                   "x.z", // nor is a '.' at the end of a name
	}
	for key, want := range cases {
		if got, ok := srv.byPane[key]; !ok || got != want {
			t.Errorf("byPane[%q] = %q (present=%v), want %q", key, got, ok, want)
		}
	}
	// A splitter that took the text before the first ':' would attribute this
	// pane to a session named "x", which does not exist.
	if got := srv.byPane["x:y:0.0"]; got == "x" {
		t.Error("pane forms must be matched exactly, never split on ':'")
	}
}

func TestParseTmuxPanesIgnoresJunkAndEmptyOutput(t *testing.T) {
	srv := parseTmuxPanes("garbage\n$1\tonly-two\tfields\n\n")
	if len(srv.sessions) != 0 {
		t.Errorf("junk lines should be skipped, got %+v", srv.sessions)
	}
	if len(srv.byPane) != 0 {
		t.Errorf("junk lines should add no attribution, got %v", srv.byPane)
	}
	if srv = parseTmuxPanes(""); len(srv.sessions) != 0 {
		t.Errorf("an empty listing should hold no sessions, got %+v", srv.sessions)
	}
}

func TestTmuxCurrentSession(t *testing.T) {
	cases := []struct{ tmux, want string }{
		{"", ""},                                // outside tmux
		{"/tmp/tmux-1000/default,1290,4", "$4"}, // socket, server pid, session id
		{"/tmp/tmux-1000/default,1290,", ""},    // no session id
		{"/tmp/tmux-1000/default,4", ""},        // not the three-field form
	}
	for _, c := range cases {
		t.Setenv("TMUX", c.tmux)
		if got := tmuxCurrentSession(); got != c.want {
			t.Errorf("tmuxCurrentSession() with TMUX=%q = %q, want %q", c.tmux, got, c.want)
		}
	}
}

// TestPollTmuxLive checks the format strings against a real server. It is the
// only test that can catch a mistyped tmux variable, which would silently make
// every chip disappear.
func TestPollTmuxLive(t *testing.T) {
	srv := pollTmux()
	if srv == nil {
		t.Skip("no reachable tmux server")
	}
	if len(srv.sessions) == 0 {
		t.Skip("tmux server has no sessions")
	}
	names := map[string]bool{}
	for _, s := range srv.sessions {
		if s.name == "" {
			t.Errorf("session %q has no name", s.id)
		}
		if s.windows < 1 {
			t.Errorf("session %q reports %d windows", s.name, s.windows)
		}
		names[s.name] = true
	}
	if len(srv.byPane) == 0 {
		t.Error("a server with sessions should attribute at least one pane")
	}
	for key, name := range srv.byPane {
		if !names[name] {
			t.Errorf("pane %q maps to unknown session %q", key, name)
		}
	}
	if srv.current != "" {
		found := false
		for _, s := range srv.sessions {
			found = found || s.id == srv.current
		}
		if !found {
			t.Errorf("current session %q is not on the server: %+v", srv.current, srv.sessions)
		}
	}
}

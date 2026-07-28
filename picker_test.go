package main

import (
	"slices"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFuzzyMatch(t *testing.T) {
	cases := []struct {
		query, s string
		want     bool
	}{
		{"", "anything", true},
		{"agent", "agent-sessions", true},
		{"agt", "agent-sessions", true},    // subsequence, not substring
		{"AGT", "agent-sessions", true},    // case-insensitive
		{"ss", "agent-sessions", true},     // repeated runes each need a match
		{"sssss", "agent-sessions", false}, // more s's than the string has
		{"tneg", "agent-sessions", false},  // out of order
		{"xyz", "agent-sessions", false},   // missing runes
		{"~/pr", "~/Projects/agent", true}, // matches the displayed label
		{"é", "café", true},                // rune-wise, not byte-wise
	}
	for _, c := range cases {
		if got := fuzzyMatch(c.query, c.s); got != c.want {
			t.Errorf("fuzzyMatch(%q, %q) = %v, want %v", c.query, c.s, got, c.want)
		}
	}
}

func TestBranchList(t *testing.T) {
	m := model{all: []Session{
		{ID: "a", CWD: "/p/one", Branch: "main"},
		{ID: "b", CWD: "/p/two", Branch: "feature"},
		{ID: "c", CWD: "/p/one", Branch: "main"}, // duplicate
		{ID: "d", CWD: "/p/one", Branch: "topic"},
		{ID: "e", CWD: "/p/one", Branch: ""}, // no branch: skipped
	}}
	want := []string{"main", "feature", "topic"}
	if got := m.branchList(); !slices.Equal(got, want) {
		t.Errorf("branchList() = %v, want %v", got, want)
	}
	// With a project filter active, only that project's branches are offered.
	m.project = "/p/one"
	want = []string{"main", "topic"}
	if got := m.branchList(); !slices.Equal(got, want) {
		t.Errorf("branchList() with project filter = %v, want %v", got, want)
	}
}

func TestApplyFilterBranch(t *testing.T) {
	m := model{all: []Session{
		{ID: "a", CWD: "/p/one", Branch: "main"},
		{ID: "b", CWD: "/p/one", Branch: "feature"},
		{ID: "c", CWD: "/p/two", Branch: "main"},
	}}
	m.branch = "main"
	m.applyFilter()
	if len(m.sessions) != 2 || m.sessions[0].ID != "a" || m.sessions[1].ID != "c" {
		t.Errorf("branch filter: got %v, want sessions a and c", m.sessions)
	}
	// Branch and project filters combine.
	m.project = "/p/one"
	m.applyFilter()
	if len(m.sessions) != 1 || m.sessions[0].ID != "a" {
		t.Errorf("branch+project filter: got %v, want only session a", m.sessions)
	}
}

// key builds the KeyMsg a keypress produces, for the keys the tests use.
func key(s string) tea.KeyMsg {
	if s == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestFilterMenu(t *testing.T) {
	newModel := func() model {
		return model{all: []Session{
			{ID: "a", CWD: "/p/one", Branch: "main"},
			{ID: "b", CWD: "/p/two", Branch: "feature"},
		}}
	}
	press := func(m model, k string) model {
		mm, _ := m.Update(key(k))
		return mm.(model)
	}

	// f opens the menu; p opens the project picker over both projects.
	m := press(newModel(), "f")
	if !m.menu.active {
		t.Fatal("f should open the filter menu")
	}
	m = press(m, "p")
	if m.menu.active {
		t.Error("choosing a menu entry should close the menu")
	}
	if !m.picker.active || !slices.Equal(m.picker.items, []string{"/p/one", "/p/two"}) {
		t.Errorf("f p should open the project picker, got %+v", m.picker)
	}

	// f b opens the branch picker.
	m = press(press(newModel(), "f"), "b")
	if !m.picker.active || !slices.Equal(m.picker.items, []string{"main", "feature"}) {
		t.Errorf("f b should open the branch picker, got %+v", m.picker)
	}

	// Esc and unbound keys cancel the menu without opening anything.
	for _, k := range []string{"esc", "x"} {
		m = press(press(newModel(), "f"), k)
		if m.menu.active || m.picker.active {
			t.Errorf("%q should cancel the menu, got menu=%v picker=%v", k, m.menu.active, m.picker.active)
		}
	}
}

func TestPickerApplyQuery(t *testing.T) {
	p := pickerState{
		all:   []string{"/home/a/alpha", "/home/a/beta", "/home/a/gamma"},
		items: []string{"/home/a/alpha", "/home/a/beta", "/home/a/gamma"},
	}
	p.query = "eta"
	p.applyQuery()
	if len(p.items) != 1 || p.items[0] != "/home/a/beta" {
		t.Errorf("query %q: got %v, want only /home/a/beta", p.query, p.items)
	}
	if p.cursor != 0 || p.offset != 0 {
		t.Errorf("applyQuery should reset cursor/offset, got %d/%d", p.cursor, p.offset)
	}
	p.query = ""
	p.applyQuery()
	if len(p.items) != 3 {
		t.Errorf("empty query: got %d items, want all 3", len(p.items))
	}
}

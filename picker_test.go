package main

import "testing"

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

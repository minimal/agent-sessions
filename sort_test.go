package main

import (
	"testing"
	"time"
)

var sortBase = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return sortBase.Add(time.Duration(min) * time.Minute) }

// order returns the session IDs in slice order, for compact assertions.
func order(sessions []Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

func sortFixtures() []Session {
	return []Session{
		{ID: "a-old", Repo: "A", Activity: at(-50), Modified: at(-50)},
		{ID: "a-new", Repo: "A", Activity: at(-20), Modified: at(-20)},
		{ID: "b-live", Repo: "B", Activity: at(-30), Modified: at(-30), PID: 111},
		{ID: "b-old", Repo: "B", Activity: at(-40), Modified: at(-40)},
		{ID: "c-new", Repo: "C", Activity: at(-10), Modified: at(-10)},
	}
}

func TestSortUngroupedFloatsLive(t *testing.T) {
	s := sortFixtures()
	sortSessions(s, nil)
	// b-live floats to the top; the rest follow by activity, newest first.
	want := []string{"b-live", "c-new", "a-new", "b-old", "a-old"}
	if got := order(s); !equalStrings(got, want) {
		t.Errorf("ungrouped order = %v, want %v", got, want)
	}
}

func TestSortGroupsByRepo(t *testing.T) {
	s := sortFixtures()
	sortSessions(s, parseSortDims("repo"))
	// Repo B holds the live session so its block comes first (live-first
	// inside), then repos by recency: C (newest) before A. Worktrees of one
	// repo stay adjacent.
	want := []string{"b-live", "b-old", "c-new", "a-new", "a-old"}
	if got := order(s); !equalStrings(got, want) {
		t.Errorf("grouped order = %v, want %v", got, want)
	}
}

// buriedFixtures models the reported pain: repo X has one live session but a
// pile of recent finished ones, while repo Y's live session is older.
func buriedFixtures() []Session {
	return []Session{
		{ID: "x-live", Repo: "X", Activity: at(-40), Modified: at(-40), PID: 1},
		{ID: "x-done1", Repo: "X", Activity: at(-5), Modified: at(-5)},
		{ID: "x-done2", Repo: "X", Activity: at(-6), Modified: at(-6)},
		{ID: "y-live", Repo: "Y", Activity: at(-50), Modified: at(-50), PID: 2},
	}
}

func TestSortRepoBuriesActive(t *testing.T) {
	s := buriedFixtures()
	sortSessions(s, parseSortDims("repo"))
	// Plain repo grouping keeps X's block whole, so its finished sessions push
	// Y's live session to the very bottom — the behaviour we want to escape.
	want := []string{"x-live", "x-done1", "x-done2", "y-live"}
	if got := order(s); !equalStrings(got, want) {
		t.Errorf("repo order = %v, want %v", got, want)
	}
}

func TestSortActiveThenRepo(t *testing.T) {
	s := buriedFixtures()
	sortSessions(s, parseSortDims("active,repo"))
	// active,repo surfaces every live session first (clustered by repo), so
	// y-live rises above X's finished sessions, which sink to the bottom.
	want := []string{"x-live", "y-live", "x-done1", "x-done2"}
	if got := order(s); !equalStrings(got, want) {
		t.Errorf("active,repo order = %v, want %v", got, want)
	}
}

func TestParseSortDims(t *testing.T) {
	cases := map[string][]sortDim{
		"":                nil,
		"activity":        nil,
		"repo":            {dimRepo},
		"active,repo":     {dimActive, dimRepo},
		" active , repo ": {dimActive, dimRepo},
		"repo,active":     {dimRepo, dimActive},
		"active,active":   {dimActive}, // duplicates collapse
		"live":            {dimActive}, // alias
	}
	for in, want := range cases {
		got := parseSortDims(in)
		if len(got) != len(want) {
			t.Errorf("parseSortDims(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("parseSortDims(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

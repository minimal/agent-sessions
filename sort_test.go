package main

import (
	"strings"
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

// jitterFixtures models the reported pain: two repos whose newest activity
// differs only by seconds. Without bucketing, the groups swap on every
// refresh as messages arrive.
func TestSortRepoStableWithinBucket(t *testing.T) {
	s := []Session{
		{ID: "b-live", Repo: "B", Activity: sortBase.Add(10 * time.Second), Modified: sortBase.Add(10 * time.Second), PID: 1},
		{ID: "a-live", Repo: "A", Activity: sortBase.Add(50 * time.Second), Modified: sortBase.Add(50 * time.Second), PID: 2},
	}
	sortSessions(s, parseSortDims("repo"))
	// Both groups' activity falls in the same minute, so the order is
	// alphabetical, not by the 40-second difference in recency.
	want := []string{"a-live", "b-live"}
	if got := order(s); !equalStrings(got, want) {
		t.Errorf("order = %v, want %v (groups must not shuffle within a minute)", got, want)
	}
}

// TestSortRepoMixedBuckets mixes a within-minute pair (A, B) with a
// cross-minute group (C) in one sort. With raw timestamps (the pre-fix
// behaviour) B would edge out A, so this only passes when both the bucketed
// recency and the alphabetical tiebreak are in effect.
func TestSortRepoMixedBuckets(t *testing.T) {
	s := []Session{
		{ID: "a", Repo: "A", Activity: sortBase.Add(10 * time.Second), Modified: sortBase.Add(10 * time.Second)},
		{ID: "b", Repo: "B", Activity: sortBase.Add(50 * time.Second), Modified: sortBase.Add(50 * time.Second)},
		{ID: "c", Repo: "C", Activity: sortBase.Add(2 * time.Minute), Modified: sortBase.Add(2 * time.Minute)},
	}
	sortSessions(s, parseSortDims("repo"))
	// C's group is a full minute newer, so it comes first. A and B fall in
	// the same minute, so they keep alphabetical order.
	want := []string{"c", "a", "b"}
	if got := order(s); !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
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

func TestSameSortDims(t *testing.T) {
	if !sameSortDims(parseSortDims("active, repo"), parseSortDims("ACTIVE,REPO")) {
		t.Error("spelling variants of a dimension list should compare equal")
	}
	if sameSortDims(parseSortDims("active,repo"), parseSortDims("repo")) {
		t.Error("different dimension lists should not compare equal")
	}
}

func TestNextSortGroup(t *testing.T) {
	cases := map[string]string{
		"activity":     "repo",
		"repo":         "active,repo",
		"active,repo":  "activity",
		"active, repo": "activity", // spelling variants match a preset
		"ACTIVE,REPO":  "activity",
		"":             "repo", // no [sort] section: plain recency seeds as activity
		"repo,active":  "repo", // custom order: the cycle restarts at activity
		"live":         "repo", // the alias is not one of the three documented presets
		"garbage":      "repo",
	}
	for in, want := range cases {
		if got := nextSortGroup(in); got != want {
			t.Errorf("nextSortGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

// sortKeyFixtures separates all three presets: plain recency interleaves the
// finished sessions of different repos, plain repo grouping keeps each repo
// together (repos ordered by their newest activity), and active,repo floats
// both live sessions above the finished ones.
func sortKeyFixtures() []Session {
	return []Session{
		{ID: "z-live", Repo: "Z", Activity: at(-100), Modified: at(-100), PID: 1},
		{ID: "z-done", Repo: "Z", Activity: at(-2), Modified: at(-2)},
		{ID: "a-done", Repo: "A", Activity: at(-3), Modified: at(-3)},
		{ID: "y-live", Repo: "Y", Activity: at(-90), Modified: at(-90), PID: 2},
		{ID: "y-done", Repo: "Y", Activity: at(-4), Modified: at(-4)},
	}
}

func TestSortKeyCyclesPresets(t *testing.T) {
	m := model{all: sortKeyFixtures(), sortGroup: "activity", configuredSortDims: parseSortDims("activity")}
	m.sessions = m.all
	steps := []struct {
		preset string
		want   []string
	}{
		{"repo", []string{"z-live", "z-done", "y-live", "y-done", "a-done"}},
		{"active,repo", []string{"z-live", "y-live", "z-done", "y-done", "a-done"}},
		{"activity", []string{"y-live", "z-live", "z-done", "a-done", "y-done"}},
	}
	for _, step := range steps {
		mm, _ := m.Update(key("s"))
		m = mm.(model)
		if m.sortGroup != step.preset {
			t.Fatalf("s should cycle to %q, got %q", step.preset, m.sortGroup)
		}
		if got := order(m.sessions); !equalStrings(got, step.want) {
			t.Errorf("after s -> %q: %v, want %v", step.preset, got, step.want)
		}
	}
}

// TestSortKeySurvivesReload presses s and then delivers a load the way the
// periodic refresh does: the list must come back in the runtime mode, not the
// configured one.
func TestSortKeySurvivesReload(t *testing.T) {
	m := model{
		all:                sortKeyFixtures(),
		sortGroup:          "activity",
		configuredSortDims: parseSortDims("activity"),
		seen:               map[string]SessionState{},
		unread:             map[string]bool{},
	}
	m.sessions = m.all
	mm, _ := m.Update(key("s"))
	m = mm.(model)
	mm, _ = m.Update(sessionsLoadedMsg{sessions: sortKeyFixtures()})
	m = mm.(model)
	want := []string{"z-live", "z-done", "y-live", "y-done", "a-done"} // repo order
	if got := order(m.sessions); !equalStrings(got, want) {
		t.Errorf("after a reload: %v, want %v", got, want)
	}
}

// TestSortKeyKeepsCursorOnSession pins the selection across a re-sort: the
// bottom row under plain recency moves up when repo grouping applies.
func TestSortKeyKeepsCursorOnSession(t *testing.T) {
	m := model{all: sortKeyFixtures(), sortGroup: "activity"}
	m.sessions = m.all
	m.cursor = len(m.sessions) - 1
	want := m.sessions[m.cursor].ID
	mm, _ := m.Update(key("s"))
	m = mm.(model)
	if got := m.sessions[m.cursor].ID; got != want {
		t.Errorf("cursor should stay on %q, got %q", want, got)
	}
}

func TestSortStatusPart(t *testing.T) {
	m := model{all: sortKeyFixtures(), sortGroup: "activity", configuredSortDims: parseSortDims("activity")}
	m.sessions = m.all
	m.applyFilter()
	if strings.Contains(m.status, "sorted by") {
		t.Errorf("the configured order should not add a status part, got %q", m.status)
	}
	m.setSortGroup("repo")
	if !strings.Contains(m.status, "sorted by repo") {
		t.Errorf("status should name the runtime order, got %q", m.status)
	}
	// A spelling variant of the configured preset is the same mode.
	m.configuredSortDims = parseSortDims("active, repo")
	m.setSortGroup("active,repo")
	if strings.Contains(m.status, "sorted by") {
		t.Errorf("a spelling variant of the configured mode should not add a part, got %q", m.status)
	}
}

func TestRepoColorStableAndScoped(t *testing.T) {
	m := model{repoColors: repoPalette(nil)}
	if c := m.repoColor("A"); c == nil || m.repoColor("A") != c {
		t.Errorf("repoColor should be non-nil and stable for a repo")
	}
	if m.repoColor("") != nil {
		t.Errorf("repoColor(\"\") should be nil")
	}
	// No palette configured -> no tint even for a real repo.
	none := model{repoColors: nil}
	if none.repoColor("A") != nil {
		t.Errorf("repoColor should be nil when no palette is configured")
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

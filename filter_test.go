package main

import (
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	week  = 7 * 24 * time.Hour
	month = 30 * 24 * time.Hour
)

func TestNextAgeWindow(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{0, week},
		{week, month},
		{month, 0},            // wraps back to "all"
		{2 * time.Hour, week}, // custom window: the cycle restarts at "all"
	}
	for _, c := range cases {
		if got := nextAgeWindow(c.in); got != c.want {
			t.Errorf("nextAgeWindow(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAgeWindowLabel(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "",
		week:             "last 7 days",
		month:            "last 30 days",
		24 * time.Hour:   "last 24h",
		90 * time.Minute: "last 90m",
		36 * time.Hour:   "last 36h",
	}
	for in, want := range cases {
		if got := ageWindowLabel(in); got != want {
			t.Errorf("ageWindowLabel(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterWithinConfig(t *testing.T) {
	cases := []struct {
		within   string
		want     time.Duration
		wantWarn bool
	}{
		{"", 0, false},
		{"168h", week, false},
		{"0", 0, false},
		{"garbage", 0, true},
		{"-1h", 0, true},
	}
	for _, c := range cases {
		var cfg Config
		cfg.Filter.Within = c.within
		got, warn := cfg.FilterWithin()
		if got != c.want || (warn != "") != c.wantWarn {
			t.Errorf("FilterWithin(%q) = %v, %q; want %v, warn=%v", c.within, got, warn, c.want, c.wantWarn)
		}
	}
}

func TestDefaultConfigStartsWithNoWindow(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	if d, warn := cfg.FilterWithin(); d != 0 || warn != "" {
		t.Errorf("default [filter] within should mean all sessions, got %v %q", d, warn)
	}
}

// ageFixtures dates sessions relative to now: finished ones at 3, 10 and 40
// days, a stub with no Activity that falls back to its mtime, and a live
// session whose last entry is older than every window.
func ageFixtures(now time.Time) []Session {
	return []Session{
		{ID: "3d", Activity: now.Add(-3 * 24 * time.Hour), Modified: now.Add(-3 * 24 * time.Hour)},
		{ID: "10d", Activity: now.Add(-10 * 24 * time.Hour), Modified: now.Add(-10 * 24 * time.Hour)},
		{ID: "40d", Activity: now.Add(-40 * 24 * time.Hour), Modified: now.Add(-40 * 24 * time.Hour)},
		{ID: "stub", Modified: now.Add(-time.Hour)},
		{ID: "live-old", Activity: now.Add(-40 * 24 * time.Hour), Modified: now.Add(-40 * 24 * time.Hour), PID: 42},
	}
}

func ageTestModel(now time.Time) model {
	m := model{all: ageFixtures(now), now: func() time.Time { return now }}
	m.sessions = m.all
	return m
}

func TestApplyFilterAgeWindow(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		window time.Duration
		want   []string
	}{
		{0, []string{"3d", "10d", "40d", "stub", "live-old"}},
		{week, []string{"3d", "stub", "live-old"}},
		{month, []string{"3d", "10d", "stub", "live-old"}},
	}
	for _, c := range cases {
		m := ageTestModel(now)
		m.ageWindow = c.window
		m.applyFilter()
		if got := order(m.sessions); !equalStrings(got, c.want) {
			t.Errorf("window %v: got %v, want %v", c.window, got, c.want)
		}
	}
}

func TestAgeKeyCycles(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	m := ageTestModel(now)
	steps := []struct {
		window time.Duration
		count  int
	}{
		{week, 3},
		{month, 4},
		{0, 5},
	}
	for _, step := range steps {
		mm, _ := m.Update(key("a"))
		m = mm.(model)
		if m.ageWindow != step.window {
			t.Fatalf("a should cycle to %v, got %v", step.window, m.ageWindow)
		}
		if len(m.sessions) != step.count {
			t.Errorf("window %v: %d rows, want %d", step.window, len(m.sessions), step.count)
		}
	}
}

func TestAgeWindowStatusPart(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	m := ageTestModel(now)
	for _, c := range []struct {
		window time.Duration
		part   string
	}{
		{week, "last 7 days"},
		{month, "last 30 days"},
		{0, ""},
	} {
		m.ageWindow = c.window
		m.applyFilter()
		if c.part == "" {
			if strings.Contains(m.status, "last ") {
				t.Errorf("no window: status should not name one, got %q", m.status)
			}
			continue
		}
		if !strings.Contains(m.status, c.part) {
			t.Errorf("window %v: status %q should contain %q", c.window, m.status, c.part)
		}
	}
}

func TestEscClearsAgeWindow(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	m := ageTestModel(now)
	m.ageWindow = week
	m.applyFilter()
	mm, _ := m.Update(key("esc"))
	m = mm.(model)
	if m.ageWindow != 0 || len(m.sessions) != 5 {
		t.Errorf("esc should clear the age window, got %v and %d rows", m.ageWindow, len(m.sessions))
	}
}

func TestMalformedFilterWithinWarns(t *testing.T) {
	var cfg Config
	cfg.Filter.Within = "garbage"
	m := newModel(cfg)
	if m.ageWindow != 0 {
		t.Errorf("a malformed [filter] within must not hide sessions, got %v", m.ageWindow)
	}
	if !strings.Contains(m.notice, "within") {
		t.Errorf("a malformed [filter] within should warn on startup, got %q", m.notice)
	}

	cfg.Filter.Within = "168h"
	m = newModel(cfg)
	if m.ageWindow != week {
		t.Errorf("a valid [filter] within should seed the window, got %v", m.ageWindow)
	}
	if m.notice != "" {
		t.Errorf("a valid [filter] within should not warn, got %q", m.notice)
	}
}

func TestFilterLiveKey(t *testing.T) {
	cases := []struct {
		live, running bool
		want          bool
		wantWarn      bool
	}{
		{false, false, false, false}, // the shipped default
		{true, false, true, false},   // the new key
		{false, true, true, true},    // the deprecated alias still turns it on
		{true, true, true, true},     // both: it filters, and still warns
	}
	for _, c := range cases {
		var cfg Config
		cfg.Filter.Live, cfg.Filter.Running = c.live, c.running
		got, warn := cfg.filterLive()
		if got != c.want || (warn != "") != c.wantWarn {
			t.Errorf("filterLive() with live=%v running=%v = (%v, %q), want (%v, warn=%v)",
				c.live, c.running, got, warn, c.want, c.wantWarn)
		}
	}
}

func TestDeprecatedRunningKeyStillFilters(t *testing.T) {
	// loadConfig decodes the shipped defaults first, then the user's file over
	// them, so an old config that only knows `running` must still filter.
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := toml.Decode("[filter]\nrunning = true\n", &cfg); err != nil {
		t.Fatal(err)
	}
	m := newModel(cfg)
	if !m.liveOnly {
		t.Error("[filter] running = true should still apply the liveness filter")
	}
	if !strings.Contains(m.notice, "deprecated") {
		t.Errorf("the deprecated key should warn on the status bar, got %q", m.notice)
	}
	// The new key is silent, and the old one cannot switch it off.
	cfg.Filter.Live, cfg.Filter.Running = true, false
	if m := newModel(cfg); !m.liveOnly || m.notice != "" {
		t.Errorf("[filter] live = true should filter silently, got liveOnly=%v notice=%q", m.liveOnly, m.notice)
	}
	cfg.Filter.Live, cfg.Filter.Running = true, true
	if m := newModel(cfg); !m.liveOnly {
		t.Error("the deprecated key must not turn off a live = true beside it")
	}
}

func TestDefaultConfigUsesLiveNotRunning(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(defaultConfigTOML, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Filter.Live || cfg.Filter.Running {
		t.Errorf("the shipped default should show every session, got live=%v running=%v",
			cfg.Filter.Live, cfg.Filter.Running)
	}
	if _, warn := cfg.filterLive(); warn != "" {
		t.Errorf("the shipped default should use no deprecated key, got %q", warn)
	}
	section := defaultConfigTOML[strings.Index(defaultConfigTOML, "[filter]"):]
	if j := strings.Index(section[1:], "\n["); j >= 0 {
		section = section[:j+1]
	}
	if !strings.Contains(section, "live = false") {
		t.Errorf("the shipped [filter] section should set live, got:\n%s", section)
	}
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "running = ") {
			t.Errorf("the shipped [filter] section should not set the deprecated running, got %q", line)
		}
	}
}

func TestStatusSaysLiveOnly(t *testing.T) {
	m := ageTestModel(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
	m.liveOnly = true
	m.applyFilter()
	if !strings.Contains(m.status, "live only") {
		t.Errorf("the status should say live only, got %q", m.status)
	}
	if strings.Contains(m.status, "running only") {
		t.Errorf("the status should never say running only, got %q", m.status)
	}
	// o still toggles the filter, in both directions.
	mm, _ := m.Update(key("o"))
	off := mm.(model)
	if off.liveOnly || strings.Contains(off.status, "live only") {
		t.Errorf("o should clear the filter, got liveOnly=%v status=%q", off.liveOnly, off.status)
	}
	mm, _ = off.Update(key("o"))
	if on := mm.(model); !on.liveOnly || !strings.Contains(on.status, "live only") {
		t.Errorf("o should apply the filter again, got liveOnly=%v status=%q", on.liveOnly, on.status)
	}
}

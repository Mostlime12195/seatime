package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/idle"
	"seatime/internal/client/presence"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

func TestDoctorReportIsPlainAndHomeRelative(t *testing.T) {
	home := hookEnv(t)
	now := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	st.Append(event.Event{Kind: event.Prompt, Harness: "claude-code", TS: now.Add(-3 * time.Minute)})
	// the lid shut from 08:00 to 17:58: one gap between idle windows
	st.Append(event.Event{Kind: event.Idle, Harness: "seatime", TS: now.Add(-10 * time.Hour), IdleSrc: "ioreg", WindowMS: 600000})
	st.Append(event.Event{Kind: event.Idle, Harness: "seatime", TS: now.Add(-time.Minute), IdleSrc: "ioreg", WindowMS: 60000})
	st.SetMeta("clock_offset_ms", "-185000")
	st.SetMeta("clock_offset_at", strconv.FormatInt(now.Add(-time.Hour).Unix(), 10))
	st.Close()
	logs := []string{
		now.Add(-30*time.Hour).Format("2006-01-02T15:04:05.000Z07:00") + " sync failed (1): old",
		now.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z07:00") + " hook: database unavailable (open " + home + "/Library/x: denied)",
		now.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z07:00") + " daemon starting",
	}
	var b bytes.Buffer
	writeReport(&b, reportEnv{
		now: now, home: home, version: "v1",
		osVer:    func() string { return "macOS 15.1" },
		agent:    func() string { return "launchd, agent loaded, running" },
		ping:     func() (map[string]any, bool) { return map[string]any{"version": "v1", "uptime_s": 120.0}, true },
		idle:     func() idle.Reading { return idle.Reading{Src: "ioreg", OK: true, Idle: 4 * time.Second} },
		harness:  func() []string { return []string{"claude-code: hooked"} },
		openSt:   openStore,
		logLines: func() []string { return logs },
		boot:     func() (time.Time, bool) { return now.Add(-3 * time.Minute), true },
	})
	out := b.String()
	for _, want := range []string{"summary:       2 to look at: clock; errors in the log", "os booted:     3m0s ago, daemon started with it",
		"sleep/wake:    1 gap(s) in the last 24 h", "longest 9h58m0s", "macOS 15.1", "agent loaded", "ioreg, last input 4s ago", "claude-code: hooked",
		"prompt from claude-code, 3 min ago", "-185s vs server", "CLOCK IS OFF", "errors (24h):  1", "~/Library/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, home) || strings.Contains(out, "): old") {
		t.Errorf("report leaks the home path or old errors:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n > 30 {
		t.Errorf("report should fit one screen, has %d lines", n)
	}
}

func TestDoctorReportWithoutIdleOrDaemon(t *testing.T) {
	hookEnv(t)
	var b bytes.Buffer
	writeReport(&b, reportEnv{
		now: time.Now(), home: "/Users/kid", version: "v1",
		osVer: func() string { return "x" }, agent: func() string { return "none" },
		ping:     func() (map[string]any, bool) { return nil, false },
		idle:     func() idle.Reading { return idle.Reading{Src: "none", Err: "no graphical session"} },
		harness:  func() []string { return nil },
		openSt:   openStore,
		logLines: func() []string { return nil },
		presence: func(ok bool) string { return presence.Level(ok, false, false, false) },
	})
	out := b.String()
	for _, want := range []string{"summary:", "daemon not running", "NOT running", "not measured", "presence:      not measured", "sleep/wake:    no gap", "last event:    none yet", "errors (24h):  none"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestTodayIsPlain(t *testing.T) {
	now := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	sm := stats.Summary{Now: now, Today: 5400, Week: 20000, GoalHours: 10, Streak: 3, EverRecorded: true,
		TodayByProj:  []stats.Named{{Name: "game", Seconds: 3600}, {Name: "site", Seconds: 1800}},
		AINotCounted: 1200}
	var b bytes.Buffer
	writeToday(&b, styler{}, sm)
	out := b.String()
	for _, want := range []string{"1h 30m today", "game", "site", "3-day streak", "agent time not counted today: 20m"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"struggle", "dark flow", "tier", "verified"} {
		if strings.Contains(out, banned) {
			t.Errorf("old vocabulary %q:\n%s", banned, out)
		}
	}
	b.Reset()
	writeToday(&b, styler{}, stats.Summary{Now: now, GoalHours: 10})
	if !strings.Contains(b.String(), "Nothing recorded yet") {
		t.Fatalf("empty state: %s", b.String())
	}
}

func TestStatuslineIsShortAndFast(t *testing.T) {
	hookEnv(t)
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// midday, so ten minutes back never crosses midnight
	now := stats.StartOfDay(time.Now(), time.Local).Add(12 * time.Hour)
	for m := 0; m < 20; m++ {
		st.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "local:app", TS: now.Add(-time.Duration(20-m) * 30 * time.Second)})
	}
	var b bytes.Buffer
	start := time.Now()
	writeStatusline(&b, st, "", now)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("statusline took %s", d)
	}
	if out := b.String(); !strings.HasPrefix(out, "seatime 9m") || strings.Contains(out, "\n") {
		t.Fatalf("%q", out)
	}
}

func TestParseBootTime(t *testing.T) {
	bt, ok := parseBootTime("{ sec = 1763700000, usec = 512345 } Fri Nov 21 04:40:00 2025\n")
	if !ok || bt.Unix() != 1763700000 {
		t.Fatalf("%v %v", bt, ok)
	}
	if _, ok := parseBootTime("garbage"); ok {
		t.Fatal("parsed garbage")
	}
}

// A clean machine says so in one line.
func TestDoctorReportClean(t *testing.T) {
	hookEnv(t)
	now := time.Now()
	var b bytes.Buffer
	writeReport(&b, reportEnv{
		now: now, home: "/Users/kid", version: "v1",
		osVer:    func() string { return "macOS 26.1" },
		agent:    func() string { return "launchd, agent loaded, running" },
		ping:     func() (map[string]any, bool) { return map[string]any{"version": "v1", "uptime_s": 600.0}, true },
		idle:     func() idle.Reading { return idle.Reading{Src: "ioreg", OK: true, Idle: time.Second} },
		harness:  func() []string { return []string{"claude-code: hooked"} },
		openSt:   openStore,
		logLines: func() []string { return nil },
		boot:     func() (time.Time, bool) { return now.Add(-11 * time.Minute), true },
	})
	if out := b.String(); !strings.Contains(out, "summary:       clean\n") || !strings.Contains(out, "daemon started with it") {
		t.Fatalf("not clean:\n%s", out)
	}
}

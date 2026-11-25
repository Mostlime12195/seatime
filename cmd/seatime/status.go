package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/canary"
	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/gitscan"
	"seatime/internal/client/setup"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/ledger"
)

func cmdStatus(args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	s := newStyler()
	cfg := config.Load()
	ping, alive := daemon.Ping(cfg)
	c := countingNow(st, cfg, ping, alive)
	if c.Yes {
		fmt.Println(s.green(c.Line()))
	} else {
		fmt.Println(s.orange(c.Line()))
	}
	if c.Note != "" {
		fmt.Println(c.Note)
	}
	fmt.Println(s.bold("seatime " + version))
	fmt.Println("data dir:  ", config.DataDir())
	if a, ok := config.LoadAuth(); ok {
		fmt.Printf("account:    %s (device %s) on %s\n", a.Username, a.DeviceID, a.APIBase(cfg))
	} else {
		fmt.Println("account:    not logged in (run `seatime login` to sync)")
	}
	if m, ok := ping, alive; ok {
		fmt.Printf("daemon:     %s (pid %.0f, up %.0fs)\n", s.green("running"), m["pid"], m["uptime_s"])
	} else {
		fmt.Printf("daemon:     %s (run `seatime setup`, or `seatime daemon`)\n", s.orange("not running"))
	}
	head, _, _ := st.Head()
	n, _ := st.Count()
	ss, _ := st.SyncState()
	fmt.Printf("events:     %d (head seq %d)\n", n, head)
	if ss.Time.IsZero() {
		fmt.Println("last sync:  never")
	} else {
		fmt.Printf("last sync:  %s, %d event(s) pending\n", ago(ss.Time), head-ss.Seq)
	}
	if v, _ := st.GetMeta(daemon.MetaSyncBlocked); v != "" {
		fmt.Println("sync:      ", s.red("stopped: "+v))
	} else if v, _ := st.GetMeta("last_sync_error"); v != "" {
		fmt.Println("sync error:", s.red(v))
	}
	if off, _, ok := syncer.ClockOffset(st); ok && (off > 2*time.Minute || off < -2*time.Minute) {
		fmt.Println("warning:   ", s.orange(fmt.Sprintf("this computer's clock is %s off the server's; hours are unaffected, but fix the clock (Settings > Date & Time)", (off.Abs()).Round(time.Second))))
	}
	if free, ok := diskFree(config.DataDir()); ok && free < 200<<20 {
		fmt.Println("warning:   ", s.orange(fmt.Sprintf("only %d MB of disk left; SeaTime keeps recording, but free some space", free>>20)))
	}
	if w := syncLagWarning(st, ss); w != "" {
		fmt.Println("warning:   ", s.orange(w))
	}
	if v, _ := st.GetMeta("server_message"); v != "" {
		fmt.Println("server says:", v)
	}
	queued, _ := st.QueuedChecks()
	switch {
	case st.PromptCheckOn():
		fmt.Printf("prompt check: on (the server checks each prompt that is not a repeat; its text is sent for the check and not kept), %d waiting to be sent\n", queued)
	case queued > 0:
		fmt.Printf("prompt check: off; %d prompts are kept here and are checked when it is back on, or when the server asks for them\n", queued)
	}
	if x, _ := st.ExpiredChecks(); x > 0 {
		fmt.Printf("prompt check: %d prompts waited too long and were never checked; their time stays pending\n", x)
	}
	for _, line := range silentLines(st) {
		fmt.Println("warning:   ", s.orange(line))
	}
	fmt.Print("harnesses:  ")
	var parts []string
	for _, a := range adapters.All() {
		inst, _ := a.Installed()
		if _, watch := a.(adapters.Watcher); watch {
			if a.Detect() {
				parts = append(parts, s.green(a.Name()+" (watched)"))
			}
			continue
		}
		switch {
		case inst:
			parts = append(parts, s.green(a.Name()+" ✓"))
		case a.Detect():
			parts = append(parts, s.orange(a.Name()+" (detected, not installed)"))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "none detected")
	}
	fmt.Println(strings.Join(parts, ", "))
	if setup.WakaProxyActive(cfg) {
		fwd := "no forwarding"
		if cfg.ForwardURL != "" {
			fwd = "forwarding to " + cfg.ForwardURL
		}
		fmt.Printf("wakatime:   proxy active on :%d (%s)\n", cfg.WakaPortOrDefault(), fwd)
	} else {
		fmt.Println("wakatime:   proxy off (`seatime setup --wakatime-proxy` to see editor activity)")
	}
	return nil
}

func cmdToday(args []string) error {
	fs := newFlagSet("today")
	fs.Parse(args)
	st, p, err := openPipeline()
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	_, _ = gitscan.Scan(ctx, p, 24*time.Hour)
	cancel()
	cfg := config.Load()
	now := time.Now()
	sm, err := stats.Summarize(st, now, now.Location(), cfg.WeeklyGoalHours)
	if err != nil {
		return err
	}
	writeToday(os.Stdout, newStyler(), sm)
	return nil
}

func writeToday(w io.Writer, s styler, sm stats.Summary) {
	fill, empty := barGlyphs()
	fmt.Fprintf(w, "%s  %s\n", s.bold("SeaTime today"), s.dim(sm.Now.Format("Mon Jan 2")))
	fmt.Fprintf(w, "  %s today   %s this week (goal %s)   %s\n\n", s.green(stats.Dur(sm.Today)), stats.Dur(sm.Week),
		stats.Dur(sm.GoalHours*3600), s.dim(fmt.Sprintf("%d-day streak", sm.Streak)))
	if len(sm.TodayByProj) == 0 {
		if !sm.EverRecorded {
			fmt.Fprintln(w, "  Nothing recorded yet. Prompt your AI tool or edit a file; `seatime status` says whether it is counting.")
		} else {
			fmt.Fprintln(w, "  Nothing counted yet today.")
		}
		return
	}
	width := 10
	for _, p := range sm.TodayByProj {
		width = max(width, min(len(p.Name), 32))
	}
	top := sm.TodayByProj[0].Seconds
	for _, p := range sm.TodayByProj {
		name := p.Name
		if len(name) > width {
			name = "…" + name[len(name)-width+1:]
		}
		fmt.Fprintf(w, "  %-*s  %s  %7s\n", width, name, s.bar(p.Seconds/top, 16, fill, empty, s.green), stats.Dur(p.Seconds))
	}
	if sm.Longest > 0 {
		fmt.Fprintf(w, "\n  longest focused stretch: %s\n", stats.Dur(sm.Longest.Seconds()))
	}
	if sm.AINotCounted > 0 {
		why := fmt.Sprintf("more than %g min after your last edit or new prompt", ledger.Window.Minutes())
		if sm.CheckOn {
			why = "an agent running on its own, past the minutes of your prompts"
		}
		fmt.Fprintf(w, "  %s\n", s.dim(fmt.Sprintf("agent time not counted today: %s (%s)", stats.Dur(sm.AINotCounted), why)))
	}
	if sm.PendingToday > 0 {
		fmt.Fprintf(w, "  %s\n", s.dim(fmt.Sprintf("waiting for the prompt check: %s (counts once the check passes the prompts behind it)", stats.Dur(sm.PendingToday))))
	}
}

func cmdProjects(args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	ps, err := st.Projects(time.Time{})
	if err != nil {
		return err
	}
	counts, _ := st.ProjectKeys()
	for k := range counts {
		found := false
		for _, p := range ps {
			found = found || p.Key == k
		}
		if !found {
			ps = append(ps, store.Project{Key: k, Name: k})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].LastSeen.After(ps[j].LastSeen) })
	if len(ps) == 0 {
		fmt.Println("no projects yet")
		return nil
	}
	for _, p := range ps {
		fmt.Printf("%-40s %6d events  last seen %s\n", p.Key, counts[p.Key], ago(p.LastSeen))
		if p.Root != "" {
			fmt.Printf("    %s\n", p.Root)
		}
	}
	return nil
}

type check struct{ level, name, detail string } // level: ok|warn|fail

func cmdDoctor(args []string) error {
	for _, a := range args {
		switch a {
		case "--report":
			cmdDoctorReport(os.Stdout)
			return nil
		case "--self-test":
			return cmdSelfTest(os.Stdout)
		}
	}
	var cs []check
	add := func(l, n, d string) { cs = append(cs, check{l, n, d}) }
	cfg := config.Load()
	dd := config.DataDir()
	if err := os.MkdirAll(dd, 0o700); err != nil {
		add("fail", "data dir", err.Error())
	} else if f, err := os.CreateTemp(dd, "doctor-*"); err != nil {
		add("fail", "data dir", "not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
		add("ok", "data dir", dd)
	}
	if st, err := openStore(); err != nil {
		add("fail", "database", err.Error())
	} else {
		t0 := time.Now()
		n, err := st.VerifyChain()
		var dmg *store.DamagedError
		if errors.As(err, &dmg) {
			add("warn", "hash chain", fmt.Sprintf("%d events check out; %v", n, err))
		} else if err != nil {
			add("fail", "hash chain", fmt.Sprintf("BROKEN after %d events: %v (restart the daemon to move the damaged log aside and continue)", n, err))
		} else {
			add("ok", "hash chain", fmt.Sprintf("%d events check out (%s)", n, time.Since(t0).Round(time.Millisecond)))
		}
		if v, _ := st.GetMeta("last_sync_error"); v != "" {
			add("warn", "last sync", v)
		}
		if ss, err := st.SyncState(); err == nil {
			if w := syncLagWarning(st, ss); w != "" {
				add("warn", "sync lag", w)
			}
		}
		for _, l := range silentLines(st) {
			add("warn", "silent harness", l)
		}
		st.Close()
	}
	if p, err := lookGit(); err != nil {
		add("warn", "git", "not found: commit tracking disabled")
	} else {
		add("ok", "git", p)
	}
	if _, ok := config.LoadAuth(); ok {
		add("ok", "login", "logged in")
	} else {
		add("warn", "login", "not logged in; stats stay local until `seatime login`")
	}
	if m, ok := daemon.Ping(cfg); ok {
		add("ok", "daemon", fmt.Sprintf("running, pid %.0f", m["pid"]))
	} else {
		add("warn", "daemon", "not running")
	}
	if m, ok := setup.ServiceStatus(cfg); ok && m == "detached" {
		add("warn", "service", "daemon was started directly and won't survive a reboot (`seatime setup` retries a proper service)")
	} else if ok {
		add("ok", "service", "kept alive via "+m)
	} else {
		add("warn", "service", "no background service (`seatime setup`)")
	}
	// Harness hooks: report, and repair what the daemon's auto-hook would
	// (new harnesses, hooks wiped by a harness update). --no-fix reports only.
	fix := true
	for _, a := range args {
		if a == "--no-fix" {
			fix = false
		}
	}
	if !cfg.AutoHookEnabled() {
		add("warn", "auto-hook", "off (\"auto_hook\": false in config.json): new harnesses are not hooked automatically")
	} else {
		add("ok", "auto-hook", "daemon checks for new/broken harness hooks every 10 minutes")
	}
	hookRes, _ := setup.SyncHooks(adapters.All(), setup.HookBinary(), &cfg, nil, true)
	var repaired bool
	for _, r := range hookRes {
		name := "harness " + r.Name
		switch r.Status {
		case "ok":
			add("ok", name, "hooks installed")
		case "opted-out":
			add("ok", name, "opted out (you uninstalled it); `seatime setup --harness "+r.Name+"` re-enables")
		case "would install", "would repair":
			if fix {
				repaired = true
				continue
			}
			add("warn", name, "detected but hooks "+strings.TrimPrefix(r.Status, "would ")+" needed (run `seatime doctor` to fix)")
		}
	}
	if repaired {
		res, changed := setup.SyncHooks(adapters.All(), setup.HookBinary(), &cfg, nil, false)
		for _, r := range res {
			switch r.Status {
			case "installed", "repaired":
				add("ok", "harness "+r.Name, "hooks "+r.Status+" just now")
			case "failed":
				add("fail", "harness "+r.Name, r.Err.Error())
			}
		}
		if changed {
			_ = config.Save(cfg)
		}
	}
	if setup.WakaProxyActive(cfg) {
		add("ok", "wakatime", "proxy active")
	}
	if fi, err := os.Stat(config.LogPath()); err == nil && fi.Size() > 0 {
		add("warn", "log", fmt.Sprintf("%s has %d bytes of messages", config.LogPath(), fi.Size()))
	}
	ul, ud := updateCheck()
	add(ul, "updates", ud)
	add("ok", "platform", runtime.GOOS+"/"+runtime.GOARCH)
	s := newStyler()
	bad := 0
	for _, c := range cs {
		tag := s.green("ok  ")
		switch c.level {
		case "warn":
			tag = s.orange("warn")
		case "fail":
			tag = s.red("FAIL")
			bad++
		}
		fmt.Printf("[%s] %-18s %s\n", tag, c.name, c.detail)
	}
	if bad > 0 {
		return fmt.Errorf("%d problem(s)", bad)
	}
	return nil
}

func lookGit() (string, error) {
	return execLookPath("git")
}

// unsyncedWarnAfter is when unsynced activity starts to be at risk: the server
// only credits time it saw a live checkpoint for (no gap over 45 min).
const unsyncedWarnAfter = 20 * time.Minute

// syncLagWarning explains, in one line, that activity has gone unsynced long
// enough that it may not be credited. Empty when logged out or caught up.
func syncLagWarning(st *store.Store, ss store.SyncState) string {
	if _, ok := config.LoadAuth(); !ok {
		return ""
	}
	oldest, err := st.MinTS(ss.Seq)
	if err != nil || oldest.IsZero() || time.Since(oldest) < unsyncedWarnAfter {
		return ""
	}
	return fmt.Sprintf("activity from %s has not reached the server; time the server never saw live (over ~45 min unsynced) is not credited. Keep the daemon running and online.", ago(oldest))
}

// silentLines explains harnesses the daemon's canary found in use with
// silent hooks (see package canary).
func silentLines(st *store.Store) []string {
	v, _ := st.GetMeta(daemon.MetaSilent)
	var ss []canary.Silent
	if v == "" || json.Unmarshal([]byte(v), &ss) != nil {
		return nil
	}
	var out []string
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%s is running but SeaTime is not hearing from it (its hooks may have been removed by an update). `seatime doctor` re-installs them; the gap is filled from its session files and labelled \"backfilled\".", s.Label))
	}
	return out
}

// countingNow gathers what CountingNow needs from the log and the daemon.
func countingNow(st *store.Store, cfg config.Config, ping map[string]any, alive bool) stats.Counting {
	now := time.Now()
	in := stats.CountingInput{Now: now}
	if set := stats.PromptChecks(st); set != nil {
		in.CheckOn = set.OnAt(now)
	}
	in.Recent, _ = st.Events(store.Query{Since: now.Add(-30 * time.Minute)})
	in.Hooked = setup.WakaProxyActive(cfg)
	for _, a := range adapters.All() {
		if ok, _ := a.Installed(); ok {
			if _, watch := a.(adapters.Watcher); !watch || a.Detect() {
				in.Hooked = true
			}
		}
	}
	if v, _ := st.GetMeta(daemon.MetaSilent); v != "" {
		var ss []canary.Silent
		if json.Unmarshal([]byte(v), &ss) == nil {
			for _, x := range ss {
				in.Silent = append(in.Silent, x.Label)
			}
		}
	}
	return stats.CountingNow(in)
}

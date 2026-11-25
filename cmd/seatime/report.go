package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/idle"
	"seatime/internal/client/presence"
	"seatime/internal/client/protected"
	"seatime/internal/client/setup"
	"seatime/internal/client/spool"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/event"
)

// reportEnv is what `doctor --report` reads, behind functions so tests can
// fake the OS.
type reportEnv struct {
	now      time.Time
	home     string
	osVer    func() string
	agent    func() string // service manager's view of the daemon
	ping     func() (map[string]any, bool)
	idle     func() idle.Reading
	harness  func() []string // one "name: state" line per detected harness
	openSt   func() (*store.Store, error)
	logLines func() []string
	boot     func() (time.Time, bool) // when the OS last started
	presence func(idleOK bool) string // this device's presence level (package presence)
	version  string
}

// cmdDoctorReport prints one screen of plain text for pasting into a chat:
// nothing personal beyond home-relative paths, and it changes nothing.
func cmdDoctorReport(w io.Writer) {
	cfg := config.Load()
	env := reportEnv{
		now: time.Now(), home: config.Home(), version: version,
		osVer: osVersion, agent: func() string { return agentState(cfg) },
		ping: func() (map[string]any, bool) { return daemon.Ping(cfg) },
		idle: func() idle.Reading {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			r := &idle.Recorder{Src: idle.Default(), Emit: func(event.Event) error { return nil }}
			r.Tick(ctx)
			return r.Last()
		},
		harness: harnessLines,
		openSt:  openStore,
		logLines: func() []string {
			return readLog(config.LogPath()+".1", config.LogPath())
		},
		boot: bootTime,
		presence: func(ok bool) string {
			p := presence.System(config.FrontOn(cfg, false))
			if p.Remote() {
				if p.AtPrompt(context.Background()).TerminalIdleS != nil {
					return presence.TerminalOnly
				}
				return presence.NotMeasured
			}
			return presence.Level(ok, false, config.FrontOn(cfg, false), presence.FrontSupported(os.Getenv))
		},
	}
	writeReport(w, env)
}

// writeReport prints the report: a header, one "summary" line that says
// "clean" or lists what to look at, then the facts behind it.
func writeReport(w io.Writer, env reportEnv) {
	var body strings.Builder
	var issues []string
	issue := func(s string) { issues = append(issues, s) }
	writeFacts(&body, env, issue)
	fmt.Fprintf(w, "seatime doctor report, %s\n", env.now.UTC().Format("2006-01-02 15:04 UTC"))
	if len(issues) == 0 {
		fmt.Fprintf(w, "%-14s %s\n", "summary:", "clean")
	} else {
		fmt.Fprintf(w, "%-14s %d to look at: %s\n", "summary:", len(issues), strings.Join(issues, "; "))
	}
	io.WriteString(w, body.String())
}

func writeFacts(w io.Writer, env reportEnv, issue func(string)) {
	clean := func(s string) string { return scrubHome(s, env.home) }
	line := func(k, v string) { fmt.Fprintf(w, "%-14s %s\n", k+":", clean(v)) }
	line("seatime", env.version)
	line("os", fmt.Sprintf("%s (%s/%s)", env.osVer(), runtime.GOOS, runtime.GOARCH))
	ag := env.agent()
	line("background", ag)
	if strings.Contains(ag, "NOT loaded") || strings.Contains(ag, "missing") || strings.HasPrefix(ag, "no background") {
		issue("background service")
	}
	var up time.Duration
	if m, ok := env.ping(); ok {
		up = time.Duration(num(m["uptime_s"])) * time.Second
		line("daemon", fmt.Sprintf("running, version %v, up %s", m["version"], up))
	} else {
		line("daemon", "NOT running")
		issue("daemon not running")
	}
	if env.boot != nil {
		if bt, ok := env.boot(); ok {
			since := env.now.Sub(bt).Round(time.Minute)
			s := fmt.Sprintf("%s ago", since)
			if up > 0 {
				// started within a few minutes of boot: it came back by
				// itself after a restart
				if lag := since - up; lag <= 5*time.Minute {
					s += ", daemon started with it"
				} else {
					s += fmt.Sprintf(", daemon started %s after it", lag.Round(time.Minute))
				}
			}
			line("os booted", s)
		}
	}
	r := env.idle()
	if r.OK {
		line("idle timer", fmt.Sprintf("%s, last input %s ago", r.Src, r.Idle.Round(time.Second)))
	} else {
		line("idle timer", "unavailable ("+r.Err+"); your records say \"not measured\"")
		if runtime.GOOS == "darwin" {
			issue("idle timer")
		}
	}
	if env.presence != nil {
		line("presence", env.presence(r.OK)+" (how this device measures that someone is there)")
	}
	guard := "off"
	if protected.ShouldEnable() {
		guard = "on (the daemon never opens Desktop, Documents, Downloads, iCloud)"
	}
	line("folder guard", guard)
	for i, h := range env.harness() {
		k := ""
		if i == 0 {
			k = "harnesses"
		}
		fmt.Fprintf(w, "%-14s %s\n", k+map[bool]string{true: ":", false: " "}[k != ""], h)
		if strings.Contains(h, "NOT hooked") || strings.Contains(h, "unreadable") {
			issue(strings.SplitN(h, ":", 2)[0] + " hooks")
		}
	}
	if st, err := env.openSt(); err != nil {
		line("database", "CANNOT OPEN: "+err.Error())
		issue("database")
	} else {
		defer st.Close()
		n, _ := st.Count()
		line("events", strconv.FormatInt(n, 10))
		if evs, _ := st.Events(store.Query{Desc: true, Limit: 50}); len(evs) > 0 {
			for _, e := range evs {
				if e.Kind.Activity() {
					line("last event", fmt.Sprintf("%s from %s, %s", e.Kind, orNone(e.Harness), agoAt(env.now, e.TS)))
					break
				}
			}
		} else {
			line("last event", "none yet")
		}
		ss, _ := st.SyncState()
		head, _, _ := st.Head()
		if _, ok := config.LoadAuth(); !ok {
			line("last sync", "not logged in")
		} else if ss.Time.IsZero() {
			line("last sync", fmt.Sprintf("never, %d event(s) waiting", head))
		} else {
			line("last sync", fmt.Sprintf("%s, %d event(s) waiting", agoAt(env.now, ss.Time), head-ss.Seq))
		}
		if v, _ := st.GetMeta("last_sync_error"); v != "" {
			line("sync error", v)
			issue("sync error")
		}
		if n, longest := sleepGaps(st, env.now); n > 0 {
			line("sleep/wake", fmt.Sprintf("%d gap(s) in the last 24 h (sleep, shutdown or daemon stopped), longest %s; recording resumed after each", n, longest.Round(time.Minute)))
		} else {
			line("sleep/wake", "no gap in the last 24 h")
		}
		if off, at, ok := syncer.ClockOffset(st); ok {
			warn := ""
			if off > 2*time.Minute || off < -2*time.Minute {
				warn = " (CLOCK IS OFF by more than 2 min)"
				issue("clock")
			}
			line("clock offset", fmt.Sprintf("%+ds vs server, measured %s%s", int(off.Seconds()), agoAt(env.now, at), warn))
		} else {
			line("clock offset", "unknown (no sync yet)")
		}
	}
	if n, _ := spool.Count(config.SpoolDir()); n > 0 {
		line("spool", fmt.Sprintf("%d file(s) of hook events waiting for the daemon", n))
		issue("spool")
	}
	var recent []string
	for _, l := range env.logLines() {
		if t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", strings.SplitN(l, " ", 2)[0]); err == nil && env.now.Sub(t) <= 24*time.Hour && isProblem(l) {
			recent = append(recent, l)
		}
	}
	if len(recent) == 0 {
		line("errors (24h)", "none")
	} else {
		line("errors (24h)", strconv.Itoa(len(recent)))
		issue("errors in the log")
		if len(recent) > 5 {
			recent = recent[len(recent)-5:]
		}
		for _, l := range recent {
			if len(l) > 160 {
				l = l[:160] + "..."
			}
			fmt.Fprintf(w, "  %s\n", clean(l))
		}
	}
}

func isProblem(l string) bool {
	l = strings.ToLower(l)
	for _, k := range []string{"fail", "error", "panic", "cannot", "could not", "broken", "dropped", "timed out", "slow", "unavailable", "denied", "refused"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// scrubHome replaces the home directory (and the user name it contains) so a
// pasted report shows only home-relative paths.
func scrubHome(s, home string) string {
	if home == "" || home == "/" {
		return s
	}
	s = strings.ReplaceAll(s, home, "~")
	if u := filepath.Base(home); len(u) >= 3 {
		s = strings.ReplaceAll(s, u, "<user>")
	}
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func agoAt(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "in the future (" + (-d).Round(time.Second).String() + " ahead)"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h ago", d.Hours())
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func readLog(paths ...string) []string {
	var out []string
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			out = append(out, sc.Text())
		}
		f.Close()
	}
	return out
}

func harnessLines() []string {
	var out []string
	for _, a := range adapters.All() {
		if _, watch := a.(adapters.Watcher); watch {
			if a.Detect() {
				out = append(out, a.Name()+": watched (reads its logs, no hooks)")
			}
			continue
		}
		inst, err := a.Installed()
		switch {
		case err != nil:
			out = append(out, a.Name()+": config unreadable ("+err.Error()+")")
		case inst:
			out = append(out, a.Name()+": hooked")
		case a.Detect():
			out = append(out, a.Name()+": detected, NOT hooked")
		}
	}
	if len(out) == 0 {
		out = []string{"none detected"}
	}
	return out
}

func osVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		if b, err := exec.CommandContext(ctx, "sw_vers", "-productVersion").Output(); err == nil {
			return "macOS " + strings.TrimSpace(string(b))
		}
	case "linux":
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
					return strings.Trim(v, `"`)
				}
			}
		}
	case "windows":
		if b, err := exec.CommandContext(ctx, "cmd", "/c", "ver").Output(); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return runtime.GOOS
}

// agentState asks the service manager about the daemon (macOS: launchd).
func agentState(cfg config.Config) string {
	method, ok := setup.ServiceStatus(cfg)
	if method == "" {
		return "no background service set up (run `seatime setup`)"
	}
	s := method
	if !ok {
		s += " (its file is missing)"
	}
	if runtime.GOOS == "darwin" && method == "launchd" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/dev.hackclub.seatime", os.Getuid())).Output()
		if err != nil {
			return s + ", agent NOT loaded"
		}
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "state = "); ok {
				return s + ", agent loaded, " + v
			}
		}
		return s + ", agent loaded"
	}
	return s
}

// sleepGaps counts the breaks between the idle timer's windows in the last
// 24 h (the machine slept or shut down, or the daemon was stopped) and the
// longest one. Recording after a gap shows the daemon came back.
func sleepGaps(st *store.Store, now time.Time) (int, time.Duration) {
	evs, err := st.Events(store.Query{Kinds: []event.Kind{event.Idle}, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		return 0, 0
	}
	n, longest := 0, time.Duration(0)
	var end time.Time
	for _, e := range evs {
		start := e.TS.Add(-time.Duration(e.WindowMS) * time.Millisecond)
		if !end.IsZero() {
			if g := start.Sub(end); g > 90*time.Second {
				n++
				longest = max(longest, g)
			}
		}
		if e.TS.After(end) {
			end = e.TS
		}
	}
	return n, longest
}

// bootTime is when the OS last started (macOS: kern.boottime; Linux:
// /proc/uptime).
func bootTime() (time.Time, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "sysctl", "-n", "kern.boottime").Output()
		if err != nil {
			return time.Time{}, false
		}
		return parseBootTime(string(out))
	case "linux":
		b, err := os.ReadFile("/proc/uptime")
		if err != nil {
			return time.Time{}, false
		}
		f, err := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Now().Add(-time.Duration(f * float64(time.Second))), true
	}
	return time.Time{}, false
}

// parseBootTime reads sysctl's "{ sec = 1763700000, usec = 12345 } Fri Nov 21 ...".
func parseBootTime(s string) (time.Time, bool) {
	_, rest, ok := strings.Cut(s, "sec = ")
	if !ok {
		return time.Time{}, false
	}
	num, _, _ := strings.Cut(rest, ",")
	sec, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

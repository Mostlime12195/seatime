// Package setup installs/removes everything SeaTime adds to a user's machine:
// harness hooks (via adapters), the background service and (with consent)
// the WakaTime proxy setting.
package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/internal/client/localauth"
)

type Options struct {
	DryRun    bool
	WakaProxy bool // explicit consent to rewrite ~/.wakatime.cfg
	NoService bool
	NoCopy    bool
	Out       io.Writer

	// AlsoHackatime: nil = keep the saved choice (default: forward);
	// true/false = do/don't keep forwarding heartbeats to the original api_url.
	AlsoHackatime *bool

	Yes     bool     // zero-prompt mode: also enables the WakaTime proxy when a wakatime cfg exists
	NoWaka  bool     // never touch ~/.wakatime.cfg, even with Yes
	Verbose bool     // echo external commands
	Harness []string // limit to these harnesses (explicitly re-enables opted-out ones)
	Server  string   // persisted into config when set
	Version string   // running seatime version (to replace a stale daemon)

	Runner Runner                    // nil = real (or fake under SEATIME_FAKE_HOME, dry under DryRun)
	Probe  func(config.Config) Probe // is a daemon running? nil = unknown
	noWait bool                      // tests: don't sleep while waiting for the daemon
	goos   string                    // tests: override runtime.GOOS for the service plan
	getenv func(string) string       // tests: override os.Getenv for headless detection
}

func (o Options) say(f string, a ...any) {
	if o.Out != nil {
		prefix := ""
		if o.DryRun {
			prefix = "[dry-run] "
		}
		fmt.Fprintf(o.Out, prefix+f+"\n", a...)
	}
}

// StableBinary copies the running executable into the data dir so hooks and
// the service keep working if the user moves/deletes the download.
func StableBinary(o Options) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if o.NoCopy {
		return exe, nil
	}
	name := "seatime"
	if runtime.GOOS == "windows" {
		name = "seatime.exe"
	}
	dst := filepath.Join(config.BinDir(), name)
	if same(exe, dst) {
		return dst, nil
	}
	if inUserBin(exe) {
		// Installed by install.sh/install.ps1 into a stable per-user folder:
		// use it in place instead of keeping a second 10 MB copy.
		return exe, nil
	}
	o.say("binary: copying %s -> %s", exe, dst)
	if o.DryRun {
		return dst, nil
	}
	if err := os.MkdirAll(config.BinDir(), 0o755); err != nil {
		return exe, nil
	}
	tmp := dst + ".new"
	if err := copyFile(exe, tmp, 0o755); err != nil {
		o.say("binary: copy failed (%v); using %s in place", err, exe)
		return exe, nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		o.say("binary: could not replace %s (in use?); using %s in place", dst, exe)
		return exe, nil
	}
	return dst, nil
}

// inUserBin reports whether exe is SeaTime(.exe) in ~/.local/bin, where the
// one-line installer puts it.
func inUserBin(exe string) bool {
	dir := filepath.Join(config.Home(), ".local", "bin")
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return filepath.Dir(exe) == dir && strings.HasPrefix(filepath.Base(exe), "seatime")
}

func same(a, b string) bool {
	ai, e1 := os.Stat(a)
	bi, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(ai, bi)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Report is what Setup did, for the final summary.
type Report struct {
	Hooks   []HookResult
	Service ServiceResult
	Waka    string // one-line note, "" if untouched
	Hints   []string
	Server  string
}

// Setup detects and installs all adapters, the service and optionally the proxy.
func Setup(o Options) (*Report, error) {
	rep := &Report{}
	bin, err := StableBinary(o)
	if err != nil {
		return rep, err
	}
	cfg := config.Load()
	if o.Server != "" && !o.DryRun {
		cfg.Server = strings.TrimRight(o.Server, "/")
		o.say("server: %s", cfg.Server)
	}
	rep.Server = cfg.ServerURL()

	// Harness hooks.
	rep.Hooks, _ = SyncHooks(adapters.All(), bin, &cfg, o.Harness, o.DryRun)
	anyHooked := false
	for _, r := range rep.Hooks {
		switch r.Status {
		case "installed", "repaired", "ok":
			anyHooked = true
			if r.Status != "ok" {
				o.say("harness %-12s hooks %s", r.Name, r.Status)
			} else {
				o.say("harness %-12s hooks already in place", r.Name)
			}
			if r.Name == "codex" && r.Status != "ok" {
				rep.Hints = append(rep.Hints, "Codex: open Codex and run /hooks once to approve the new hooks")
			}
		case "failed", "opted-out":
			o.say("harness %s", r)
		case "absent":
			o.say("harness %-12s not detected, skipping", r.Name)
		default: // dry-run "would ..."
			o.say("harness %-12s %s", r.Name, r.Status)
		}
	}
	if !o.DryRun && !anyHooked && len(o.Harness) == 0 {
		rep.Hints = append(rep.Hints, "no supported AI tool found yet: install one, and SeaTime hooks it automatically within about 10 minutes")
	}

	// WakaTime / Hackatime proxy.
	_, cfgErr := os.Stat(wakaCfgPath())
	hasWakaCfg := cfgErr == nil
	enableWaka, auto := o.WakaProxy, false
	if o.AlsoHackatime != nil && (*o.AlsoHackatime || cfg.Waka.Enabled) {
		enableWaka = true // forwarding rides on the proxy, so asking for it is consent
	}
	if !enableWaka && o.Yes && !o.NoWaka && hasWakaCfg && !o.NoService {
		enableWaka, auto = true, true
	}
	switch {
	case o.NoWaka:
		cfg.WakaAuto = false
	case o.Yes && !o.NoService && !o.DryRun:
		cfg.WakaAuto = true // a plugin installed later is connected by the daemon
	}
	wakaWas := cfg.Waka.Enabled
	switch {
	case enableWaka:
		var sb strings.Builder
		wo := o
		wo.Out = &sb
		if err := EnableWakaProxy(wo, &cfg); err != nil {
			o.say("wakatime: FAILED: %v", err)
		} else {
			rep.Waka = strings.TrimSpace(sb.String())
			o.say("%s", rep.Waka)
		}
	case hasWakaCfg && o.Yes && o.NoService:
		o.say("wakatime: left ~/.wakatime.cfg alone (--no-service: nothing would receive the heartbeats)")
	case hasWakaCfg && !o.NoWaka:
		o.say("wakatime: left ~/.wakatime.cfg alone (pass --wakatime-proxy, or run `seatime setup --yes`, to let SeaTime see editor activity via your WakaTime/Hackatime plugin)")
	case !hasWakaCfg:
		if eds := EditorsPresent(); len(eds) > 0 {
			hint := fmt.Sprintf("editor time: found %s but no WakaTime plugin; install the \"WakaTime\" extension there, then run `seatime setup --yes` again so SeaTime sees it", strings.Join(eds, ", "))
			if cfg.WakaAuto {
				hint = fmt.Sprintf("editor time: found %s but no WakaTime plugin; install the \"WakaTime\" extension there, and SeaTime connects it within about 10 minutes", strings.Join(eds, ", "))
			}
			rep.Hints = append(rep.Hints, hint)
		}
	}
	// Save before starting the daemon: it reads forward_url / port at startup.
	if !o.DryRun {
		if err := config.Save(cfg); err != nil {
			return rep, err
		}
	}

	if !o.NoService {
		res, err := InstallService(o, &cfg, bin)
		rep.Service = res
		if err != nil {
			o.say("service: FAILED: %v", err)
			rep.Service.Notes = append(rep.Service.Notes, err.Error())
		} else {
			o.say("service: %s%s", res.Method, noteSuffix(res.Notes))
		}
		if auto && !wakaWas && !o.DryRun && (err != nil || !res.Running) {
			// Editors would be sending heartbeats at a dead port: undo.
			var sb strings.Builder
			wo := o
			wo.Out = &sb
			_ = DisableWakaProxy(wo, &cfg)
			rep.Waka = ""
			rep.Hints = append(rep.Hints, "wakatime: the daemon isn't running, so ~/.wakatime.cfg was left untouched; once `seatime daemon` is up, run `seatime setup --yes`")
			o.say("wakatime: daemon not running; restored ~/.wakatime.cfg")
		}
		if !o.DryRun {
			_ = config.Save(cfg)
		}
	}
	if !o.DryRun {
		removeOldExtension()
	}
	return rep, nil
}

func noteSuffix(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

// Summary prints the short, friendly wrap-up.
func (r *Report) Summary(w io.Writer, loginLine string) {
	var hooked, problems []string
	for _, h := range r.Hooks {
		switch h.Status {
		case "installed", "repaired", "ok":
			hooked = append(hooked, h.Name)
		case "failed":
			problems = append(problems, h.String())
		}
	}
	fmt.Fprintln(w, "\nAll set. Here's what SeaTime did:")
	if len(hooked) > 0 {
		fmt.Fprintf(w, "  AI tools:  hooked %s\n", strings.Join(hooked, ", "))
	} else {
		fmt.Fprintln(w, "  AI tools:  none found yet (SeaTime checks again every 10 minutes and hooks new ones itself)")
	}
	switch {
	case r.Service.Method == "":
		fmt.Fprintln(w, "  daemon:    not installed")
	case r.Service.Running:
		fmt.Fprintf(w, "  daemon:    running, kept alive via %s%s\n", r.Service.Method, noteSuffix(r.Service.Notes))
	default:
		fmt.Fprintf(w, "  daemon:    installed via %s but not confirmed running%s\n", r.Service.Method, noteSuffix(r.Service.Notes))
	}
	if r.Waka != "" {
		fmt.Fprintln(w, "  editors:   WakaTime/Hackatime plugin detected and wired to seatime")
	}
	if loginLine != "" {
		fmt.Fprintf(w, "  account:   %s\n", loginLine)
	}
	for _, p := range problems {
		fmt.Fprintf(w, "  problem:   %s\n", p)
	}
	for _, h := range r.Hints {
		fmt.Fprintf(w, "  note:      %s\n", h)
	}
	fmt.Fprintln(w, "Nothing else to do. `seatime today` shows your stats, `seatime doctor` checks health, `seatime uninstall` undoes all of this.")
}

// EditorsPresent lists code editors found without a WakaTime config.
func EditorsPresent() []string {
	h := config.Home()
	var out []string
	has := func(paths ...string) bool {
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
		return false
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		appdata = filepath.Join(h, "AppData", "Roaming")
	}
	if has(filepath.Join(h, ".vscode"), filepath.Join(h, ".config", "Code")) {
		out = append(out, "VS Code")
	}
	if has(filepath.Join(h, ".cursor")) {
		out = append(out, "Cursor")
	}
	if has(filepath.Join(h, ".config", "JetBrains"), filepath.Join(h, "Library", "Application Support", "JetBrains"), filepath.Join(appdata, "JetBrains")) {
		out = append(out, "JetBrains")
	}
	return out
}

// Uninstall reverses Setup. Local data is kept unless purge.
func Uninstall(o Options, purge bool) error {
	cfg := config.Load()
	if len(o.Harness) > 0 {
		err := UninstallHarness(o, adapters.All(), &cfg, o.Harness)
		if err == nil && !o.DryRun {
			err = config.Save(cfg)
		}
		return err
	}
	for _, a := range adapters.All() {
		if ok, _ := a.Installed(); !ok {
			continue
		}
		if o.DryRun {
			o.say("harness %-12s would remove hooks", a.Name())
			continue
		}
		if err := a.Uninstall(); err != nil {
			o.say("harness %-12s uninstall FAILED: %v", a.Name(), err)
		} else {
			o.say("harness %-12s hooks removed", a.Name())
		}
	}
	if err := DisableWakaProxy(o, &cfg); err != nil {
		o.say("wakatime: FAILED: %v", err)
	}
	_ = UninstallService(o, &cfg)
	if !o.DryRun {
		removeOldExtension()
		if err := localauth.Remove(); err != nil {
			o.say("local secret: remove FAILED: %v", err)
		}
	}
	if !o.DryRun {
		cfg.HookOptOut, cfg.Hooked = nil, nil
		_ = config.Save(cfg)
	}
	if purge {
		o.say("removing data dir %s", config.DataDir())
		if !o.DryRun {
			_ = os.RemoveAll(config.DataDir())
		}
	} else {
		o.say("kept your data in %s (use --purge to delete it)", config.DataDir())
	}
	return nil
}

// removeOldExtension deletes the browser extension that versions before
// browser chats were dropped unpacked into the data folder.
func removeOldExtension() {
	_ = os.RemoveAll(filepath.Join(config.DataDir(), "extension"))
}

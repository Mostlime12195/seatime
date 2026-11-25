package setup

import (
	"io"
	"os"
	"strconv"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/internal/client/localauth"
)

// What the daemon does on its own or because the owner asked on the web
// (internal/client/daemon remote.go). Each is what a command does, so the
// command stays a duplicate.

// SetForwarding turns relaying editor heartbeats to the plugin's original
// server (Hackatime) on or off, as `seatime setup --also-hackatime` does.
// It reports whether the config changed. Without the proxy there is
// nothing to relay: the daemon passes the choice on when it connects one.
func SetForwarding(cfg *config.Config, on bool) bool {
	if !cfg.Waka.Enabled {
		return false
	}
	want := ""
	if on {
		want = upstream(cfg.Waka)
	}
	if cfg.ForwardURL == want {
		return false
	}
	cfg.ForwardURL = want
	return true
}

// AutoConnectWaka connects a WakaTime/Hackatime editor plugin installed
// after setup, as `seatime setup --yes` would have: only when setup ran
// that way (cfg.WakaAuto), a service keeps the daemon running, and
// ~/.wakatime.cfg exists. alsoHackatime is the account's choice (nil:
// relay, the default). It reports whether it connected one.
func AutoConnectWaka(cfg *config.Config, alsoHackatime *bool) (bool, error) {
	if !cfg.WakaAuto || cfg.Waka.Enabled || cfg.Service.Method == "" {
		return false, nil
	}
	if _, err := os.Stat(wakaCfgPath()); err != nil {
		return false, nil
	}
	if err := EnableWakaProxy(Options{Out: io.Discard, AlsoHackatime: alsoHackatime}, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// UninstallSelf is `seatime uninstall` run by the daemon itself. The
// service is removed last, because removing it stops this process: first
// the hooks, the editor plugin's settings, the local secret and (purge)
// the data, then beforeStop, then the autostart entries. The caller exits
// after it returns.
func UninstallSelf(o Options, purge bool, beforeStop func()) error {
	cfg := config.Load()
	for _, a := range adapters.All() {
		if ok, _ := a.Installed(); !ok {
			continue
		}
		if err := a.Uninstall(); err != nil {
			o.say("harness %-12s uninstall FAILED: %v", a.Name(), err)
		}
	}
	if err := DisableWakaProxy(o, &cfg); err != nil {
		o.say("wakatime: FAILED: %v", err)
	}
	removeOldExtension()
	_ = localauth.Remove()
	cfg.HookOptOut, cfg.Hooked, cfg.WakaAuto = nil, nil, false
	svcCfg := cfg
	cfg.Service = config.ServiceState{}
	_ = config.Save(cfg)
	if beforeStop != nil {
		beforeStop()
	}
	if purge {
		_ = os.RemoveAll(config.DataDir())
	}
	s := newSvc(o, &svcCfg, "")
	for _, m := range []string{mXDG, mCron, mStartup, mSchtasks, mSystemd, mLaunchd} {
		s.removeSelf(m)
	}
	return nil
}

// removeSelf is remove for a daemon that removes its own service: the
// files go first, and the one step that stops this process comes last.
func (s *svc) removeSelf(m string) {
	switch m {
	case mSystemd:
		p := systemdUnitPath()
		if _, err := os.Stat(p); err == nil {
			_, _ = s.run("", "systemctl", "--user", "disable", "seatime.service")
			_ = os.Remove(p)
			_, _ = s.run("", "systemctl", "--user", "daemon-reload")
			// Restart=on-failure: the exit after this is not restarted;
			// --no-block asks systemd to stop it without waiting here
			_, _ = s.run("", "systemctl", "--user", "stop", "--no-block", "seatime.service")
		}
	case mLaunchd:
		p := launchdPath()
		if _, err := os.Stat(p); err == nil {
			_ = os.Remove(p)
			// KeepAlive would start it again: unload the job by its label
			// (the file is gone), which ends this process
			_, _ = s.run("", "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
		}
	default:
		s.remove(m, true)
	}
}

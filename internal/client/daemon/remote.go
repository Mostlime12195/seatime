package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"time"

	"seatime/internal/client/canary"
	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/setup"
	"seatime/internal/client/syncer"
	"seatime/internal/client/update"
	"seatime/pkg/api"
	"seatime/pkg/event"
)

// The check-in (pkg/api device.go): about once a minute the daemon tells
// the server how this computer is doing, applies the settings its owner
// chose on the web, and carries out what the owner asked this computer to
// do there. Each of those is what a command does, so the command stays a
// duplicate.

// can is what this version carries out from the web.
var can = []string{api.RequestForget, api.RequestUninstall, api.RequestSettings}

// MetaApplied holds, per account setting, the time of the change last
// applied (unix ms): a setting is applied only when it changed after that,
// so a change made with a command on this computer stays until the setting
// changes on the web again.
func MetaApplied(key string) string { return "acct_applied:" + key }

// MetaAcct holds the account's value of a setting as last seen ("1"/"0").
func MetaAcct(key string) string { return "acct:" + key }

func (d *Daemon) checkInLoop(ctx context.Context) {
	every := d.CheckInEvery
	if every <= 0 {
		every = time.Minute
	}
	sleep(ctx, 15*time.Second)
	for ctx.Err() == nil {
		wait := every
		if err := d.CheckInOnce(ctx); err != nil && !errors.Is(err, syncer.ErrNotLoggedIn) {
			var he *syncer.HTTPError
			if errors.As(err, &he) && he.Status == 404 {
				wait = 6 * time.Hour // an older server: nothing to check in with
			} else {
				logx.Printf("check-in: %v", err)
			}
		}
		sleepWall(ctx, wait, 30*time.Second)
	}
}

// CheckInOnce sends one check-in, applies the settings in the answer and
// carries out its requests (exported for tests).
func (d *Daemon) CheckInOnce(ctx context.Context) error {
	auth, ok := d.auth()
	if !ok {
		return syncer.ErrNotLoggedIn
	}
	c := syncer.NewClient(auth.APIBase(config.Load()), auth.Token, d.Version)
	resp, err := c.CheckIn(ctx, d.Health(auth.DeviceID))
	if err != nil {
		return err
	}
	d.applySettings(resp.Settings)
	for _, q := range resp.Requests {
		res, after := d.carryOut(q)
		res.DeviceID = auth.DeviceID
		if err := c.RequestResult(ctx, q.ID, res); err != nil {
			logx.Printf("check-in: answering request %d: %v", q.ID, err)
		}
		if after != nil {
			after()
			return nil
		}
	}
	return nil
}

func (d *Daemon) auth() (config.Auth, bool) {
	if d.Auth != nil {
		return *d.Auth, d.Auth.Token != ""
	}
	return config.LoadAuth()
}

// Health is what this computer reports at a check-in: no paths, no text,
// fixed problem codes only.
func (d *Daemon) Health(deviceID string) api.CheckIn {
	cfg := config.Load()
	h := api.CheckIn{DeviceID: deviceID, ClientVersion: d.Version, Can: can,
		Settings: api.DeviceSettings{AutoUpdate: cfg.AutoUpdateEnabled(), AlsoHackatime: cfg.ForwardURL != "", EditorPlugin: cfg.Waka.Enabled}}
	d.hookMu.Lock()
	res := append([]setup.HookResult(nil), d.hookRes...)
	d.hookMu.Unlock()
	for _, r := range res {
		switch r.Status {
		case "installed", "repaired", "ok":
			h.Tools = append(h.Tools, api.ToolHealth{Name: r.Name, State: api.ToolHooked})
		case "failed":
			h.Tools = append(h.Tools, api.ToolHealth{Name: r.Name, State: api.ToolFailed})
			h.Problems = append(h.Problems, api.Problem{Code: api.ProblemHookFailed, Tool: r.Name})
		case "opted-out":
			h.Tools = append(h.Tools, api.ToolHealth{Name: r.Name, State: api.ToolOptedOut})
		}
	}
	sort.Slice(h.Tools, func(i, j int) bool { return h.Tools[i].Name < h.Tools[j].Name })
	if v, _ := d.Store.GetMeta(MetaSilent); v != "" {
		var ss []canary.Silent
		if json.Unmarshal([]byte(v), &ss) == nil {
			for _, s := range ss {
				h.Problems = append(h.Problems, api.Problem{Code: api.ProblemHooksSilent, Tool: s.Harness})
			}
		}
	}
	if v, _ := d.Store.GetMeta("last_sync_error"); v != "" {
		h.Problems = append(h.Problems, api.Problem{Code: api.ProblemSyncFailing})
	}
	if ss, err := d.Store.SyncState(); err == nil {
		if t, err := d.Store.MinTS(ss.Seq); err == nil && !t.IsZero() && time.Since(t) > 20*time.Minute {
			h.Problems = append(h.Problems, api.Problem{Code: api.ProblemSyncBehind})
		}
	}
	if m := cfg.Service.Method; m == "" || m == "detached" {
		h.Problems = append(h.Problems, api.Problem{Code: api.ProblemNoService})
	}
	if !cfg.AutoHookEnabled() {
		h.Problems = append(h.Problems, api.Problem{Code: api.ProblemAutoHookOff})
	}
	if _, err := exec.LookPath("git"); err != nil {
		h.Problems = append(h.Problems, api.Problem{Code: api.ProblemNoGit})
	}
	if d.Update != nil && update.LoadState(config.DataDir()).LastError != "" {
		h.Problems = append(h.Problems, api.Problem{Code: api.ProblemUpdateFailed})
	}
	return h
}

// applySettings applies each account setting that changed on the web
// after the last change this computer applied.
func (d *Daemon) applySettings(sets []api.AccountSetting) {
	if len(sets) == 0 {
		return
	}
	cfg := config.Load()
	changed := false
	for _, s := range sets {
		v := "0"
		if s.Value {
			v = "1"
		}
		_ = d.Store.SetMeta(MetaAcct(s.Key), v)
		ms := s.Updated.UnixMilli()
		if last, _ := d.Store.GetMeta(MetaApplied(s.Key)); last != "" {
			if n, err := strconv.ParseInt(last, 10, 64); err == nil && n >= ms {
				continue
			}
		}
		switch s.Key {
		case api.SettingAutoUpdate:
			on := s.Value
			cfg.AutoUpdate = &on
			changed = true
			logx.Printf("check-in: automatic updates turned %s on the web", onOff(on))
		case api.SettingAlsoHackatime:
			if setup.SetForwarding(&cfg, s.Value) {
				changed = true
			}
			logx.Printf("check-in: sending editor time to Hackatime turned %s on the web", onOff(s.Value))
		default:
			continue
		}
		_ = d.Store.SetMeta(MetaApplied(s.Key), strconv.FormatInt(ms, 10))
	}
	if changed {
		if err := config.Save(cfg); err != nil {
			logx.Printf("check-in: saving config: %v", err)
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// acctBool is the account's last seen value of a setting (nil: never seen).
func (d *Daemon) acctBool(key string) *bool {
	v, _ := d.Store.GetMeta(MetaAcct(key))
	if v == "" {
		return nil
	}
	b := v == "1"
	return &b
}

// carryOut does one request. after, when set, runs once the answer is
// sent (removing SeaTime ends this process).
func (d *Daemon) carryOut(q api.DeviceRequest) (res api.RequestResult, after func()) {
	switch q.Kind {
	case api.RequestForget:
		key := ""
		if q.Project != "" {
			if key = d.localKey(q.Project); key == "" {
				return api.RequestResult{Error: "unknown_project"}, nil
			}
		}
		n, err := d.Store.Forget(key)
		if err != nil {
			logx.Printf("check-in: deleting prompt text: %v", err)
			return api.RequestResult{Error: "failed"}, nil
		}
		logx.Printf("check-in: deleted the text of %d record(s), as asked on the web", n)
		return api.RequestResult{OK: true, Forgotten: n}, nil
	case api.RequestUninstall:
		logx.Printf("check-in: removing SeaTime from this computer, as asked on the web")
		return api.RequestResult{OK: true}, func() { d.removeSelf(q.Purge) }
	}
	return api.RequestResult{Error: "unsupported"}, nil
}

// localKey is the local project key behind a server key ("" if none).
func (d *Daemon) localKey(server string) string {
	ps, err := d.Store.Projects(time.Time{})
	if err != nil {
		return ""
	}
	for _, p := range ps {
		if event.ServerKey(p.Key) == server {
			return p.Key
		}
	}
	return ""
}

// removeSelf is `seatime uninstall` (--purge) from the daemon, which then exits.
func (d *Daemon) removeSelf(purge bool) {
	if d.Uninstall != nil {
		d.Uninstall(purge)
		return
	}
	_ = setup.UninstallSelf(setup.Options{}, purge, func() { _ = d.Store.Close() })
	d.exit(0)
}

func (d *Daemon) exit(code int) {
	if d.Exit != nil {
		d.Exit(code)
		return
	}
	osExit(code)
}

// autoWaka connects an editor plugin installed after setup (setup.AutoConnectWaka).
func (d *Daemon) autoWaka() {
	cfg := config.Load()
	ok, err := setup.AutoConnectWaka(&cfg, d.acctBool(api.SettingAlsoHackatime))
	switch {
	case err != nil:
		logx.Printf("auto-hook: connecting the WakaTime plugin: %v", err)
	case ok:
		if err := config.Save(cfg); err != nil {
			logx.Printf("auto-hook: saving config: %v", err)
			return
		}
		logx.Printf("auto-hook: found a WakaTime/Hackatime plugin config; editors now report to SeaTime")
	}
}

// osExit is os.Exit (a variable for tests).
var osExit = os.Exit

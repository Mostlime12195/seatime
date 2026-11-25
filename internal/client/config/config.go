// Package config resolves seatime's data directory, config file and credentials.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const DefaultServer = "https://seatime.hackclub.dev" // placeholder
const DefaultWakaPort = 47823
const DefaultDashPort = 47824

// Home returns the user's home directory, honouring SEATIME_FAKE_HOME for tests.
func Home() string {
	if h := os.Getenv("SEATIME_FAKE_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// ApplyFakeHome points the process's notion of "home" at SEATIME_FAKE_HOME so
// adapters (which call os.UserHomeDir) are sandboxed too.
func ApplyFakeHome() {
	h := os.Getenv("SEATIME_FAKE_HOME")
	if h == "" {
		return
	}
	os.Setenv("HOME", h)
	os.Setenv("USERPROFILE", h)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	os.Setenv("XDG_DATA_HOME", filepath.Join(h, ".local", "share"))
	os.Setenv("APPDATA", filepath.Join(h, "AppData", "Roaming"))
	os.Setenv("LOCALAPPDATA", filepath.Join(h, "AppData", "Local"))
}

// DataDir is where the database, config, credentials and logs live.
func DataDir() string {
	if d := os.Getenv("SEATIME_HOME"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "seatime")
		}
		return filepath.Join(Home(), "AppData", "Local", "seatime")
	case "darwin":
		return filepath.Join(Home(), "Library", "Application Support", "seatime")
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "seatime")
	}
	return filepath.Join(Home(), ".local", "share", "seatime")
}

func DBPath() string     { return filepath.Join(DataDir(), "seatime.db") }
func LogPath() string    { return filepath.Join(DataDir(), "seatime.log") }
func ConfigPath() string { return filepath.Join(DataDir(), "config.json") }
func AuthPath() string   { return filepath.Join(DataDir(), "auth.json") }
func BinDir() string     { return filepath.Join(DataDir(), "bin") }

// SpoolDir holds events hooks could not store in time (see package spool).
func SpoolDir() string { return filepath.Join(DataDir(), "spool") }
func StateDir(name string) string {
	return filepath.Join(DataDir(), "adapters", name)
}

// WakaProxy records the user's consent-based rewrite of ~/.wakatime.cfg.
type WakaProxy struct {
	Enabled        bool   `json:"enabled"`
	OriginalAPIURL string `json:"original_api_url,omitempty"`
	HadAPIURL      bool   `json:"had_api_url,omitempty"`
	HadAPIKey      bool   `json:"had_api_key,omitempty"`
	AddedAPIKey    bool   `json:"added_api_key,omitempty"`
	CfgPath        string `json:"cfg_path,omitempty"`
	// WrittenSHA256 is the hash of the cfg SeaTime wrote; if it still matches at
	// uninstall time the saved backup is restored byte-for-byte.
	WrittenSHA256 string `json:"written_sha256,omitempty"`
	BackupPath    string `json:"backup_path,omitempty"`
	CreatedFile   bool   `json:"created_file,omitempty"` // cfg did not exist before SeaTime
}

type Config struct {
	Server   string `json:"server,omitempty"`
	WakaPort int    `json:"wakatime_port,omitempty"`
	DashPort int    `json:"dash_port,omitempty"`
	// ForwardURL: where the WakaTime proxy forwards heartbeats unchanged
	// (the user's previous api_url, e.g. Hackatime). Set by default when the
	// proxy is enabled; empty after `seatime setup --also-hackatime=false`.
	ForwardURL string    `json:"forward_url,omitempty"`
	Waka       WakaProxy `json:"wakatime_proxy"`
	// WakaAuto: setup ran with --yes and without --no-wakatime, so the
	// daemon connects a WakaTime/Hackatime editor plugin installed later,
	// as setup would have.
	WakaAuto bool `json:"wakatime_auto,omitempty"`

	// AutoHook: nil/true = the daemon periodically hooks newly installed
	// harnesses and repairs removed hooks; false = never touch harness config
	// from the daemon.
	AutoHook *bool `json:"auto_hook,omitempty"`
	// HookOptOut lists harnesses the user explicitly uninstalled; neither setup
	// nor the daemon re-hooks them until `seatime setup --harness <name>`.
	HookOptOut []string `json:"hook_opt_out,omitempty"`
	// Hooked records harnesses SeaTime has installed hooks into (to tell "repair"
	// from "new" in logs).
	Hooked []string `json:"hooked,omitempty"`
	// Service records how the background daemon is kept running.
	Service ServiceState `json:"service,omitempty"`
	// KeepPromptText: nil/true = prompt text and agent shell commands are
	// kept in the local log (they never leave this machine); false =
	// only their fingerprints and a similarity sketch are stored.
	KeepPromptText *bool `json:"keep_prompt_text,omitempty"`
	// WeeklyGoalHours is the goal on the personal dashboard (0 = 10 h).
	WeeklyGoalHours float64 `json:"weekly_goal_hours,omitempty"`
	// AutoUpdate: nil/true = the daemon installs a new signed release about
	// once a day; false = only `seatime update` does (`seatime update off`).
	AutoUpdate *bool `json:"auto_update,omitempty"`
	// UpdateURL is where releases are downloaded from ("" = <server>/dl/).
	// Releases are trusted by their signature, not by this host.
	UpdateURL string `json:"update_url,omitempty"`
	// FrontWindowCheck: true = at each prompt, and every 5 s while you
	// work, SeaTime asks the system whether the AI tool's own window (or
	// terminal) is in front and records only yes, no or "no window
	// attached", never a program name. It is also on, unless
	// FrontWindowOff, while the server's prompt check is on (FrontOn): that
	// check needs the time with the tool's window in front.
	FrontWindowCheck bool `json:"front_window_check,omitempty"`
	// FrontWindowOff turns the front-window check off even while the
	// server's prompt check is on (FrontOn).
	FrontWindowOff bool `json:"front_window_off,omitempty"`
}

// ServiceState is the background-service mechanism chosen by setup.
type ServiceState struct {
	Method string `json:"method,omitempty"` // systemd|xdg-autostart|cron|launchd|startup-folder|schtasks|detached
	Note   string `json:"note,omitempty"`
}

func (c Config) AutoHookEnabled() bool { return c.AutoHook == nil || *c.AutoHook }

// AutoUpdateEnabled reports whether the daemon updates SeaTime by itself.
func (c Config) AutoUpdateEnabled() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

// UpdateBase is the release download folder, ending in "/".
func (c Config) UpdateBase() string {
	u := strings.TrimSpace(c.UpdateURL)
	if u == "" {
		u = c.ServerURL() + "/dl/"
	}
	return strings.TrimRight(u, "/") + "/"
}

// KeepsPromptText reports whether prompt text is stored locally.
func (c Config) KeepsPromptText() bool { return c.KeepPromptText == nil || *c.KeepPromptText }

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (c Config) OptedOut(h string) bool  { return contains(c.HookOptOut, h) }
func (c Config) WasHooked(h string) bool { return contains(c.Hooked, h) }

func (c *Config) SetOptOut(h string, out bool) {
	c.HookOptOut = without(c.HookOptOut, h)
	if out {
		c.HookOptOut = append(c.HookOptOut, h)
	}
}

func (c *Config) SetHooked(h string, on bool) {
	c.Hooked = without(c.Hooked, h)
	if on {
		c.Hooked = append(c.Hooked, h)
	}
}

func without(l []string, s string) []string {
	var out []string
	for _, x := range l {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func (c Config) ServerURL() string {
	s := os.Getenv("SEATIME_SERVER")
	if s == "" {
		s = c.Server
	}
	if s == "" {
		s = DefaultServer
	}
	return strings.TrimRight(s, "/")
}

func (c Config) WakaPortOrDefault() int {
	if c.WakaPort > 0 {
		return c.WakaPort
	}
	return DefaultWakaPort
}

func (c Config) DashPortOrDefault() int {
	if c.DashPort > 0 {
		return c.DashPort
	}
	return DefaultDashPort
}

func Load() Config {
	var c Config
	if b, err := os.ReadFile(ConfigPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func Save(c Config) error {
	return writeJSON(ConfigPath(), c, 0o644)
}

// Auth holds device credentials. Stored 0600.
type Auth struct {
	Token      string `json:"token"`
	DeviceID   string `json:"device_id"`
	Username   string `json:"username"`
	Server     string `json:"server"`
	PrivateKey string `json:"private_key"` // base64 ed25519 (64 bytes)
	PublicKey  string `json:"public_key"`
}

func LoadAuth() (Auth, bool) {
	var a Auth
	b, err := os.ReadFile(AuthPath())
	if err != nil || json.Unmarshal(b, &a) != nil || a.Token == "" {
		return Auth{}, false
	}
	return a, true
}

func SaveAuth(a Auth) error { return writeJSON(AuthPath(), a, 0o600) }

func writeJSON(path string, v any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	_ = os.Chmod(tmp, perm)
	return os.Rename(tmp, path)
}

// APIBase is the server an authenticated device talks to: SEATIME_SERVER wins,
// then the server the token was issued by, then the configured default.
func (a Auth) APIBase(c Config) string {
	if os.Getenv("SEATIME_SERVER") == "" && a.Server != "" {
		return strings.TrimRight(a.Server, "/")
	}
	return c.ServerURL()
}

// FrontOn is whether the front-window check runs: switched on in the
// config, or the server's prompt check is on and it is not switched off.
func FrontOn(c Config, promptCheck bool) bool {
	if c.FrontWindowOff {
		return false
	}
	return c.FrontWindowCheck || promptCheck
}

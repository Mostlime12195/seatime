package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"seatime/internal/client/config"
)

const dummyKey = "00000000-0000-4000-8000-000000000000"

func wakaCfgPath() string { return filepath.Join(config.Home(), ".wakatime.cfg") }

// iniGet returns the value of key in [section] ("" , false if absent).
func iniGet(text, section, key string) (string, bool) {
	cur := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = strings.ToLower(t[1 : len(t)-1])
			continue
		}
		if cur != section || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// iniSet sets (or removes, when remove) key in [section], preserving all other lines.
func iniSet(text, section, key, val string, remove bool) string {
	lines := strings.Split(text, "\n")
	cur := ""
	secEnd := -1 // index after last line of the section
	found := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = strings.ToLower(t[1 : len(t)-1])
			if cur == section {
				secEnd = i + 1
			}
			continue
		}
		if cur != section {
			continue
		}
		secEnd = i + 1
		k, _, ok := strings.Cut(t, "=")
		if ok && !strings.HasPrefix(t, "#") && strings.EqualFold(strings.TrimSpace(k), key) {
			found = true
			if remove {
				lines[i] = "\x00"
			} else {
				lines[i] = key + " = " + val
			}
		}
	}
	if !found && !remove {
		newLine := key + " = " + val
		if secEnd < 0 {
			if strings.TrimSpace(text) != "" && !strings.HasSuffix(text, "\n") {
				lines = append(lines, "")
			}
			lines = append(lines, "["+section+"]", newLine)
			if lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
		} else {
			lines = append(lines[:secEnd], append([]string{newLine}, lines[secEnd:]...)...)
		}
	}
	var out []string
	for _, l := range lines {
		if l != "\x00" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func localAPIURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d/api", port) }

// EnableWakaProxy rewrites api_url in ~/.wakatime.cfg to the local proxy,
// remembering the original in seatime's config.
func EnableWakaProxy(o Options, cfg *config.Config) error {
	path := wakaCfgPath()
	b, err := os.ReadFile(path)
	text := string(b)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	port := cfg.WakaPortOrDefault()
	local := localAPIURL(port)
	orig, had := iniGet(text, "settings", "api_url")
	_, hadKey := iniGet(text, "settings", "api_key")
	if had && orig == local {
		if o.AlsoHackatime != nil {
			cfg.ForwardURL = forwardFor(o, cfg.Waka)
		}
		o.say("wakatime: %s already points at SeaTime (%s)%s", path, local, forwardNote(cfg))
		return nil
	}
	fresh := !cfg.Waka.Enabled
	if fresh {
		cfg.Waka = config.WakaProxy{Enabled: true, OriginalAPIURL: orig, HadAPIURL: had, HadAPIKey: hadKey, CfgPath: path,
			CreatedFile: os.IsNotExist(err)}
	}
	if o.AlsoHackatime != nil || fresh {
		cfg.ForwardURL = forwardFor(o, cfg.Waka)
	}
	o.say("wakatime: editors now report to SeaTime (api_url -> %s in %s; `seatime uninstall` restores the original)%s", local, path, forwardNote(cfg))
	if o.DryRun {
		return nil
	}
	if err == nil {
		if _, e := os.Stat(path + ".seatime-bak"); os.IsNotExist(e) {
			_ = os.WriteFile(path+".seatime-bak", b, 0o600)
		}
		cfg.Waka.BackupPath = path + ".seatime-bak"
	}
	text = iniSet(text, "settings", "api_url", local, false)
	if !hadKey {
		text = iniSet(text, "settings", "api_key", dummyKey, false)
		cfg.Waka.AddedAPIKey = true
		o.say("wakatime: no api_key present; added a placeholder (plugins refuse to send without one)")
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	cfg.Waka.WrittenSHA256 = sum(text)
	mode := os.FileMode(0o600)
	if fi, e := os.Stat(path); e == nil {
		mode = fi.Mode().Perm()
	}
	return os.WriteFile(path, []byte(text), mode)
}

// upstream is where the editor plugin reported before SeaTime: the saved
// api_url, or WakaTime's default when only an api_key was set ("" = nowhere).
func upstream(w config.WakaProxy) string {
	switch {
	case w.HadAPIURL && w.OriginalAPIURL != "":
		return w.OriginalAPIURL
	case w.HadAPIKey:
		return "https://api.wakatime.com/api/v1"
	}
	return ""
}

// forwardFor is where heartbeats are relayed: the original upstream unless
// the user passed --also-hackatime=false.
func forwardFor(o Options, w config.WakaProxy) string {
	if o.AlsoHackatime != nil && !*o.AlsoHackatime {
		return ""
	}
	return upstream(w)
}

func forwardNote(cfg *config.Config) string {
	if cfg.ForwardURL != "" {
		return "; heartbeats are also forwarded unchanged to " + cfg.ForwardURL
	}
	if up := upstream(cfg.Waka); up != "" {
		return "; not forwarded to " + up + " (--also-hackatime=false)"
	}
	return ""
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// DisableWakaProxy restores the original api_url.
func DisableWakaProxy(o Options, cfg *config.Config) error {
	w := cfg.Waka
	if !w.Enabled {
		return nil
	}
	path := w.CfgPath
	if path == "" {
		path = wakaCfgPath()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(b)
	cur, _ := iniGet(text, "settings", "api_url")
	if w.WrittenSHA256 != "" && sum(text) == w.WrittenSHA256 {
		// Untouched since SeaTime wrote it: put the original back byte-for-byte.
		if w.CreatedFile {
			o.say("wakatime: removed %s (SeaTime had created it)", path)
			if !o.DryRun {
				_ = os.Remove(path)
			}
		} else if bak, e := os.ReadFile(orFallback(w.BackupPath, path+".seatime-bak")); e == nil {
			o.say("wakatime: restored %s exactly as it was", path)
			if !o.DryRun {
				mode := os.FileMode(0o600)
				if fi, e := os.Stat(path); e == nil {
					mode = fi.Mode().Perm()
				}
				if err := os.WriteFile(path, bak, mode); err != nil {
					return err
				}
				_ = os.Remove(orFallback(w.BackupPath, path+".seatime-bak"))
			}
		} else {
			cur = localAPIURL(cfg.WakaPortOrDefault()) // fall through to the line-level restore
			goto lineRestore
		}
		goto done
	}
lineRestore:
	if cur == localAPIURL(cfg.WakaPortOrDefault()) {
		text = iniSet(text, "settings", "api_url", w.OriginalAPIURL, !w.HadAPIURL)
		if w.AddedAPIKey {
			if k, _ := iniGet(text, "settings", "api_key"); k == dummyKey {
				text = iniSet(text, "settings", "api_key", "", true)
			}
		}
		o.say("wakatime: restored api_url in %s to %s", path, orFallback(w.OriginalAPIURL, "<default>"))
		if !o.DryRun {
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				return err
			}
		}
	}
done:
	if !o.DryRun {
		cfg.Waka = config.WakaProxy{}
		cfg.ForwardURL = ""
	}
	return nil
}

func orFallback(s, f string) string {
	if s == "" {
		return f
	}
	return s
}

// WakaProxyActive reports whether ~/.wakatime.cfg currently points at seatime.
func WakaProxyActive(cfg config.Config) bool {
	b, err := os.ReadFile(wakaCfgPath())
	if err != nil {
		return false
	}
	v, _ := iniGet(string(b), "settings", "api_url")
	return v == localAPIURL(cfg.WakaPortOrDefault())
}

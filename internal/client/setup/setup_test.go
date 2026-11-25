package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seatime/internal/client/config"
)

func TestIni(t *testing.T) {
	in := "[settings]\napi_key = abc\n# api_url = old\napi_url = https://hackatime/api\ndebug = false\n\n[other]\napi_url = keep\n"
	if v, ok := iniGet(in, "settings", "api_url"); !ok || v != "https://hackatime/api" {
		t.Fatal(v, ok)
	}
	out := iniSet(in, "settings", "api_url", "http://x", false)
	if !strings.Contains(out, "api_url = http://x") || !strings.Contains(out, "api_url = keep") || !strings.Contains(out, "# api_url = old") {
		t.Fatal(out)
	}
	out = iniSet(in, "settings", "api_url", "", true)
	if strings.Contains(out, "hackatime") || !strings.Contains(out, "api_key = abc") {
		t.Fatal(out)
	}
	out = iniSet("", "settings", "api_url", "http://x", false)
	if v, _ := iniGet(out, "settings", "api_url"); v != "http://x" {
		t.Fatal(out)
	}
}

func TestWakaProxyRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	cfgPath := filepath.Join(home, ".wakatime.cfg")
	orig := "[settings]\napi_url = https://hackatime.hackclub.com/api/hackatime/v1\napi_key = k-123\n"
	os.WriteFile(cfgPath, []byte(orig), 0o600)

	cfg := config.Load()
	o := Options{}
	// dry run changes nothing
	if err := EnableWakaProxy(Options{DryRun: true}, &cfg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != orig {
		t.Fatal("dry run modified file")
	}
	cfg = config.Load()
	if err := EnableWakaProxy(o, &cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "api_url = http://127.0.0.1:47823/api") || !strings.Contains(string(b), "k-123") {
		t.Fatal(string(b))
	}
	if cfg.ForwardURL != "https://hackatime.hackclub.com/api/hackatime/v1" || !cfg.Waka.Enabled {
		t.Fatalf("default must forward to the original api_url: %+v", cfg)
	}
	if bak, _ := os.ReadFile(cfgPath + ".seatime-bak"); string(bak) != orig {
		t.Fatal("backup missing")
	}
	if !WakaProxyActive(cfg) {
		t.Fatal("not active")
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	cfg = config.Load()
	if err := DisableWakaProxy(o, &cfg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != orig {
		t.Fatalf("not restored exactly:\n%q\nwant\n%q", b, orig)
	}
	if _, err := os.Stat(cfgPath + ".seatime-bak"); err == nil {
		t.Fatal("backup should be gone after exact restore")
	}
	if cfg.Waka.Enabled || cfg.ForwardURL != "" {
		t.Fatalf("%+v", cfg)
	}
}

func TestWakaRestoreIsByteExactWithOddFormatting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	cfgPath := filepath.Join(home, ".wakatime.cfg")
	orig := "; comment\r\n[settings]\r\n  api_key=abc\r\napi_url=https://h/api/v1\r\n\r\n\r\n[internal]\r\nx = 1"
	os.WriteFile(cfgPath, []byte(orig), 0o640)
	cfg := config.Load()
	if err := EnableWakaProxy(Options{}, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := DisableWakaProxy(Options{}, &cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfgPath)
	if string(b) != orig {
		t.Fatalf("%q != %q", b, orig)
	}
	if fi, _ := os.Stat(cfgPath); fi.Mode().Perm() != 0o640 {
		t.Fatal(fi.Mode())
	}
}

func TestWakaRestoreWhenUserEditedAfterwards(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	cfgPath := filepath.Join(home, ".wakatime.cfg")
	os.WriteFile(cfgPath, []byte("[settings]\napi_url = https://h/api\napi_key = k\n"), 0o600)
	cfg := config.Load()
	EnableWakaProxy(Options{}, &cfg)
	b, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, append(b, []byte("debug = true\n")...), 0o600) // plugin added a setting
	DisableWakaProxy(Options{}, &cfg)
	b, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "api_url = https://h/api") || !strings.Contains(string(b), "debug = true") || strings.Contains(string(b), "127.0.0.1") {
		t.Fatal(string(b))
	}
}

func TestWakaCreatedFileIsRemoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	cfg := config.Load()
	EnableWakaProxy(Options{}, &cfg)
	if _, err := os.Stat(wakaCfgPath()); err != nil {
		t.Fatal(err)
	}
	DisableWakaProxy(Options{}, &cfg)
	if _, err := os.Stat(wakaCfgPath()); err == nil {
		t.Fatal("file SeaTime created should be removed")
	}
}

func TestWakaAlsoHackatimeRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	cfgPath := filepath.Join(home, ".wakatime.cfg")
	const up = "https://hackatime.hackclub.com/api/hackatime/v1"
	orig := "[settings]\napi_url = " + up + "\napi_key = k-123\n"
	os.WriteFile(cfgPath, []byte(orig), 0o600)
	yes, no := true, false

	cfg := config.Load()
	var out strings.Builder
	if err := EnableWakaProxy(Options{AlsoHackatime: &no, Out: &out}, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ForwardURL != "" || !strings.Contains(out.String(), "not forwarded to "+up) {
		t.Fatalf("%+v\n%s", cfg, out.String())
	}
	// a later run without the flag keeps the opt-out; =true turns it back on
	if err := EnableWakaProxy(Options{}, &cfg); err != nil || cfg.ForwardURL != "" {
		t.Fatalf("opt-out flipped: %v %+v", err, cfg)
	}
	if err := EnableWakaProxy(Options{AlsoHackatime: &yes}, &cfg); err != nil || cfg.ForwardURL != up {
		t.Fatalf("=true ignored: %v %+v", err, cfg)
	}
	if err := EnableWakaProxy(Options{}, &cfg); err != nil || cfg.ForwardURL != up {
		t.Fatalf("forward dropped: %v %+v", err, cfg)
	}
	if err := DisableWakaProxy(Options{}, &cfg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != orig {
		t.Fatalf("not restored exactly:\n%q\nwant\n%q", b, orig)
	}
	if cfg.Waka.Enabled || cfg.ForwardURL != "" {
		t.Fatalf("%+v", cfg)
	}
}

func TestWakaDefaultForward(t *testing.T) {
	const up = "https://hackatime.hackclub.com/api/hackatime/v1"
	for _, c := range []struct {
		name, cfg, want string
	}{
		{"hackatime", "[settings]\napi_url = " + up + "\napi_key = k\n", up},
		{"wakatime default", "[settings]\napi_key = k\n", "https://api.wakatime.com/api/v1"},
		{"no cfg", "", ""},
		{"no api_url or key", "[settings]\ndebug = true\n", ""},
		{"already SeaTime", "[settings]\napi_url = http://127.0.0.1:47823/api\napi_key = k\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEATIME_FAKE_HOME", home)
			t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
			if c.cfg != "" {
				os.WriteFile(filepath.Join(home, ".wakatime.cfg"), []byte(c.cfg), 0o600)
			}
			cfg := config.Load()
			var out strings.Builder
			if err := EnableWakaProxy(Options{Out: &out}, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.ForwardURL != c.want {
				t.Fatalf("forward=%q want %q", cfg.ForwardURL, c.want)
			}
			if strings.Contains(out.String(), "not forwarded") || strings.Contains(out.String(), "no longer") {
				t.Fatalf("default run should not warn:\n%s", out.String())
			}
		})
	}
}

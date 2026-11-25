package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"seatime/internal/client/config"
	"seatime/internal/client/update"
)

// The daemon reads the setting and the download URL from config on every
// check, and asks Run to restart after an update.
func TestDaemonUpdateOnce(t *testing.T) {
	sandbox(t)
	pk, priv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("new")
	s := sha256.Sum256(bin)
	m, _ := update.Sign(update.Manifest{Version: "v1.1.0", Files: map[string]string{
		update.AssetName("linux", "amd64"): hex.EncodeToString(s[:])}}, priv)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if filepath.Base(r.URL.Path) == update.ManifestName {
			w.Write(m)
		} else {
			w.Write(bin)
		}
	}))
	defer ts.Close()
	exe := filepath.Join(t.TempDir(), "seatime")
	os.WriteFile(exe, []byte("old"), 0o755)
	d := &Daemon{restart: make(chan struct{}, 1), Update: &update.Updater{
		Version: "v1.0.0", Exe: exe, Dir: config.DataDir(), Key: base64.StdEncoding.EncodeToString(pk),
		GOOS: "linux", GOARCH: "amd64",
		SelfTest: func(context.Context, string) (string, error) { return update.SelfTestLine("v1.1.0"), nil },
	}}

	off := false
	config.Save(config.Config{AutoUpdate: &off, UpdateURL: ts.URL + "/somewhere/else"})
	if res, err := d.UpdateOnce(context.Background()); err != nil || res.Updated || hits.Load() != 0 {
		t.Fatalf("updates are off, yet: %+v %v, %d requests", res, err, hits.Load())
	}

	config.Save(config.Config{UpdateURL: ts.URL + "/somewhere/else"})
	res, err := d.UpdateOnce(context.Background())
	if err != nil || !res.Updated {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("not installed")
	}
	select {
	case <-d.restart:
	default:
		t.Fatal("no restart requested after the update")
	}
}

func TestUpdateBaseURL(t *testing.T) {
	t.Setenv("SEATIME_SERVER", "")
	for _, c := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{Server: "https://seatime.example.com/"}, "https://seatime.example.com/dl/"},
		{config.Config{Server: "https://x", UpdateURL: "https://github.com/o/r/releases/latest/download"}, "https://github.com/o/r/releases/latest/download/"},
	} {
		if got := c.cfg.UpdateBase(); got != c.want {
			t.Errorf("%+v: %s", c.cfg, got)
		}
	}
}

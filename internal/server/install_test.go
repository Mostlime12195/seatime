package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"seatime/internal/server/store"
	"seatime/internal/server/store/storetest"
)

func newInstallServer(t *testing.T, dl string) (*httptest.Server, *Server) {
	t.Helper()
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{Dev: true, Secret: "s", DLDir: dl,
		Admins: map[string]bool{}}
	srv, err := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	srv.cfg.BaseURL = ts.URL // the installer bakes in the public URL
	return ts, srv
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b), r.Header
}

// writeRelease makes a fake release dir: each binary is a shell stub that
// records how it was invoked.
func writeRelease(t *testing.T, dir string, corrupt bool) {
	t.Helper()
	var sums strings.Builder
	for _, n := range []string{"seatime-linux-amd64", "seatime-linux-arm64", "seatime-darwin-amd64", "seatime-darwin-arm64", "seatime-windows-amd64.exe", "seatime-windows-386.exe"} {
		body := "#!/bin/sh\necho \"ran: $*\" > \"$SEATIME_TEST_OUT\"\n"
		h := sha256.Sum256([]byte(body))
		sum := hex.EncodeToString(h[:])
		if corrupt {
			sum = strings.Repeat("0", 64)
		}
		fmt.Fprintf(&sums, "%s  %s\n", sum, n)
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644)
}

func TestInstallScriptsAreTemplated(t *testing.T) {
	ts, _ := newInstallServer(t, "")
	code, body, hdr := get(t, ts.URL+"/install.sh")
	if code != 200 || strings.Contains(body, "@@SERVER@@") || !strings.Contains(body, `SERVER="`+ts.URL+`"`) || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatal(code, hdr, body[:200])
	}
	code, body, _ = get(t, ts.URL+"/install.ps1")
	if code != 200 || strings.Contains(body, "@@SERVER@@") || !strings.Contains(body, `$server = '`+ts.URL+`'`) {
		t.Fatal(code)
	}
}

func TestInstallRefusesUnsafeBaseURL(t *testing.T) {
	ts, srv := newInstallServer(t, "")
	srv.cfg.BaseURL = `http://x"; rm -rf ~; echo "`
	if code, _, _ := get(t, ts.URL+"/install.sh"); code != 500 {
		t.Fatal(code)
	}
}

func TestDownloadsOnlyAllowlistedNames(t *testing.T) {
	dir := t.TempDir()
	writeRelease(t, dir, false)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644)
	os.WriteFile(filepath.Join(dir, "seatime-linux-riscv"), []byte("no"), 0o644)
	ts, _ := newInstallServer(t, dir)
	for name, want := range map[string]int{
		"SHA256SUMS": 200, "seatime-linux-amd64": 200, "seatime-windows-386.exe": 200,
		"secret.txt": 404, "seatime-linux-riscv": 404, "..%2fsecret.txt": 404, "seatime-linux-arm64.exe": 404,
	} {
		if code, _, _ := get(t, ts.URL+"/dl/"+name); code != want {
			t.Errorf("%s: %d want %d", name, code, want)
		}
	}
	// no download dir configured -> 404 rather than serving from cwd
	ts2, _ := newInstallServer(t, "")
	if code, _, _ := get(t, ts2.URL+"/dl/SHA256SUMS"); code != 404 {
		t.Fatal(code)
	}
}

func TestLandingPageShowsRealInstallCommands(t *testing.T) {
	ts, _ := newInstallServer(t, "")
	_, body, _ := get(t, ts.URL+"/")
	for _, want := range []string{"curl -fsSL " + ts.URL + "/install.sh | sh", "irm " + ts.URL + "/install.ps1 | iex"} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page lacks %q", want)
		}
	}
}

// Runs the real install.sh against the real handler with a stub "seatime".
func runInstallSH(t *testing.T, dl string) (out string, err error, installDir, marker string) {
	t.Helper()
	for _, bin := range []string{"sh", "curl", "awk"} {
		if _, e := exec.LookPath(bin); e != nil {
			t.Skip(bin + " not available")
		}
	}
	ts, _ := newInstallServer(t, dl)
	_, script, _ := get(t, ts.URL+"/install.sh")
	home := t.TempDir()
	installDir = filepath.Join(home, ".local", "bin")
	marker = filepath.Join(home, "marker")
	// Pipe the script on stdin exactly like `curl | sh` does.
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "SHELL=/bin/bash", "SEATIME_TEST_OUT=" + marker}
	b, e := cmd.CombinedOutput()
	return string(b), e, installDir, marker
}

func TestInstallSHVerifiesChecksumAndRunsSetup(t *testing.T) {
	dir := t.TempDir()
	writeRelease(t, dir, false)
	out, err, installDir, marker := runInstallSH(t, dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Checksum OK") {
		t.Fatal(out)
	}
	if _, e := os.Stat(filepath.Join(installDir, "seatime")); e != nil {
		t.Fatal("binary not installed", out)
	}
	b, _ := os.ReadFile(marker)
	if !strings.HasPrefix(string(b), "ran: setup --yes --server http://127.0.0.1:") {
		t.Fatalf("setup not run with --yes --server: %q\n%s", b, out)
	}
	rc, _ := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(installDir)), ".bashrc"))
	if !strings.Contains(string(rc), `.local/bin:$PATH" # added by seatime`) {
		t.Fatalf("PATH not added: %q", rc)
	}
}

func TestInstallSHRejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	writeRelease(t, dir, true)
	out, err, installDir, marker := runInstallSH(t, dir)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("must fail: %v\n%s", err, out)
	}
	if _, e := os.Stat(filepath.Join(installDir, "seatime")); e == nil {
		t.Fatal("a binary that failed verification was installed")
	}
	if _, e := os.Stat(marker); e == nil {
		t.Fatal("setup ran after checksum failure")
	}
}

func TestInstallSHFailsClearlyWithoutDownloads(t *testing.T) {
	out, err, _, _ := runInstallSH(t, "")
	if err == nil || !strings.Contains(out, "could not download") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDownloadsServeTheSignedManifest(t *testing.T) {
	dir := t.TempDir()
	writeRelease(t, dir, false)
	os.WriteFile(filepath.Join(dir, "RELEASE"), []byte("seatime-release v0.1.0\n"), 0o644)
	ts, _ := newInstallServer(t, dir)
	if code, body, hdr := get(t, ts.URL+"/dl/RELEASE"); code != 200 || body != "seatime-release v0.1.0\n" || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatal(code, hdr, body)
	}
}

// With SEATIME_DL_UPSTREAM the server holds no binaries: allowlisted names
// redirect there, everything else is still 404.
func TestDownloadsRedirectUpstream(t *testing.T) {
	ts, srv := newInstallServer(t, t.TempDir())
	srv.cfg.DLUpstream = "https://github.com/example/seatime/releases/latest/download"
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for name, want := range map[string]string{
		"RELEASE": srv.cfg.DLUpstream + "/RELEASE", "SHA256SUMS": srv.cfg.DLUpstream + "/SHA256SUMS",
		"seatime-darwin-arm64": srv.cfg.DLUpstream + "/seatime-darwin-arm64", "secret.txt": "",
	} {
		r, err := c.Get(ts.URL + "/dl/" + name)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if want == "" {
			if r.StatusCode != 404 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
			continue
		}
		if r.StatusCode != http.StatusFound || r.Header.Get("Location") != want {
			t.Errorf("%s: %d -> %q", name, r.StatusCode, r.Header.Get("Location"))
		}
	}
}

func TestDownloadUpstreamMustBePlainURL(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := Config{Dev: true, Secret: "s", DLUpstream: `javascript:alert(1)`}
	if _, err := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "SEATIME_DL_UPSTREAM") {
		t.Fatal(err)
	}
}

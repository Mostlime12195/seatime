// Package update keeps seatime current: one stable channel, checked once a
// day by the daemon or on demand by `seatime update`.
//
// A release is a signed manifest (RELEASE: version plus each file's SHA-256,
// ed25519-signed) next to the binaries. The updater trusts the signature,
// checked against the key compiled into this binary, not the host it
// downloads from. It only moves forward to a newer vX.Y.Z, downloads the
// binary for this OS and CPU next to the running one, checks its SHA-256,
// swaps it in (keeping the previous binary as <exe>.prev), and runs
// `seatime doctor --self-test` on it. If that fails, the previous binary goes
// back, the release is never tried again, and the user is told once.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Daily is how often the daemon checks.
const Daily = 24 * time.Hour

const maxBinary = 256 << 20

// ErrDevBuild: development builds never update themselves.
var ErrDevBuild = errors.New("this is a development build; it does not update itself")

// Updater checks for and installs new releases of the binary at Exe.
type Updater struct {
	Version string // running version (main.version)
	Exe     string // the binary to replace (resolved before any swap)
	BaseURL string // where RELEASE and the binaries are, ending in "/"
	Dir     string // data dir: update.json and update.lock live here
	Notify  bool   // leave a notice for the user (the daemon; the CLI prints instead)

	Key      string                                                // base64 ed25519 public key ("" = PublicKeys)
	Client   *http.Client                                          // nil = 15 min timeout, proxy from env
	GOOS     string                                                // "" = runtime.GOOS
	GOARCH   string                                                // "" = runtime.GOARCH
	SelfTest func(ctx context.Context, exe string) (string, error) // nil = run `exe doctor --self-test`
	Aside    *bool                                                 // nil = rename-aside swap on Windows only
	Now      func() time.Time
}

// Result says what Apply did.
type Result struct {
	Current    string // version that was running
	Latest     string // newest signed release found ("" if the check failed)
	Updated    bool   // Latest is now installed at Exe
	RolledBack bool   // Latest failed its self-test; the previous binary is back
	Skipped    bool   // Latest failed its self-test here before; not tried again
	Detail     string // why the self-test failed
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Updater) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 15 * time.Minute}
}

func (u *Updater) aside() bool {
	if u.Aside != nil {
		return *u.Aside
	}
	return u.goos() == "windows"
}

func (u *Updater) keys() []string {
	if u.Key != "" {
		return []string{u.Key}
	}
	return PublicKeys
}

// Auto is the daemon's check: it does nothing when updates are turned off,
// on a development build, or when the last attempt was less than a day ago.
func (u *Updater) Auto(ctx context.Context, enabled bool) (Result, error) {
	if !enabled || !IsRelease(u.Version) {
		return Result{Current: u.Version}, nil
	}
	if since := u.now().Sub(LoadState(u.Dir).LastCheck); since >= 0 && since < Daily {
		return Result{Current: u.Version}, nil
	}
	return u.Apply(ctx)
}

// Latest fetches and verifies the release manifest.
func (u *Updater) Latest(ctx context.Context) (Manifest, error) {
	var pubs []ed25519.PublicKey
	for _, k := range u.keys() {
		if pub, err := ParseKey(k); err == nil {
			pubs = append(pubs, pub)
		}
	}
	if len(pubs) == 0 {
		return Manifest{}, ErrNoKey
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := u.get(ctx, ManifestName)
	if err != nil {
		return Manifest{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("download %s: %w", ManifestName, err)
	}
	return ParseAny(b, u.now(), pubs...)
}

func (u *Updater) get(ctx context.Context, name string) (*http.Response, error) {
	url := u.BaseURL + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("seatime/%s (%s/%s)", u.Version, u.goos(), u.goarch()))
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// Apply checks for a newer release and installs it. A development build
// returns ErrDevBuild without touching the network.
func (u *Updater) Apply(ctx context.Context) (res Result, err error) {
	res.Current = u.Version
	if !IsRelease(u.Version) {
		return res, ErrDevBuild
	}
	unlock, err := lock(u.Dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	st := LoadState(u.Dir)
	st.LastCheck = u.now()
	defer func() {
		st.LastError = ""
		if err != nil {
			st.LastError = err.Error()
		}
		_ = saveState(u.Dir, st)
	}()

	m, err := u.Latest(ctx)
	if err != nil {
		return res, err
	}
	res.Latest, st.Latest = m.Version, m.Version
	if !Newer(m.Version, u.Version) {
		return res, nil
	}
	for _, b := range st.Bad {
		if b == m.Version {
			res.Skipped = true
			return res, nil
		}
	}
	name := AssetName(u.goos(), u.goarch())
	want, ok := m.Files[name]
	if !ok {
		return res, fmt.Errorf("release %s has no %s", m.Version, name)
	}

	dir := filepath.Dir(u.Exe)
	prev := u.Exe + ".prev"
	cleanLeftovers(dir, u.Exe)
	tmp, err := u.download(ctx, name, dir, want)
	if err != nil {
		return res, err
	}
	if err := swap(u.Exe, tmp, prev, u.aside()); err != nil {
		_ = os.Remove(tmp)
		return res, fmt.Errorf("install %s: %w", u.Exe, err)
	}

	test := u.SelfTest
	if test == nil {
		test = runSelfTest
	}
	out, terr := test(ctx, u.Exe)
	if terr == nil && passed(out, m.Version) {
		res.Updated = true
		if u.Notify {
			st.Notice = fmt.Sprintf("SeaTime updated itself to %s.", m.Version)
		}
		return res, nil
	}
	res.Detail = selfTestDetail(out, terr)
	if rerr := rollback(u.Exe, prev); rerr != nil {
		return res, fmt.Errorf("%s failed its self-test (%s) and putting %s back failed too: %v; reinstall SeaTime with the install line from your server's home page",
			m.Version, res.Detail, u.Version, rerr)
	}
	res.RolledBack = true
	st.Bad = append(st.Bad, m.Version)
	if u.Notify {
		st.Notice = fmt.Sprintf("SeaTime %s failed its self-test on this computer, so SeaTime went back to %s. It will try again when a newer version is out.", m.Version, u.Version)
	}
	return res, nil
}

// download fetches name into a temp file in dir (same folder as the binary,
// so the swap is a rename) and checks its SHA-256. On any error, including a
// download cut off half way, the temp file is gone and nothing changed.
func (u *Updater) download(ctx context.Context, name, dir, want string) (path string, err error) {
	resp, err := u.get(ctx, name)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.CreateTemp(dir, ".seatime-new-*")
	if err != nil {
		return "", fmt.Errorf("cannot write next to %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return "", fmt.Errorf("download %s cut off after %d bytes: %w", name, n, err)
	}
	if n > maxBinary {
		return "", fmt.Errorf("download %s: larger than %d MB", name, maxBinary>>20)
	}
	if got := hexOf(h.Sum(nil)); got != want {
		return "", fmt.Errorf("download %s: checksum mismatch (got %s, release says %s)", name, got, want)
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Chmod(f.Name(), 0o755); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// swap puts tmp at exe and keeps the old binary at prev. On Unix the old file
// gets a second name first and exe is then replaced in one rename, so a hook
// starting at that moment always finds a binary. Windows cannot replace a
// running .exe but can rename it, so the old one is moved aside first.
func swap(exe, tmp, prev string, aside bool) error {
	if err := os.Remove(prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the old backup %s: %w", prev, err)
	}
	if aside {
		if err := os.Rename(exe, prev); err != nil {
			return err
		}
		if err := os.Rename(tmp, exe); err != nil {
			_ = os.Rename(prev, exe)
			return err
		}
		return nil
	}
	if err := os.Link(exe, prev); err != nil {
		if err := copyFile(exe, prev); err != nil {
			return err
		}
	}
	return os.Rename(tmp, exe)
}

// rollback puts prev back at exe. If the failed binary is still in use
// (Windows: a hook started it), it is renamed aside first.
func rollback(exe, prev string) error {
	if err := os.Rename(prev, exe); err == nil {
		return nil
	}
	bad := exe + ".bad"
	_ = os.Remove(bad)
	if err := os.Rename(exe, bad); err != nil {
		return err
	}
	return os.Rename(prev, exe)
}

// cleanLeftovers removes temp files from an update that died half way and a
// failed binary that was still running at its rollback.
func cleanLeftovers(dir, exe string) {
	old, _ := filepath.Glob(filepath.Join(dir, ".seatime-new-*"))
	for _, p := range append(old, exe+".bad") {
		_ = os.Remove(p)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SelfTestLine is the last line `seatime doctor --self-test` prints on success.
func SelfTestLine(version string) string { return "self-test ok " + version }

// passed: the self-test exited 0 and reported the version the manifest
// promised (a mislabelled build would otherwise be installed every day).
func passed(out, version string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == SelfTestLine(version) {
			return true
		}
	}
	return false
}

func selfTestDetail(out string, err error) string {
	out = strings.TrimSpace(out)
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	switch {
	case err != nil && out != "":
		return fmt.Sprintf("%v: %s", err, out)
	case err != nil:
		return err.Error()
	case out == "":
		return "no output"
	}
	return "it reported " + out
}

func runSelfTest(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, exe, "doctor", "--self-test")
	hideWindow(c)
	out, err := c.CombinedOutput()
	return string(out), err
}

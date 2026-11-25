package update

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A throwaway key pair; the real one is never in the repo.
func testKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

func sum(b []byte) string { s := sha256.Sum256(b); return hexOf(s[:]) }

// release is a fake download host for one release.
type release struct {
	version  string
	binary   []byte // what is served as the binary
	listed   []byte // what the manifest's checksum is computed from (nil = binary)
	signer   ed25519.PrivateKey
	cutOff   bool // send half the binary, then drop the connection
	hits     atomic.Int32
	binHits  atomic.Int32
	manifest []byte
}

func (r *release) serve(t *testing.T) *httptest.Server {
	t.Helper()
	listed := r.listed
	if listed == nil {
		listed = r.binary
	}
	name := AssetName("linux", "amd64")
	m, err := Sign(Manifest{Version: r.version, Files: map[string]string{
		name:                          sum(listed),
		AssetName("windows", "amd64"): sum([]byte("other")),
		AssetName("darwin", "arm64"):  sum([]byte("other")),
	}}, r.signer)
	if err != nil {
		t.Fatal(err)
	}
	r.manifest = m
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.hits.Add(1)
		switch q.URL.Path {
		case "/dl/" + ManifestName:
			w.Write(r.manifest)
		case "/dl/" + name:
			r.binHits.Add(1)
			w.Header().Set("Content-Length", fmt.Sprint(len(r.binary)))
			if !r.cutOff {
				w.Write(r.binary)
				return
			}
			w.Write(r.binary[:len(r.binary)/2])
			w.(http.Flusher).Flush()
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err == nil {
				buf.Flush()
				conn.Close()
			}
		default:
			http.NotFound(w, q)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// fakeSelfTest passes when the installed file says "good <version>".
func fakeSelfTest(calls *int) func(context.Context, string) (string, error) {
	return func(_ context.Context, exe string) (string, error) {
		*calls++
		b, err := os.ReadFile(exe)
		if err != nil {
			return "", err
		}
		v, ok := strings.CutPrefix(string(b), "good ")
		if !ok {
			return "panic: runtime error\n", errors.New("exit status 2")
		}
		return "store ok\n" + SelfTestLine(v) + "\n", nil
	}
}

type env struct {
	u     *Updater
	exe   string
	tests int
}

func newEnv(t *testing.T, ts *httptest.Server, pub string, aside bool) *env {
	t.Helper()
	bin := t.TempDir()
	e := &env{exe: filepath.Join(bin, "seatime")}
	if err := os.WriteFile(e.exe, []byte("good v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.u = &Updater{Version: "v1.0.0", Exe: e.exe, Dir: t.TempDir(), Key: pub, Notify: true,
		GOOS: "linux", GOARCH: "amd64", Aside: &aside, SelfTest: fakeSelfTest(&e.tests)}
	if ts != nil {
		e.u.BaseURL = ts.URL + "/dl/"
	}
	return e
}

func (e *env) content(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// Nothing but the binary (and the old one, after an update) is left in its folder.
func (e *env) noLeftovers(t *testing.T, want ...string) {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Dir(e.exe))
	var got []string
	for _, x := range ents {
		got = append(got, x.Name())
	}
	if strings.Join(got, ",") != strings.Join(append([]string{"seatime"}, want...), ",") {
		t.Fatalf("binary folder holds %v", got)
	}
}

func TestGoodUpdate(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprintf("aside=%v", aside), func(t *testing.T) {
			pub, priv := testKey(t)
			r := &release{version: "v1.2.0", binary: []byte("good v1.2.0"), signer: priv}
			e := newEnv(t, r.serve(t), pub, aside)
			res, err := e.u.Apply(context.Background())
			if err != nil || !res.Updated || res.Latest != "v1.2.0" || res.RolledBack {
				t.Fatalf("%+v %v", res, err)
			}
			if got := e.content(t, e.exe); got != "good v1.2.0" {
				t.Fatalf("exe = %q", got)
			}
			if got := e.content(t, e.exe+".prev"); got != "good v1.0.0" {
				t.Fatalf("previous binary not kept: %q", got)
			}
			if fi, _ := os.Stat(e.exe); fi.Mode().Perm()&0o100 == 0 {
				t.Fatalf("new binary not executable: %v", fi.Mode())
			}
			e.noLeftovers(t, "seatime.prev")
			if n := TakeNotice(e.u.Dir); n != "SeaTime updated itself to v1.2.0." {
				t.Fatalf("notice %q", n)
			}
			if n := TakeNotice(e.u.Dir); n != "" {
				t.Fatalf("told twice: %q", n)
			}
			if st := LoadState(e.u.Dir); st.LastError != "" || st.Latest != "v1.2.0" {
				t.Fatalf("%+v", st)
			}
		})
	}
}

// The updater running as the new version finds nothing newer.
func TestUpToDateAndNoDowngrade(t *testing.T) {
	pub, priv := testKey(t)
	for _, v := range []string{"v1.0.0", "v0.9.9"} {
		r := &release{version: v, binary: []byte("good " + v), signer: priv}
		e := newEnv(t, r.serve(t), pub, false)
		res, err := e.u.Apply(context.Background())
		if err != nil || res.Updated || res.Latest != v || r.binHits.Load() != 0 {
			t.Fatalf("%s: %+v %v, %d downloads", v, res, err, r.binHits.Load())
		}
		if e.content(t, e.exe) != "good v1.0.0" {
			t.Fatal("binary changed")
		}
		e.noLeftovers(t)
	}
}

func TestBadChecksum(t *testing.T) {
	pub, priv := testKey(t)
	r := &release{version: "v1.2.0", binary: []byte("good v1.2.0 but tampered"), listed: []byte("good v1.2.0"), signer: priv}
	e := newEnv(t, r.serve(t), pub, false)
	res, err := e.u.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || res.Updated {
		t.Fatalf("%+v %v", res, err)
	}
	if e.content(t, e.exe) != "good v1.0.0" || e.tests != 0 {
		t.Fatal("tampered binary was installed or run")
	}
	e.noLeftovers(t)
	if st := LoadState(e.u.Dir); !strings.Contains(st.LastError, "checksum") {
		t.Fatalf("error not recorded: %+v", st)
	}
}

func TestBadSignature(t *testing.T) {
	pub, _ := testKey(t)
	_, other := testKey(t)
	r := &release{version: "v1.2.0", binary: []byte("good v1.2.0"), signer: other}
	e := newEnv(t, r.serve(t), pub, false)
	if _, err := e.u.Apply(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatal(err)
	}
	if r.binHits.Load() != 0 || e.content(t, e.exe) != "good v1.0.0" {
		t.Fatal("downloaded or installed despite a bad signature")
	}

	// a correctly signed manifest edited afterwards (version bumped) fails too
	_, priv := testKey(t)
	r2 := &release{version: "v1.2.0", binary: []byte("good v1.2.0"), signer: priv}
	ts := r2.serve(t)
	pub2 := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	r2.manifest = []byte(strings.Replace(string(r2.manifest), "v1.2.0", "v9.0.0", 1))
	e2 := newEnv(t, ts, pub2, false)
	if _, err := e2.u.Apply(context.Background()); !errors.Is(err, ErrBadSignature) {
		t.Fatal(err)
	}
	if _, err := Parse([]byte("seatime-release v1.2.0\n"), priv.Public().(ed25519.PublicKey)); !errors.Is(err, ErrBadSignature) {
		t.Fatal("unsigned manifest accepted:", err)
	}
}

func TestFailedSelfTestRollsBackAndTellsOnce(t *testing.T) {
	for _, aside := range []bool{false, true} {
		t.Run(fmt.Sprintf("aside=%v", aside), func(t *testing.T) {
			pub, priv := testKey(t)
			r := &release{version: "v1.2.0", binary: []byte("crashes on start"), signer: priv}
			e := newEnv(t, r.serve(t), pub, aside)
			res, err := e.u.Apply(context.Background())
			if err != nil || !res.RolledBack || res.Updated || !strings.Contains(res.Detail, "exit status 2") {
				t.Fatalf("%+v %v", res, err)
			}
			if e.content(t, e.exe) != "good v1.0.0" || e.tests != 1 {
				t.Fatalf("not rolled back: %q (%d self-tests)", e.content(t, e.exe), e.tests)
			}
			e.noLeftovers(t)
			want := "SeaTime v1.2.0 failed its self-test on this computer, so SeaTime went back to v1.0.0. It will try again when a newer version is out."
			if n := TakeNotice(e.u.Dir); n != want {
				t.Fatalf("notice %q", n)
			}
			if n := TakeNotice(e.u.Dir); n != "" {
				t.Fatalf("told twice: %q", n)
			}
			// the bad release is not downloaded or tried again
			res, err = e.u.Apply(context.Background())
			if err != nil || !res.Skipped || r.binHits.Load() != 1 || e.tests != 1 {
				t.Fatalf("retried a rolled-back release: %+v %v", res, err)
			}
			if n := TakeNotice(e.u.Dir); n != "" {
				t.Fatalf("told again: %q", n)
			}
		})
	}
}

// A binary that runs but reports another version (a mislabelled build) is
// rolled back too; otherwise it would be reinstalled every day.
func TestSelfTestMustReportTheReleaseVersion(t *testing.T) {
	pub, priv := testKey(t)
	r := &release{version: "v1.2.0", binary: []byte("good v1.1.9"), signer: priv}
	e := newEnv(t, r.serve(t), pub, false)
	res, err := e.u.Apply(context.Background())
	if err != nil || !res.RolledBack || e.content(t, e.exe) != "good v1.0.0" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestDownloadCutOffHalfWay(t *testing.T) {
	pub, priv := testKey(t)
	r := &release{version: "v1.2.0", binary: []byte(strings.Repeat("x", 1<<20)), signer: priv, cutOff: true}
	e := newEnv(t, r.serve(t), pub, false)
	res, err := e.u.Apply(context.Background())
	if err == nil || res.Updated || !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("%+v %v", res, err)
	}
	if e.content(t, e.exe) != "good v1.0.0" || e.tests != 0 {
		t.Fatal("a partial download was installed or run")
	}
	e.noLeftovers(t)
}

// A download that ends early without an error (no Content-Length, the
// connection just closes) is caught by the checksum.
func TestShortDownloadWithoutLength(t *testing.T) {
	pub, priv := testKey(t)
	name := AssetName("linux", "amd64")
	full := []byte("good v1.2.0")
	m, _ := Sign(Manifest{Version: "v1.2.0", Files: map[string]string{name: sum(full)}}, priv)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, ManifestName) {
			w.Write(m)
			return
		}
		conn, buf, _ := w.(http.Hijacker).Hijack()
		buf.WriteString("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
		buf.Write(full[:5])
		buf.Flush()
		conn.Close()
	}))
	defer ts.Close()
	e := newEnv(t, ts, pub, false)
	if _, err := e.u.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal(err)
	}
	e.noLeftovers(t)
}

func TestUpdatesTurnedOff(t *testing.T) {
	pub, priv := testKey(t)
	r := &release{version: "v1.2.0", binary: []byte("good v1.2.0"), signer: priv}
	e := newEnv(t, r.serve(t), pub, false)
	res, err := e.u.Auto(context.Background(), false)
	if err != nil || res.Updated || r.hits.Load() != 0 {
		t.Fatalf("%+v %v, %d requests", res, err, r.hits.Load())
	}
}

func TestDevBuildNeverUpdates(t *testing.T) {
	pub, priv := testKey(t)
	r := &release{version: "v99.0.0", binary: []byte("good v99.0.0"), signer: priv}
	ts := r.serve(t)
	for _, v := range []string{"dev", "", "0fea046", "v1.0.0-3-g0fea046", "v1.0.0-dirty", "1.0.0"} {
		e := newEnv(t, ts, pub, false)
		e.u.Version = v
		if _, err := e.u.Apply(context.Background()); !errors.Is(err, ErrDevBuild) {
			t.Fatalf("%q: %v", v, err)
		}
		if res, err := e.u.Auto(context.Background(), true); err != nil || res.Updated {
			t.Fatalf("%q: %+v %v", v, res, err)
		}
		if e.content(t, e.exe) != "good v1.0.0" {
			t.Fatalf("%q: replaced", v)
		}
	}
	if r.hits.Load() != 0 {
		t.Fatalf("a dev build made %d requests", r.hits.Load())
	}
}

func TestPlaceholderKeyRefuses(t *testing.T) {
	if _, err := ParseKey(PublicKey); !errors.Is(err, ErrNoKey) {
		t.Skip("a real release key is compiled in")
	}
	_, priv := testKey(t)
	r := &release{version: "v1.2.0", binary: []byte("good v1.2.0"), signer: priv}
	e := newEnv(t, r.serve(t), "", false) // "" = the compiled-in PublicKey
	if _, err := e.u.Apply(context.Background()); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	if _, err := e.u.Latest(context.Background()); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	if r.hits.Load() != 0 || e.content(t, e.exe) != "good v1.0.0" {
		t.Fatal("updated without a key")
	}
	if st := LoadState(e.u.Dir); st.LastCheck.IsZero() || st.LastError == "" {
		t.Fatalf("attempt not recorded (the daemon would retry hourly): %+v", st)
	}
}

func TestAutoChecksOncePerDay(t *testing.T) {
	pub, priv := testKey(t)
	r := &release{version: "v1.0.0", binary: []byte("good v1.0.0"), signer: priv}
	e := newEnv(t, r.serve(t), pub, false)
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	e.u.Now = func() time.Time { return now }
	for i, step := range []struct {
		after time.Duration
		hits  int32
	}{{0, 1}, {time.Hour, 1}, {22 * time.Hour, 1}, {2 * time.Hour, 2}} {
		now = now.Add(step.after)
		if _, err := e.u.Auto(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if r.hits.Load() != step.hits {
			t.Fatalf("step %d: %d requests, want %d", i, r.hits.Load(), step.hits)
		}
	}
}

func TestOneUpdaterAtATime(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(dir); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	unlock()
	unlock, err = lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	// a lock left by a process that died long ago is taken over
	p := filepath.Join(dir, "update.lock")
	os.WriteFile(p, nil, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	if unlock, err = lock(dir); err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestManifestFormat(t *testing.T) {
	pubS, priv := testKey(t)
	pub, err := ParseKey(pubS)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "v0.3.0", Files: map[string]string{
		"seatime-linux-arm64": sum([]byte("a")), "seatime-darwin-amd64": sum([]byte("b")), "seatime-windows-386.exe": sum([]byte("c")),
	}}
	b, err := Sign(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 5 || lines[0] != "seatime-release v0.3.0" || !strings.HasSuffix(lines[1], "  seatime-darwin-amd64") || !strings.HasPrefix(lines[4], "ed25519 ") {
		t.Fatalf("%q", lines)
	}
	got, err := Parse(b, pub)
	if err != nil || got.Version != "v0.3.0" || len(got.Files) != 3 || got.Files["seatime-linux-arm64"] != sum([]byte("a")) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []Manifest{
		{Version: "v0.3.0-dirty", Files: m.Files},
		{Version: "v0.3.0", Files: map[string]string{"../etc/passwd": sum(nil)}},
		{Version: "v0.3.0", Files: map[string]string{"seatime-linux-amd64": "abc"}},
		{Version: "v0.3.0"},
	} {
		if _, err := Sign(bad, priv); err == nil {
			t.Errorf("signed %+v", bad)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.0.1", "v1.0.0", true}, {"v1.10.0", "v1.9.9", true}, {"v2.0.0", "v1.99.99", true},
		{"v1.0.0", "v1.0.0", false}, {"v1.0.0", "v1.0.1", false}, {"v1.0.1", "dev", false}, {"dev", "v1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// A manifest signed by any listed key is accepted (a key rotation),
// one signed by another key is not, and an expired manifest is refused.
func TestKeyRotationAndExpiry(t *testing.T) {
	oldPub, oldPriv, _ := ed25519.GenerateKey(nil)
	newPub, newPriv, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	m := Manifest{Version: "v1.2.0", Files: map[string]string{"seatime-linux-amd64": strings.Repeat("a", 64)}}
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for _, priv := range []ed25519.PrivateKey{oldPriv, newPriv} {
		b, _ := Sign(m, priv)
		if _, err := ParseAny(b, now, oldPub, newPub); err != nil {
			t.Fatalf("a listed key was refused: %v", err)
		}
	}
	b, _ := Sign(m, otherPriv)
	if _, err := ParseAny(b, now, oldPub, newPub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an unlisted key: %v", err)
	}
	m.Expires = now.Add(time.Hour)
	b, _ = Sign(m, newPriv)
	if got, err := ParseAny(b, now, newPub); err != nil || !got.Expires.Equal(m.Expires) {
		t.Fatalf("before expiry: %v %v", got.Expires, err)
	}
	if _, err := ParseAny(b, now.Add(2*time.Hour), newPub); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("after expiry: %v", err)
	}
}

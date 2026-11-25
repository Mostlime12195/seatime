package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ManifestName is the signed release manifest, published next to the
// binaries and SHA256SUMS. It is plain text:
//
//	seatime-release v0.3.0
//	<sha256 hex>  seatime-darwin-amd64
//	...
//	ed25519 <base64 signature of every byte above this line>
const ManifestName = "RELEASE"

const maxManifest = 64 << 10

var (
	// ErrNoKey: this build has no release key compiled in (key.go).
	ErrNoKey = errors.New("this build has no release signing key, so it cannot update itself")
	// ErrBadSignature: the manifest was not signed by the compiled-in key.
	ErrBadSignature = errors.New("the release manifest's signature does not match SeaTime's release key")
)

// Manifest is a verified release: its version and the SHA-256 of each file.
type Manifest struct {
	Version string
	Files   map[string]string // file name -> lowercase hex SHA-256
	// Expires, when set, is when the manifest stops being accepted: a
	// mirror cannot keep serving an old signed release past it
	// (seatime-release sign -expires).
	Expires time.Time
}

// AssetRE matches the release binaries' file names.
var AssetRE = regexp.MustCompile(`^seatime-(windows|darwin|linux)-(amd64|386|arm64)(\.exe)?$`)

var hexSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AssetName is the release file for an OS and CPU.
func AssetName(goos, goarch string) string {
	n := "seatime-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// ParseKey decodes a base64 ed25519 public key. Anything else, including the
// placeholder in key.go, is ErrNoKey.
func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrNoKey
	}
	return ed25519.PublicKey(b), nil
}

func (m Manifest) body() ([]byte, error) {
	if !IsRelease(m.Version) {
		return nil, fmt.Errorf("version %q is not a release version (vX.Y.Z)", m.Version)
	}
	if len(m.Files) == 0 {
		return nil, errors.New("no files")
	}
	names := make([]string, 0, len(m.Files))
	for n, h := range m.Files {
		if !AssetRE.MatchString(n) {
			return nil, fmt.Errorf("%q is not a release file name", n)
		}
		if !hexSum.MatchString(h) {
			return nil, fmt.Errorf("%s: %q is not a SHA-256", n, h)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	fmt.Fprintf(&b, "seatime-release %s\n", m.Version)
	if !m.Expires.IsZero() {
		fmt.Fprintf(&b, "expires %s\n", m.Expires.UTC().Format(time.RFC3339))
	}
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", m.Files[n], n)
	}
	return b.Bytes(), nil
}

// Sign returns the manifest file signed with priv.
func Sign(m Manifest, priv ed25519.PrivateKey) ([]byte, error) {
	b, err := m.body()
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, b)
	return append(b, "ed25519 "+base64.StdEncoding.EncodeToString(sig)+"\n"...), nil
}

// Parse checks the signature against pub, then reads the manifest. Nothing in
// an unsigned or wrongly signed file is looked at.
func Parse(b []byte, pub ed25519.PublicKey) (Manifest, error) {
	return ParseAny(b, time.Now(), pub)
}

// ParseAny is Parse accepting a signature by any of pubs, and refusing a
// manifest whose expiry is before now.
func ParseAny(b []byte, now time.Time, pubs ...ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	if len(b) > maxManifest {
		return m, errors.New("release manifest is too large")
	}
	i := bytes.LastIndex(b, []byte("\ned25519 "))
	if i < 0 {
		return m, ErrBadSignature
	}
	body, sigLine := b[:i+1], strings.TrimSpace(string(b[i+1:]))
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sigLine, "ed25519 "))
	if err != nil || len(sig) != ed25519.SignatureSize || !slices.ContainsFunc(pubs, func(pub ed25519.PublicKey) bool {
		return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, body, sig)
	}) {
		return m, ErrBadSignature
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	v, ok := strings.CutPrefix(lines[0], "seatime-release ")
	if !ok || !IsRelease(v) {
		return m, fmt.Errorf("release manifest: bad first line %q", lines[0])
	}
	m = Manifest{Version: v, Files: map[string]string{}}
	rest := lines[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "expires ") {
		t, err := time.Parse(time.RFC3339, strings.TrimPrefix(rest[0], "expires "))
		if err != nil {
			return Manifest{}, fmt.Errorf("release manifest: bad line %q", rest[0])
		}
		if now.After(t) {
			return Manifest{}, fmt.Errorf("release manifest expired %s", t.Format(time.RFC3339))
		}
		m.Expires, rest = t, rest[1:]
	}
	for _, l := range rest {
		f := strings.Fields(l)
		if len(f) != 2 || !hexSum.MatchString(f[0]) || !AssetRE.MatchString(f[1]) {
			return Manifest{}, fmt.Errorf("release manifest: bad line %q", l)
		}
		m.Files[f[1]] = f[0]
	}
	return m, nil
}

var relVer = regexp.MustCompile(`^v(\d{1,6})\.(\d{1,6})\.(\d{1,6})$`)

// IsRelease reports whether v is a release version, vX.Y.Z. Anything else
// ("dev", a `git describe` build between tags, a dirty tree) is a
// development build, which never updates itself.
func IsRelease(v string) bool { return relVer.MatchString(v) }

// Newer reports whether release version a is later than b.
func Newer(a, b string) bool {
	pa, pb := relVer.FindStringSubmatch(a), relVer.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// hexOf is a lowercase hex SHA-256 sum.
func hexOf(sum []byte) string { return hex.EncodeToString(sum) }

// Command seatime-release makes the release signing key and signs releases.
//
//	go run ./cmd/seatime-release keygen KEYFILE
//	    write a new private key to KEYFILE (refuses to overwrite) and print the
//	    public key to paste into internal/client/update/key.go
//	go run ./cmd/seatime-release check -key KEYFILE -version vX.Y.Z
//	    fail early if the key does not match the compiled-in public key or the
//	    version is not a release version
//	go run ./cmd/seatime-release sign -key KEYFILE -version vX.Y.Z DIR
//	    write DIR/RELEASE: the version and SHA-256 of every seatime-* binary
//	    in DIR, signed with the key
//
// scripts/release.sh runs check and sign when SEATIME_SIGNING_KEY is set.
// The private key belongs on the machine that cuts releases, never on the
// server: seatimed and the download host only serve the signed files.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/client/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "seatime-release:", err)
		os.Exit(1)
	}
}

const usage = "usage: seatime-release keygen KEYFILE | check -key KEYFILE -version vX.Y.Z | sign -key KEYFILE -version vX.Y.Z DIR"

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return errors.New("usage: seatime-release keygen KEYFILE")
		}
		pub, err := keygen(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote the private key to %s. Keep it secret and backed up; never put it on the server.\n\n", args[1])
		fmt.Fprintf(out, "Public key:\n\n  %s\n\nSet it in internal/client/update/key.go:\n\n  const PublicKey = %q\n", pub, pub)
		return nil
	case "check", "sign":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		keyPath := fs.String("key", "", "private key file (from keygen)")
		ver := fs.String("version", "", "release version, vX.Y.Z")
		expires := fs.Duration("expires", 0, "sign: the manifest stops being accepted this long after now (0: never)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		priv, err := loadKey(*keyPath, update.PublicKeys)
		if err != nil {
			return err
		}
		if !update.IsRelease(*ver) {
			return fmt.Errorf("version %q is not a release version (vX.Y.Z): tag the commit or set VERSION", *ver)
		}
		if args[0] == "check" {
			return nil
		}
		if fs.NArg() != 1 {
			return errors.New("usage: seatime-release sign -key KEYFILE -version vX.Y.Z DIR")
		}
		var exp time.Time
		if *expires > 0 {
			exp = time.Now().Add(*expires).UTC().Truncate(time.Second)
		}
		n, err := signDir(fs.Arg(0), *ver, priv, exp)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s (%s, %d files)\n", filepath.Join(fs.Arg(0), update.ManifestName), *ver, n)
		return nil
	}
	return errors.New(usage)
}

// keygen writes a new private key (base64 seed) to path, mode 0600, and
// returns the base64 public key.
func keygen(path string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w (keygen never overwrites a key)", err)
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// loadKey reads a private key and makes sure its public half is compiledKey:
// a release signed with any other key would be refused by every client.
// compiledKeys are update.PublicKeys (any of them will do, for a rotation).
func loadKey(path string, compiledKeys []string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("no key: pass -key KEYFILE (scripts/release.sh: SEATIME_SIGNING_KEY=KEYFILE)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a key made by `seatime-release keygen`", path)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if len(compiledKeys) == 0 || compiledKeys[0] == "" {
		return nil, fmt.Errorf("internal/client/update/key.go has no public key yet: set it to %s and rebuild", pub)
	}
	for _, k := range compiledKeys {
		if _, err := update.ParseKey(k); err == nil && k == pub {
			return priv, nil
		}
	}
	return nil, fmt.Errorf("this key's public half (%s) is not the one in internal/client/update/key.go (PublicKeys %v); clients would refuse the release", pub, compiledKeys)
}

// signDir writes dir/RELEASE for the seatime-* binaries in dir.
func signDir(dir, version string, priv ed25519.PrivateKey, expires time.Time) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	m := update.Manifest{Version: version, Files: map[string]string{}, Expires: expires}
	for _, e := range ents {
		if !e.Type().IsRegular() || !update.AssetRE.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return 0, err
		}
		s := sha256.Sum256(b)
		m.Files[e.Name()] = hex.EncodeToString(s[:])
	}
	if len(m.Files) == 0 {
		return 0, fmt.Errorf("no seatime-* binaries in %s", dir)
	}
	b, err := update.Sign(m, priv)
	if err != nil {
		return 0, err
	}
	p := filepath.Join(dir, update.ManifestName)
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return 0, err
	}
	return len(m.Files), os.Rename(p+".tmp", p)
}

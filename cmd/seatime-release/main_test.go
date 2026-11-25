package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/update"
)

func TestKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "release.key")
	pub, err := keygen(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode())
	}
	if _, err := keygen(keyPath); err == nil {
		t.Fatal("keygen overwrote a key")
	}

	// the key must match the compiled-in public key
	if _, err := loadKey(keyPath, update.PublicKeys); err == nil || !strings.Contains(err.Error(), "key.go") {
		t.Fatalf("signed with the placeholder in key.go: %v", err)
	}
	if _, err := loadKey(keyPath, []string{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}); err == nil || !strings.Contains(err.Error(), "not the one") {
		t.Fatalf("signed with a key clients do not have: %v", err)
	}
	priv, err := loadKey(keyPath, []string{pub})
	if err != nil {
		t.Fatal(err)
	}

	dist := filepath.Join(dir, "dist")
	os.Mkdir(dist, 0o755)
	files := map[string]string{"seatime-linux-amd64": "L", "seatime-windows-amd64.exe": "W", "SHA256SUMS": "x", "notes.txt": "y"}
	for n, c := range files {
		os.WriteFile(filepath.Join(dist, n), []byte(c), 0o644)
	}
	if _, err := signDir(dist, "v0.1.0-dirty", priv, time.Time{}); err == nil {
		t.Fatal("signed a dev version")
	}
	n, err := signDir(dist, "v0.1.0", priv, time.Time{})
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	b, _ := os.ReadFile(filepath.Join(dist, update.ManifestName))
	pk, _ := update.ParseKey(pub)
	m, err := update.Parse(b, pk)
	if err != nil || m.Version != "v0.1.0" || len(m.Files) != 2 {
		t.Fatalf("%+v %v\n%s", m, err, b)
	}
	s := sha256.Sum256([]byte("W"))
	if m.Files["seatime-windows-amd64.exe"] != hex.EncodeToString(s[:]) {
		t.Fatal("wrong checksum")
	}
	if _, err := update.Parse(b, priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
}

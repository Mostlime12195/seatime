// Package localauth protects seatime's localhost HTTP endpoints.
//
// Threat model: any web page can make the browser send requests to
// 127.0.0.1:47823 (directly, or after a DNS-rebinding trick). A page must not
// be able to forge prompts or heartbeats. Layers, all enforced by the daemon:
//
//  1. Host must be a loopback name (defeats DNS rebinding).
//  2. A request carrying an Origin header comes from a browser and is
//     refused, even with the secret.
//  3. Writers must present the per-install secret (Authorization: Bearer ...).
//     Scripts read it from the 0600 file in the data dir (`seatime token`).
//
// State lives in <data dir>/local_auth.json (mode 0600), shared by the CLI and
// the daemon (re-read on every use; it is tiny and requests are rare).
package localauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"seatime/internal/client/config"
)

// File is where the state is stored.
func File() string { return filepath.Join(config.DataDir(), "local_auth.json") }

// State is the persisted state.
type State struct {
	Secret string `json:"secret"`
}

var mu sync.Mutex // serialises read-modify-write within one process

func load() State {
	var s State
	if b, err := os.ReadFile(File()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func save(s State) error {
	p := File()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, p)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no entropy: refuse to run rather than mint a guessable secret
	}
	return hex.EncodeToString(b)
}

// withSecret loads the state, minting the secret if needed.
func withSecret() (State, error) {
	s := load()
	if len(s.Secret) < 32 {
		s.Secret = randHex(32)
		if err := save(s); err != nil {
			return s, err
		}
	}
	return s, nil
}

// Secret returns the per-install secret, creating it on first use.
func Secret() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	s, err := withSecret()
	return s.Secret, err
}

// Check reports whether tok is the install secret (constant time). It never
// creates a secret: with none minted yet, nothing authenticates.
func Check(tok string) bool {
	mu.Lock()
	defer mu.Unlock()
	s := load()
	return len(s.Secret) >= 32 && len(tok) == len(s.Secret) &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(s.Secret)) == 1
}

// Rotate replaces the secret (scripts holding the old one stop working).
func Rotate() error {
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(File()); os.IsNotExist(err) {
		return nil
	}
	return save(State{Secret: randHex(32)})
}

// Remove deletes the state file (uninstall).
func Remove() error {
	mu.Lock()
	defer mu.Unlock()
	err := os.Remove(File())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// HostOK reports whether the Host header names the loopback interface. It is
// the DNS-rebinding defence: a rebound page still sends its own host name.
func HostOK(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	switch strings.ToLower(h) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// Decision is the verdict for one request.
type Decision int

const (
	Allow Decision = iota
	DenyHost
	DenyOrigin
	DenySecret
)

// Authorize applies the policy to a request's Host, Origin and Authorization
// headers. Requests without an Origin (CLI tools, scripts) need only the
// secret; anything a browser sends carries an Origin and is refused.
func Authorize(host, origin, authorization string) Decision {
	if !HostOK(host) {
		return DenyHost
	}
	if origin != "" {
		return DenyOrigin
	}
	tok, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || !Check(strings.TrimSpace(tok)) {
		return DenySecret
	}
	return Allow
}

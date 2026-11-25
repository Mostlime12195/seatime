package localauth

import (
	"os"
	"path/filepath"
	"testing"
)

func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("SEATIME_HOME", t.TempDir())
}

func TestSecretCreatedOnceAndPrivate(t *testing.T) {
	sandbox(t)
	if Check("anything") {
		t.Fatal("no secret minted yet: nothing may authenticate")
	}
	a, err := Secret()
	if err != nil || len(a) < 32 {
		t.Fatal(a, err)
	}
	b, _ := Secret()
	if a != b || !Check(a) || Check(a+"x") || Check("") || Check(a[:len(a)-1]) {
		t.Fatal("secret must be stable and exactly matched")
	}
	if fi, err := os.Stat(File()); err != nil || fi.Mode().Perm()&0o077 != 0 && filepath.Separator == '/' {
		t.Fatalf("state file must be private: %v %v", fi, err)
	}
}

func TestAuthorizeMatrix(t *testing.T) {
	sandbox(t)
	sec, _ := Secret()
	b := "Bearer " + sec
	cases := []struct {
		host, origin, auth string
		want               Decision
	}{
		{"127.0.0.1:47823", "", b, Allow},
		{"localhost:47823", "", b, Allow},
		{"[::1]:47823", "", b, Allow},
		{"127.0.0.1:47823", "chrome-extension://abc", b, DenyOrigin},
		{"127.0.0.1:47823", "https://chatgpt.com", b, DenyOrigin},
		{"127.0.0.1:47823", "null", b, DenyOrigin},
		{"127.0.0.1:47823", "", "", DenySecret},
		{"127.0.0.1:47823", "", "Bearer wrong", DenySecret},
		{"127.0.0.1:47823", "chrome-extension://abc", "", DenyOrigin},
		{"attacker.example:47823", "", b, DenyHost},
		{"127.0.0.1.attacker.example", "", b, DenyHost},
		{"", "", b, DenyHost},
	}
	for _, c := range cases {
		if got := Authorize(c.host, c.origin, c.auth); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}

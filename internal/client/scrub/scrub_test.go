package scrub

import (
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	cases := []struct{ in, mustNot string }{
		{"export OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwx", "abcdefghijklmnop"},
		{"my key is sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA ok", "AAAAAAAAAAAA"},
		{"token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdef"},
		{"AKIAIOSFODNN7EXAMPLE leaked", "AKIAIOSFODNN7EXAMPLE"},
		{"curl -H 'Authorization: Bearer abcdef0123456789abcdef' x", "abcdef0123456789abcdef"},
		{"git clone https://bob:hunter22@github.com/a/b", "hunter22"},
		{"mail me at kid@example.com please", "kid@example.com"},
		{"run with --password hunter2 now", "hunter2"},
		{"DB_PASSWORD=\"correct horse\"", "correct horse"},
		{"{\"api_key\": \"abc123def456\"}", "abc123def456"},
		{"cat /home/alice/project/x.go", "alice"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----", "MIIEow"},
		{"STRIPE=aB3dE5gH7jK9mN1pQ3rS5", "aB3dE5gH7jK9"},
		// diff and untracked-file lines start with "+"
		{"+MY_SERVICE_KEY=8f7a6b5c4d3e2f1a0b9c8d7e", "8f7a6b5c4d3e"},
		{"+OPENROUTER_KEY=or-v1-0123456789abcdef0123", "0123456789abcdef"},
		{"-STRIPE_KEY=rk_9a8b7c6d5e4f3a2b1c0d", "9a8b7c6d5e4f"},
		{"+DATABASE_URL_PART=Zx9Yw8Vu7Ts6Rq5Po4Nm", "Zx9Yw8Vu7Ts6"},
		{`const stripeKey = "abc123def456ghi"`, "abc123def456ghi"},
	}
	for _, c := range cases {
		out, f := Scrub(c.in)
		if strings.Contains(out, c.mustNot) || len(f) == 0 {
			t.Errorf("not scrubbed: %q -> %q (%v)", c.in, out, f)
		}
	}
}

func TestScrubKeepsNormalText(t *testing.T) {
	for _, s := range []string{
		"fix the failing test in parser.go",
		"git remote add origin git@github.com:user/repo.git",
		"npm i @types/node@18.0.0",
		"commit 3f786850e387550fdab836ed7e6dc881de23001b",
		"why does the token count go up? the tokenizer is slow",
	} {
		if out, f := Scrub(s); out != s {
			t.Errorf("changed %q -> %q (%v)", s, out, f)
		}
	}
}

func TestSecretPath(t *testing.T) {
	for _, p := range []string{".env", "app/.env.local", "certs/server.pem", "tls.key", "id_rsa", "id_ed25519.pub", "C:\\x\\.env"} {
		if !SecretPath(p) {
			t.Errorf("SecretPath(%q) = false", p)
		}
	}
	for _, p := range []string{"main.go", "env.go", "docs/keys.md", "id_test.go", "environment.ts"} {
		if SecretPath(p) {
			t.Errorf("SecretPath(%q) = true", p)
		}
	}
}

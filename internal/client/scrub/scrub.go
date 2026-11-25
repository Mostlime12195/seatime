// Package scrub removes secrets and obvious personal data from free text
// (prompts, shell commands) before it is stored or submitted.
package scrub

import (
	"regexp"
	"sort"
	"strings"
)

type rule struct {
	kind string
	re   *regexp.Regexp
	repl string // replacement; may use $1
}

// secretKey matches a name that holds a secret. A plain "key" counts when it
// ends the name (MY_SERVICE_KEY, stripeKey): over-redacting a value is
// cheap, sending a key to a model provider is not.
const secretKey = `(?:api[_-]?key|apikey|secret|token|passwd|password|passphrase|pwd|private[_-]?key|access[_-]?key|auth|credential|client[_-]?secret|bearer|session[_-]?id|cookie|key\b)`

var rules = []rule{
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED:private-key]"},
	{"aws-key", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b`), "[REDACTED:aws-key]"},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{20,})\b`), "[REDACTED:github-token]"},
	{"api-key", regexp.MustCompile(`\b(?:sk|rk|pk)[-_](?:live[-_]|test[-_]|ant[-_]|proj[-_])?[A-Za-z0-9_\-]{20,}`), "[REDACTED:api-key]"},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "[REDACTED:slack-token]"},
	{"webhook", regexp.MustCompile(`https://(?:hooks\.slack\.com/services|discord(?:app)?\.com/api/webhooks)/[A-Za-z0-9/_\-]+`), "[REDACTED:webhook]"},
	{"discord-token", regexp.MustCompile(`\b[MNO][A-Za-z0-9_\-]{23,}\.[A-Za-z0-9_\-]{6}\.[A-Za-z0-9_\-]{27,}\b`), "[REDACTED:discord-token]"},
	{"google-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), "[REDACTED:google-key]"},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`), "[REDACTED:jwt]"},
	{"bearer", regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]{16,}`), "$1 [REDACTED:token]"},
	{"url-credentials", regexp.MustCompile(`(://)[^\s/@:]+:[^\s/@]+@`), "$1[REDACTED:credentials]@"},
	// --password foo / --token=foo
	{"cli-secret", regexp.MustCompile(`(?i)(--?[a-z\-]*` + secretKey + `[a-z\-]*)(?:\s+|=)("[^"\n]+"|'[^'\n]+'|[^\s"'-][^\s"']*)`), "$1=[REDACTED:secret]"},
	// KEY=value / "key": "value" / key: value with a secret-ish key name
	{"secret-assignment", regexp.MustCompile(`(?i)(\b[A-Z0-9_.\-]*` + secretKey + `[A-Z0-9_.\-]*["']?\s*[:=]\s*)("[^"\n]{4,}"|'[^'\n]{4,}'|[^\s"',;\[][^\s"',;]{5,})`), "$1[REDACTED:secret]"},
	// .env-looking lines with random-looking values, also as a diff line
	// ("+KEY=...", "-KEY=...", " KEY=...")
	{"env-value", regexp.MustCompile(`(?m)^([+\- ]?\s*(?:export\s+)?[A-Z][A-Z0-9_]{2,}=)([A-Za-z0-9+/_\-]{16,})$`), "$1[REDACTED:env]"},
	{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}\b`), "[email]"},
	{"home-path", regexp.MustCompile(`(?i)(?:/home/|/Users/|[A-Z]:\\Users\\)[^/\\\s"']+`), "~"},
}

// Finding counts redactions of one kind.
type Finding struct {
	Kind  string
	Count int
}

// Scrub redacts s and reports what it removed.
func Scrub(s string) (string, []Finding) {
	if s == "" {
		return s, nil
	}
	counts := map[string]int{}
	for _, r := range rules {
		if r.kind == "email" {
			s = r.re.ReplaceAllStringFunc(s, func(m string) string {
				if strings.HasPrefix(strings.ToLower(m), "git@") || strings.HasPrefix(m, "noreply@") {
					return m
				}
				counts["email"]++
				return "[email]"
			})
			continue
		}
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			if strings.Contains(m, "[REDACTED") && r.kind != "private-key" {
				return m // already redacted by an earlier rule
			}
			counts[r.kind]++
			return r.re.ReplaceAllString(m, r.repl)
		})
	}
	var out []Finding
	for k, n := range counts {
		out = append(out, Finding{k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return s, out
}

// SecretPath reports whether a file's name says it holds secrets (.env*,
// *.pem, *.key, *.p12, *.pfx, SSH keys id_*): its contents are never read
// into a prompt check.
func SecretPath(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	base := strings.ToLower(p[strings.LastIndex(p, "/")+1:])
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return true
	case strings.HasPrefix(base, "id_") && !strings.Contains(base, "."), strings.HasPrefix(base, "id_") && strings.HasSuffix(base, ".pub"):
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// Text is Scrub without the report.
func Text(s string) string { s, _ = Scrub(s); return s }

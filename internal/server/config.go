// Package server implements seatimed: HTTP API, auth, web UI, live updates.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr       string
	DB         string
	BaseURL    string
	Secret     string // cookie signing key
	Dev        bool
	TrustProxy bool // honor X-Forwarded-For (set when behind Caddy)
	// TrustedHops is how many proxies in front of seatimed append to
	// X-Forwarded-For (SEATIME_TRUSTED_HOPS, default 1): the client address
	// is that many entries from the right, so entries a client wrote itself
	// on the left are never used.
	TrustedHops int
	// Debug serves /debug/stats and /debug/pprof (SEATIME_DEBUG=1) on a
	// separate listener at DebugAddr (SEATIME_DEBUG_ADDR, default
	// 127.0.0.1:6060), which must be a loopback address: never on the
	// public port.
	Debug     bool
	DebugAddr string
	DLDir     string // directory of release binaries + SHA256SUMS served at /dl/ ("" = none)
	// DLUpstream: when set, /dl/{file} redirects to DLUpstream/{file} instead
	// of serving DLDir, so release files can live elsewhere (a GitHub
	// release). Clients trust the signed RELEASE manifest, not this host.
	DLUpstream string

	HCAIssuer, HCAClientID, HCASecret, HCAScopes string
	GHClientID, GHSecret                         string

	// Admins are verified login identities, "provider:subject"
	// (SEATIME_ADMIN_IDS, e.g. "hackclub:<sub>", "github:<numeric id>", or
	// "slack:<member id>" as vouched for by Hack Club Auth), never
	// usernames: those are first-come and user-chosen, so matching them
	// would let anyone squat an admin's name.
	Admins map[string]bool

	// HackatimeTrust turns on asking Hackatime for an account's trust colour
	// by its Slack ID (SEATIME_HACKATIME_TRUST=1; off by default because it
	// sends the Slack ID to Hackatime). Off, the Hackatime-shaped API answers
	// "blue". HackatimeURL is Hackatime's base URL
	// (SEATIME_HACKATIME_URL, default https://hackatime.hackclub.com).
	HackatimeTrust bool
	HackatimeURL   string

	// ReviewKeys are the keys of Hack Club's review tools
	// (SEATIME_REVIEW_KEYS, comma-separated, each 32 characters or more;
	// env only, never logged or shown). A request that carries one reads
	// the records of any account's project that is not hidden
	// (reviewapi.go). With none set, that API answers 404.
	ReviewKeys []string

	// PromptCheck is the default of the prompt check's switch (setting
	// prompt_check; SEATIME_PROMPT_CHECK=1; off by default). On, a prompt
	// that is not an exact repeat counts only when the model
	// (pkg/promptcheck) passes it. PromptCheckKey is the OpenRouter key
	// (OPENROUTER_API_KEY): env only, never logged or shown. PromptCheckURL
	// is the decisions API address, or a comma-separated list of the
	// addresses the judge list may use (SEATIME_PROMPT_CHECK_URL; env only,
	// because the key is sent there). PromptCheckModel is the default of the
	// judge list (setting prompt_check_models; SEATIME_PROMPT_CHECK_MODEL).
	PromptCheck      bool
	PromptCheckKey   string
	PromptCheckURL   string
	PromptCheckModel string
	// PromptCheckFile is a JSON file of rules (pkg/promptcheck Rules), the
	// default of setting prompt_check_rules (SEATIME_PROMPT_CHECK_FILE);
	// "" is the compiled default. PromptCheckRules is the same JSON given
	// in the environment itself (SEATIME_PROMPT_CHECK_RULES), for a host
	// that builds from the repository and keeps its rules out of it; it
	// comes before the file.
	PromptCheckFile  string
	PromptCheckRules string

	// CountEvery is how often the server counts the records devices send
	// (SEATIME_COUNT_EVERY, a duration such as 1h or 15m; default 1h): at
	// every multiple of it on the clock, so 1h is each hour on the hour.
	CountEvery time.Duration
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// idSet parses a comma-separated list of "provider:subject" login ids. The
// provider is case-insensitive ("hca" is an alias of "hackclub"); the subject
// is kept exactly, as the provider issued it. Entries without a subject
// are dropped.
func idSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		prov, sub, ok := strings.Cut(strings.TrimSpace(p), ":")
		prov, sub = strings.ToLower(strings.TrimSpace(prov)), strings.TrimSpace(sub)
		if !ok || prov == "" || sub == "" {
			continue
		}
		if prov == "hca" {
			prov = "hackclub"
		}
		if prov == "dev" {
			sub = strings.ToLower(sub) // dev logins are keyed by the lowercased name
		}
		m[prov+":"+sub] = true
	}
	return m
}

func ConfigFromEnv() Config {
	c := Config{
		Addr:        env("SEATIME_ADDR", "127.0.0.1:8790"),
		DB:          env("SEATIME_DB", "seatime.db"),
		BaseURL:     strings.TrimRight(env("SEATIME_BASE_URL", "http://127.0.0.1:8790"), "/"),
		Secret:      os.Getenv("SEATIME_SECRET"),
		Dev:         os.Getenv("SEATIME_DEV") == "1",
		TrustProxy:  os.Getenv("SEATIME_TRUST_PROXY") == "1",
		Debug:       os.Getenv("SEATIME_DEBUG") == "1",
		DebugAddr:   env("SEATIME_DEBUG_ADDR", "127.0.0.1:6060"),
		TrustedHops: 1,
		DLDir:       os.Getenv("SEATIME_DL_DIR"),
		DLUpstream:  strings.TrimRight(os.Getenv("SEATIME_DL_UPSTREAM"), "/"),
		HCAIssuer:   env("SEATIME_HCA_ISSUER", "https://auth.hackclub.com"),
		HCAClientID: os.Getenv("SEATIME_HCA_CLIENT_ID"),
		HCASecret:   os.Getenv("SEATIME_HCA_CLIENT_SECRET"),
		HCAScopes:   env("SEATIME_HCA_SCOPES", "openid profile slack_id"),
		GHClientID:  os.Getenv("SEATIME_GITHUB_CLIENT_ID"),
		GHSecret:    os.Getenv("SEATIME_GITHUB_CLIENT_SECRET"),
		Admins:      idSet(os.Getenv("SEATIME_ADMIN_IDS")),
	}
	c.CountEvery, _ = time.ParseDuration(os.Getenv("SEATIME_COUNT_EVERY"))
	c.HackatimeTrust = os.Getenv("SEATIME_HACKATIME_TRUST") == "1"
	c.HackatimeURL = strings.TrimRight(env("SEATIME_HACKATIME_URL", "https://hackatime.hackclub.com"), "/")
	for _, k := range strings.Split(os.Getenv("SEATIME_REVIEW_KEYS"), ",") {
		if k = strings.TrimSpace(k); len(k) >= 32 {
			c.ReviewKeys = append(c.ReviewKeys, k)
		}
	}
	c.PromptCheck = os.Getenv("SEATIME_PROMPT_CHECK") == "1"
	c.PromptCheckKey = os.Getenv("OPENROUTER_API_KEY")
	c.PromptCheckURL = os.Getenv("SEATIME_PROMPT_CHECK_URL")
	c.PromptCheckModel = os.Getenv("SEATIME_PROMPT_CHECK_MODEL")
	c.PromptCheckFile = os.Getenv("SEATIME_PROMPT_CHECK_FILE")
	c.PromptCheckRules = os.Getenv("SEATIME_PROMPT_CHECK_RULES")
	if n, err := strconv.Atoi(os.Getenv("SEATIME_TRUSTED_HOPS")); err == nil && n >= 1 {
		c.TrustedHops = n
	}
	return c
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

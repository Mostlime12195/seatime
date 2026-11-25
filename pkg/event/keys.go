package event

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// IsPublicForge reports whether a normalized project key or remote
// ("host/owner/repo") is on a well-known public forge; only those keys leave
// the machine in the clear.
func IsPublicForge(remote string) bool {
	h, _, _ := strings.Cut(remote, "/")
	switch h {
	case "github.com", "gitlab.com", "codeberg.org", "bitbucket.org", "git.sr.ht", "sr.ht", "gitea.com":
		return true
	}
	return false
}

// HashedKey is "local:" + 12 hex chars of sha256(key): how a project that is
// not on a public forge is named off this machine.
func HashedKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return "local:" + hex.EncodeToString(h[:6])
}

var hashedRe = regexp.MustCompile(`^local:[0-9a-f]{12}$`)

// ServerKey is the key a project goes by off this machine: a public forge
// path as-is, anything else hashed. It is idempotent.
func ServerKey(key string) string {
	if key == "" || hashedRe.MatchString(key) {
		return key
	}
	if !strings.HasPrefix(key, "local:") && IsPublicForge(key) {
		return key
	}
	return HashedKey(key)
}

// FolderName is the name a project that is not on a public forge goes by
// until the user names it: the last part of its local key, which is the
// folder's name ("local:myapp" -> "myapp") or a private remote's repo
// name. "" for a key that holds no name (one already hashed) and for a
// public forge's.
func FolderName(local string) string {
	if local == "" || ServerKey(local) == local {
		return ""
	}
	n := local[strings.LastIndexAny(local, "/:")+1:]
	if len(n) > 200 { // the server takes no longer name
		return ""
	}
	return n
}

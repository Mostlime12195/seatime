package api

import (
	"time"

	"seatime/pkg/event"
)

// ---- project keys ----

// HashedProjectKey is the key a client sends for a project that is not a
// public forge repo: "local:" + 12 hex chars of sha256(local key).
func HashedProjectKey(localKey string) string { return event.HashedKey(localKey) }

// ProjectKeyMatches reports whether an event's (local) project key belongs to
// a bundle's (server) project key: equal, or hashing to it.
func ProjectKeyMatches(eventKey, bundleKey string) bool {
	if eventKey == "" || bundleKey == "" {
		return false
	}
	return eventKey == bundleKey || HashedProjectKey(eventKey) == bundleKey
}

// ServerCheckpoint is a checkpoint as seatimed received it (server clock).
type ServerCheckpoint struct {
	Seq      int64     `json:"seq"`
	Hash     string    `json:"hash"`
	ServerTS time.Time `json:"server_ts"`
}

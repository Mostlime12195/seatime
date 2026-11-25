package promptcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// The prompt chain. Every prompt a device records, with the check on or
// off, is kept on the device (text, salt and the turn around it) and its
// hash goes to the server at once (POST /api/v1/prompt-hashes). Each
// device's hashes form a chain: entry n's Link hashes entry n-1's Link
// with entry n's own fields. The server keeps every entry and checks each
// new one against the Link it already holds, so once an entry is on the
// server, changing, dropping or reordering it, or its prompt's text,
// cannot go unnoticed.
//
// What an entry commits to:
//
//   - Commit, the prompt hash (Hash): sha256 over a fixed tag, the salt and
//     the text. The salt is 16 random bytes made on the device for that one
//     prompt. It stays on the device, with the text, until the text is sent
//     for its check; it hides short prompts ("yes", "fix it") from a word
//     list. The server does not need it in advance: it checks a text when
//     the text arrives, with the salt that comes with it, and finding a
//     second salt and text with the same sha256 is not possible.
//   - TS, when the prompt was sent (milliseconds), and Project, the
//     project's server key: the check is tied to the time and project the
//     text was counted in.
//   - Repeat, whether the prompt repeats one already sent in the project
//     (it is context for later checks, never judged on its own).
//   - Seq, its place in the device's chain, and Prev, the Link before it.
//
// For a prompt that is not a repeat, Commit is also the record's
// event.Event.CheckHash in the device's log, so the log, the chain and a
// verdict all name the same prompt.

// ChainGenesis is the Link before a device's first entry.
const ChainGenesis = "0000000000000000000000000000000000000000000000000000000000000000"

// ChainEntry is one prompt's place in its device's chain.
type ChainEntry struct {
	Seq     int64     `json:"seq"`
	TS      time.Time `json:"ts"`
	Project string    `json:"project"`
	Commit  string    `json:"commit"`
	Repeat  bool      `json:"repeat,omitempty"`
	Link    string    `json:"link"`
}

// ChainLink is the Link of e after prev: sha256 over a fixed tag and
// each field, separated by zero bytes. TS counts in whole milliseconds.
func ChainLink(prev string, e ChainEntry) string {
	h := sha256.New()
	rep := "0"
	if e.Repeat {
		rep = "1"
	}
	for _, f := range []string{"seatime prompt chain 1", prev, strconv.FormatInt(e.Seq, 10),
		strconv.FormatInt(e.TS.UnixMilli(), 10), e.Project, e.Commit, rep} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrChainBreak: the entries do not continue the chain the server holds.
var ErrChainBreak = errors.New("the prompt hashes do not continue this device's chain")

// VerifyChain checks that es continue a chain whose last entry is seq
// with Link prev: consecutive seqs, each Link recomputed from the one
// before, and fields of the right shape.
func VerifyChain(seq int64, prev string, es []ChainEntry) error {
	for _, e := range es {
		switch {
		case e.Seq != seq+1:
			return fmt.Errorf("%w: entry %d follows %d", ErrChainBreak, e.Seq, seq)
		case !hexHash.MatchString(e.Commit):
			return fmt.Errorf("entry %d: bad commit", e.Seq)
		case e.Project == "" || len(e.Project) > 300:
			return fmt.Errorf("entry %d: bad project", e.Seq)
		case e.TS.IsZero():
			return fmt.Errorf("entry %d: no time", e.Seq)
		case e.Link != ChainLink(prev, e):
			return fmt.Errorf("%w: entry %d has the wrong link", ErrChainBreak, e.Seq)
		}
		seq, prev = e.Seq, e.Link
	}
	return nil
}

// Relink renumbers es to follow the entry seq with Link prev and
// recomputes their Links. A device does this to entries the server has
// not taken yet, when the server holds a different head (the device's
// store was lost or restored from an older copy): the server never saw
// those entries, so nothing it holds changes.
func Relink(seq int64, prev string, es []ChainEntry) []ChainEntry {
	out := make([]ChainEntry, len(es))
	for i, e := range es {
		e.Seq = seq + 1
		e.Link = ChainLink(prev, e)
		out[i] = e
		seq, prev = e.Seq, e.Link
	}
	return out
}

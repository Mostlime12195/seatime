// Package segment builds a signed pkg/api Bundle for one stretch of one
// project (a segment). The daemon uploads it once that stretch of work is
// over (Next), but only to a server that does not take records as they
// are written (package records); `seatime inspect --segment` prints one.
// The server checks it against the hash chain: its records
// and the idle-timer records in the range are revealed in their private form
// (chain.Hide: no prompt text, command, file path, folder or extras; the
// project by its server key), every other record is a bare leaf hash, and
// the whole segment replays against the hash chain. Editor heartbeats on a
// file inside the project's git repo carry that file's repo-relative path
// beside the record (RepoPath), as the editor plugin already sends it to
// Hackatime; nothing outside the repo and no absolute path is ever named.
package segment

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/project"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

type Options struct {
	Project string // resolved local project key
	Since   time.Time
	Until   time.Time
	MaxSeq  int64 // only include events up to this seq (the last synced checkpoint); 0 = no limit
}

type Summary struct {
	Project  string
	From, To time.Time
	FirstSeq int64
	LastSeq  int64
	Revealed int
	Hidden   int
	ByKind   map[event.Kind]int
	// Withheld counts the project's records written before private records
	// (they commit to paths and folder names in the clear, so they go as
	// bare leaf hashes and their time is not counted).
	Withheld int
	// Named counts the editor heartbeats sent with their file's path in the repo.
	Named    int
	Claimed  ledger.Bucket
	ChainOK  bool
	ChainErr string
}

type Prepared struct {
	Bundle  api.Bundle
	Summary Summary
}

// ProjectRoot returns the local checkout of a project key ("" if unknown).
func ProjectRoot(st *store.Store, key string) string {
	ps, err := st.Projects(time.Time{})
	if err != nil {
		return ""
	}
	for _, p := range ps {
		if p.Key == key {
			return p.Root
		}
	}
	return ""
}

// RepoRoot is the project's checkout when it is the top of a git repo, else "".
func RepoRoot(st *store.Store, key string) string {
	root := ProjectRoot(st, key)
	if root == "" || !filepath.IsAbs(root) {
		return ""
	}
	if top, _, ok := project.FindGitRoot(root); !ok || filepath.Clean(top) != filepath.Clean(root) {
		return ""
	}
	return root
}

// RepoPath is the path, inside the git repo at repo, of the file an editor
// heartbeat touched, or "" when there is none to give: not an editor
// heartbeat, no repo, a file outside it (the local log keeps those as ~/... or
// absolute paths), an absolute or escaping path, or a file that is not in the
// checkout now. The local log already holds in-repo files relative to the
// project's root (package emit).
func RepoPath(e event.Event, repo string) string {
	if repo == "" || e.Kind != event.Edit || !strings.HasPrefix(e.Harness, "editor:") || len(e.Files) != 1 {
		return ""
	}
	p := e.Files[0]
	if p == "" || len(p) > 400 || chain.IsCommitted(p) || strings.ContainsAny(p, "\\\x00\n\r") ||
		strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" ||
		path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	if fi, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(p))); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

// ResolveProject maps user input (key, name or unique substring) to a key.
func ResolveProject(st *store.Store, q string) (string, error) {
	keys, err := st.ProjectKeys()
	if err != nil {
		return "", err
	}
	names := map[string]string{}
	if ps, err := st.Projects(time.Time{}); err == nil {
		for _, p := range ps {
			names[p.Key] = p.Name
			if _, ok := keys[p.Key]; !ok {
				keys[p.Key] = 0
			}
		}
	}
	if _, ok := keys[q]; ok {
		return q, nil
	}
	var exact, sub []string
	lq := strings.ToLower(q)
	for k := range keys {
		n := names[k]
		if strings.EqualFold(n, q) || strings.EqualFold(strings.TrimPrefix(k, "local:"), q) {
			exact = append(exact, k)
		} else if strings.Contains(strings.ToLower(k), lq) {
			sub = append(sub, k)
		}
	}
	sort.Strings(exact)
	sort.Strings(sub)
	pick := exact
	if len(pick) == 0 {
		pick = sub
	}
	switch len(pick) {
	case 0:
		return "", fmt.Errorf("no project matches %q (see `seatime projects`)", q)
	case 1:
		return pick[0], nil
	}
	return "", fmt.Errorf("%q is ambiguous: %s", q, strings.Join(pick, ", "))
}

// Build assembles and signs a bundle. It never mutates the store.
func Build(st *store.Store, auth config.Auth, o Options) (*Prepared, error) {
	q := store.Query{Project: o.Project, Since: o.Since, Until: o.Until, UpToSeq: o.MaxSeq}
	min, max, n, err := st.SeqRange(q)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("no events for %s in that range", o.Project)
	}
	// The server only trusts a range that ends at a checkpoint it received.
	// The project's last event usually isn't one (other projects' events
	// follow it before the next sync), so run on, as bare leaf hashes, to the
	// last synced seq, which is.
	if o.MaxSeq > max {
		max = o.MaxSeq
	}
	rows, err := st.Rows(min, max)
	if err != nil {
		return nil, err
	}
	start, err := st.HashAt(min - 1)
	if err != nil {
		return nil, err
	}
	sum := Summary{Project: o.Project, FirstSeq: min, LastSeq: max, ByKind: map[event.Kind]int{}}
	repo := RepoRoot(st, o.Project)
	var entries []chain.Entry
	for _, r := range rows {
		var e event.Event
		mine := r.Project == o.Project && inRange(r, o)
		if mine || r.Project == "" {
			if err := json.Unmarshal(r.Data, &e); err != nil {
				mine = false // a damaged record: leaf only
			} else if r.Project == "" {
				// idle-timer records: when there was input, shown with the
				// project's records
				mine = e.Kind == event.Idle && inRange(r, o)
			}
		}
		if !mine {
			entries = append(entries, chain.Entry{Seq: r.Seq, LeafHash: r.Leaf})
			sum.Hidden++
			continue
		}
		if e.Canon < event.CanonKeyed {
			// an older record: its leaf commits to file paths, the folder
			// and the local project name in the clear (form 0), or to text
			// by a plain SHA-256 a word list can reverse (form 2), so it
			// cannot be revealed safely
			entries = append(entries, chain.Entry{Seq: r.Seq, LeafHash: r.Leaf})
			sum.Hidden++
			if e.Kind != event.Idle {
				sum.Withheld++
			}
			continue
		}
		// the private form: no prompt text, no command, no file path, no
		// folder, no extras, the project by its server key
		named := RepoPath(e, repo)
		if e, err = st.Hide(e); err != nil {
			return nil, err
		}
		entries = append(entries, chain.Entry{Seq: r.Seq, Event: &e, Path: named})
		if named != "" {
			sum.Named++
		}
		sum.Revealed++
		sum.ByKind[e.Kind]++
		if e.Kind == event.Idle {
			continue
		}
		if sum.From.IsZero() || e.TS.Before(sum.From) {
			sum.From = e.TS
		}
		if e.TS.After(sum.To) {
			sum.To = e.TS
		}
	}
	key := syncer.ServerKey(o.Project)
	set := stats.PromptChecks(st) // nil when the prompt check was never on
	b := api.Bundle{DeviceID: auth.DeviceID, Project: key, StartHash: start, Entries: entries}
	b.ScoreVersion = set.Version(promptcheck.Span(audit.Revealed(b)))
	if set.AnyOn(sum.From, sum.To) {
		b.Others, b.OthersUntil, err = OthersCap(st, set, o.Project, sum.From, sum.To)
	} else {
		b.Others, err = Others(st, o.Project, sum.From, sum.To)
	}
	if err != nil {
		return nil, err
	}
	// the claim is the shared analysis over exactly what the server will see
	revealed := audit.Revealed(b)
	revealed, _ = set.Apply(revealed) // as the server counts it (pkg/promptcheck); a nil set leaves them
	b.Claimed = audit.ClaimWith(set, revealed, key, b.Others, b.OthersUntil)
	sum.Claimed = b.Claimed

	headHash := rows[len(rows)-1].Hash
	_, verr := chain.Verify(start, entries, []chain.Checkpoint{{Seq: max, Hash: headHash}})
	sum.ChainOK = verr == nil
	if verr != nil {
		sum.ChainErr = verr.Error()
	}
	if err := syncer.SignBundle(auth, &b); err != nil {
		return nil, err
	}
	return &Prepared{Bundle: b, Summary: sum}, nil
}

func inRange(r store.Row, o Options) bool {
	if o.Since.IsZero() && o.Until.IsZero() {
		return true
	}
	var e struct {
		TS time.Time `json:"ts"`
	}
	if json.Unmarshal(r.Data, &e) != nil {
		return false
	}
	return (o.Since.IsZero() || !e.TS.Before(o.Since)) && (o.Until.IsZero() || e.TS.Before(o.Until))
}

// Hours are the claimed hand hours, AI hours and AI hours not counted.
func (s Summary) Hours() (hand, ai, notCounted float64) {
	return s.Claimed.CodingSeconds / 3600, s.Claimed.AISeconds / 3600, s.Claimed.AINotCountedSeconds / 3600
}

// Others are the bare times of counted heartbeats the segment does not
// reveal (audit.Others), from this device's log.
func Others(st *store.Store, project string, from, to time.Time) ([]time.Time, error) {
	if from.IsZero() {
		return nil, nil
	}
	evs, err := stats.Events(st, from, to.Add(time.Millisecond))
	if err != nil {
		return nil, err
	}
	return audit.Others(evs, project, from, to), nil
}

// OthersCap is Others under the cap rule (audit.OthersCap).
func OthersCap(st *store.Store, set *promptcheck.Set, project string, from, to time.Time) (times, until []time.Time, err error) {
	if from.IsZero() {
		return nil, nil, nil
	}
	evs, err := stats.Events(st, from, to.Add(time.Millisecond))
	if err != nil {
		return nil, nil, err
	}
	times, until = audit.OthersCap(evs, set, project, from, to)
	return times, until, nil
}

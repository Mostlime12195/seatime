package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

func promptText(i int) string {
	return fmt.Sprintf("why does the parser %d loop forever on empty input and how should I test it", i)
}

// localChain builds a live-looking session of a project that is not a
// public repo: prompts (text committed, never plaintext), agent turns, a
// failing then passing test, a commit. All in the last few minutes.
func localChain(t *testing.T, project string) ([]event.Event, []string) {
	t.Helper()
	base := time.Now().UTC().Add(-9 * time.Minute).Truncate(time.Millisecond)
	prev := chain.Genesis
	var evs []event.Event
	var heads []string
	seq := int64(0)
	add := func(e event.Event) {
		seq++
		e.Seq, e.Project, e.Harness, e.SessionID = seq, project, "claude-code", "s1"
		var err error
		if prev, err = chain.Next(prev, e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
		heads = append(heads, prev)
	}
	fail, pass := 1, 0
	for i := 0; i < 8; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		add(event.Event{TS: at, Kind: event.Prompt, Text: chain.Commit(promptText(i)), Words: 15, Chars: 80, Novelty: 0.7,
			TextHash: fmt.Sprintf("%064x", 1000+i)})
		add(event.Event{TS: at.Add(time.Second), Kind: event.AgentStart})
		ec := pass
		if i == 3 {
			ec = fail
		}
		add(event.Event{TS: at.Add(20 * time.Second), Kind: event.Run, RunKind: event.RunTest, ExitCode: &ec})
		add(event.Event{TS: at.Add(30 * time.Second), Kind: event.AgentStop})
	}
	add(event.Event{TS: base.Add(8*time.Minute + 10*time.Second), Kind: event.Commit, SHA: strings.Repeat("c", 40), Insertions: 40})
	return evs, heads
}

func (d *device) localBundle(evs []event.Event, project string) api.Bundle {
	var entries []chain.Entry
	for i := range evs {
		e := evs[i]
		entries = append(entries, chain.Entry{Seq: e.Seq, Event: &e})
	}
	b := api.Bundle{DeviceID: d.id, Project: api.HashedProjectKey(project), StartHash: chain.Genesis, Entries: entries,
		ScoreVersion: ledger.Version}
	b.Claimed = audit.Claim(audit.Revealed(b), b.Project)
	d.resign(&b)
	return b
}

func apiJSON[T any](t *testing.T, e *tenv, path, token string) (int, T) {
	t.Helper()
	code, out := e.apiDo("GET", path, token, nil)
	var v T
	if code == 200 {
		if err := json.Unmarshal(out, &v); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, out)
		}
	}
	return code, v
}

// Regression: a project that is not a public repo is submitted under its
// hashed key ("local:<sha256 prefix>") while its events keep "local:<name>";
// the server must count it under that key.
func TestHashedProjectKeyIsCounted(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("lena")
	evs, heads := localChain(t, "local:myapp")
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: heads[len(heads)-1]}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, d.localBundle(evs, "local:myapp"))
	if code != 200 {
		t.Fatal(code, string(out))
	}
	var resp api.SegmentResp
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != api.StatusVerified || resp.Scored.Seconds() < 60 {
		t.Fatalf("status %s, credited %.0f s, problems %v", resp.Status, resp.Scored.Seconds(), resp.Problems)
	}
}

// An honest project reads clean on its public page: the checked hours,
// verified, nothing of the prompts.
func TestProjectPageReadsClean(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("lena")
	sendLocal(t, e, d, "local:myapp")
	code, page := e.browser().get("/@lena/" + api.HashedProjectKey("local:myapp"))
	if code != 200 {
		t.Fatal(code)
	}
	for _, want := range []string{"Checked hours", "verified"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, "failed") {
		t.Error("an honest project shows failed")
	}
	if strings.Contains(page, "parser") {
		t.Error("prompt text on the project page")
	}
}

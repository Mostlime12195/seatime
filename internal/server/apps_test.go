package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// offStretch is a device that worked on github.com/<user>/zorblax while the
// account had the prompt check off: one prompt, chained and hashed, and
// hand edits after it, synced live and sent as a checked segment.
type offStretch struct {
	d      *device
	br     *browser
	entry  promptcheck.ChainEntry
	salt   string
	text   string
	key    string
	before api.SegmentResp
}

func newOffStretch(t *testing.T, e *tenv, user string) *offStretch {
	t.Helper()
	d, br := e.enroll(user)
	setCheck(t, br, false)
	time.Sleep(5 * time.Millisecond)
	o := &offStretch{d: d, br: br, key: "github.com/" + user + "/zorblax", text: "make the parser keep comments so the formatter can put them back"}
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	es, salts := chained(0, promptcheck.ChainGenesis, o.key, t0, o.text)
	o.entry, o.salt = es[0], salts[0]
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	// the records: the prompt (with its check hash) and three hand edits
	prev := chain.Genesis
	var evs []event.Event
	add := func(e event.Event) {
		e.Seq, e.Project = int64(len(evs)+1), o.key
		var err error
		if prev, err = chain.Next(prev, e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	yes := true
	add(event.Event{TS: o.entry.TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(o.text), Words: 12, New: &yes, CheckHash: o.entry.Commit})
	for i := 1; i <= 3; i++ {
		add(event.Event{TS: t0.Add(time.Duration(i) * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 10 + i})
	}
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: prev}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	var entries []chain.Entry
	for i := range evs {
		entries = append(entries, chain.Entry{Seq: evs[i].Seq, Event: &evs[i]})
	}
	b := api.Bundle{DeviceID: d.id, Project: o.key, StartHash: chain.Genesis, Entries: entries, ScoreVersion: ledger.Version,
		Claimed: audit.Claim(evs, o.key)}
	d.resign(&b)
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, b)
	json.Unmarshal(out, &o.before)
	if code != 200 || o.before.Status != api.StatusVerified {
		t.Fatalf("segment: %d %s", code, out)
	}
	return o
}

func (o *offStretch) stats(t *testing.T, e *tenv, token string) api.ProjectStats {
	t.Helper()
	code, out := e.apiDo("GET", "/api/v1/users/"+o.d.user+"/projects/zorblax/stats", token, nil)
	var st api.ProjectStats
	if code != 200 || json.Unmarshal(out, &st) != nil {
		t.Fatalf("stats: %d %s", code, out)
	}
	return st
}

// A stretch worked with the check off, then a application reads the project:
// the server asks the device for the saved prompts, the project is pending
// until they are checked, then verified. Reading it without a application's
// token (the public) asks for nothing.
func TestAppReadChecksOffStretch(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	o := newOffStretch(t, e, "ada")
	if st := o.stats(t, e, ""); st.Status != api.StatusVerified || st.HumanSeconds == 0 {
		t.Fatalf("before any application: %+v", st)
	}
	dev := e.browser()
	dev.login("devon", "user")
	id, secret := registerApp(t, e, dev)
	// the consent screen shows what the application would read
	_, ch := pkcePair()
	if _, page := o.br.get("/oauth/authorize?" + authQuery(id, ch).Encode()); !strings.Contains(page, "zorblax") || !strings.Contains(page, "<td>verified</td>") {
		t.Fatalf("consent preview: %s", page)
	}
	if r := syncResp(t, o.d, 10); len(r.Pulls) != 0 {
		t.Fatalf("a public read or the consent screen asked for prompts: %+v", r.Pulls)
	}
	access, _ := connect(t, e, o.br, id, secret)
	st := o.stats(t, e, access)
	if st.Status != api.StatusPending || !strings.Contains(st.StatusReason, "asked the computer for prompts") {
		t.Fatalf("after the first application read: %+v", st)
	}
	r := syncResp(t, o.d, 11)
	if len(r.Pulls) == 0 {
		t.Fatal("the device was not asked for its prompts")
	}
	// the device sends the saved prompt; it passes
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", o.d.token, chainedReq(o.d, o.entry, o.salt, o.text)); code != 200 {
		t.Fatalf("pulled check: %d %s", code, out)
	}
	// once the pull settles (it stays open an hour), the project is checked again
	e.srv.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	access, _ = connect(t, e, o.br, id, secret) // the first token has expired by now
	st = o.stats(t, e, access)
	if st.Status != api.StatusVerified || st.HumanSeconds == 0 {
		t.Fatalf("after the check came back: %+v", st)
	}
	// a second application read asks for nothing more
	if r := syncResp(t, o.d, 12); len(r.Pulls) != 0 {
		t.Fatalf("asked again: %+v", r.Pulls)
	}
}

// The same, but the device never sends the prompt: after 14 days it counts
// as missing and the project fails, with the reason.
func TestAppReadMissingPromptFails(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	o := newOffStretch(t, e, "bea")
	dev := e.browser()
	dev.login("devon", "user")
	id, secret := registerApp(t, e, dev)
	access, _ := connect(t, e, o.br, id, secret)
	if st := o.stats(t, e, access); st.Status != api.StatusPending {
		t.Fatalf("after the first application read: %+v", st)
	}
	e.srv.now = func() time.Time { return time.Now().Add(15 * 24 * time.Hour) }
	st := o.stats(t, e, "") // anyone reading it sees the outcome
	if st.Status != api.StatusFailed || !strings.Contains(st.StatusReason, "never came") {
		t.Fatalf("after the deadline: %+v", st)
	}
}

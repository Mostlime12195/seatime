package server

import (
	"context"
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

// With the prompt check on, a device is not told which prompts count. It
// syncs hand time; whatever AI time it sends is dropped. The AI time of a
// stretch of work shows on the site once the server has checked its
// records, and no later sync of the device changes it.
func TestCheckedMinutesShowOnTheSite(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	d, br := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	text := "make the parser keep comments so the formatter can put them back"
	t0 := time.Now().UTC().Truncate(time.Minute)

	// the prompt's hash, then its check: the answer names nothing
	es, salts := chained(0, promptcheck.ChainGenesis, key, t0, text)
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[0], salts[0], text)); code != 200 || strings.Contains(string(out), "pass") || strings.Contains(string(out), "minutes") {
		t.Fatalf("check: %d %s", code, out)
	}

	// the records: the prompt, the agent stopping after 4 minutes, a hand edit
	prev := chain.Genesis
	var evs []event.Event
	add := func(ev event.Event) {
		ev.Seq, ev.Project = int64(len(evs)+1), key
		var err error
		if prev, err = chain.Next(prev, ev); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	no := false
	add(event.Event{TS: es[0].TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(text), Words: 12, New: &no, CheckHash: es[0].Commit})
	add(event.Event{TS: t0.Add(4 * time.Minute), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s"})
	add(event.Event{TS: t0.Add(8 * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	add(event.Event{TS: t0.Add(9 * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 4})

	e.srv.now = func() time.Time { return t0.Add(10 * time.Minute) }
	// the live sync: a minute of hand time, and AI time the device has no
	// way to know (a changed client, or one from before)
	live := []ledger.Bucket{
		{Project: key, Start: t0, AISeconds: 55, AINotCountedSeconds: 5, Prompts: 1},
		{Project: key, Start: t0.Add(8 * time.Minute), CodingSeconds: 60, Edits: 2},
	}
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: prev}, live, nil); code != 200 {
		t.Fatal(code, out)
	}
	u, _ := e.st.UserByName(ctx, "cyd")
	p, err := e.st.ProjectByKey(ctx, u.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(bs []ledger.Bucket) (hand, ai float64, prompts int) {
		for _, b := range bs {
			hand, ai, prompts = hand+b.CodingSeconds, ai+b.AISeconds+b.AINotCountedSeconds, prompts+b.Prompts
		}
		return
	}
	bs, _ := e.st.Buckets(ctx, u.ID, p.ID, t0.Add(-time.Hour), t0.Add(time.Hour))
	if hand, ai, prompts := sum(bs); hand != 60 || ai != 0 || prompts != 1 {
		t.Fatalf("live: hand %v, AI %v, %d prompts", hand, ai, prompts)
	}
	if _, body := br.get("/@cyd"); !strings.Contains(body, "1m &#43; 1 prompt unchecked") {
		t.Fatal("the profile does not say what is unchecked")
	}

	// the stretch's records arrive: the server counts the prompt itself
	var entries []chain.Entry
	for i := range evs {
		entries = append(entries, chain.Entry{Seq: evs[i].Seq, Event: &evs[i]})
	}
	b := api.Bundle{DeviceID: d.id, Project: key, StartHash: chain.Genesis, Entries: entries, ScoreVersion: ledger.CapVersion,
		Claimed: audit.Claim(evs, key)}
	d.resign(&b)
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, b)
	var seg api.SegmentResp
	json.Unmarshal(out, &seg)
	if code != 200 || seg.Status != api.StatusVerified || seg.Scored.AISeconds < 4*60 {
		t.Fatalf("segment: %d %s", code, out)
	}
	bs, _ = e.st.Buckets(ctx, u.ID, p.ID, t0.Add(-time.Hour), t0.Add(time.Hour))
	hand, ai, prompts := sum(bs)
	if ai < 4*60 || prompts != 1 || hand < 60 {
		t.Fatalf("after the check: hand %v, AI %v, %d prompts", hand, ai, prompts)
	}
	if synced, _ := e.st.SyncedBuckets(ctx, u.ID, p.ID, t0.Add(-time.Hour), t0.Add(time.Hour)); len(synced) != 2 {
		t.Fatalf("the device's own minutes: %+v", synced)
	} else if _, sai, _ := sum(synced); sai != 0 {
		t.Fatalf("the device's minutes carry AI time: %v", sai)
	}
	if _, body := br.get("/@cyd"); strings.Contains(body, "unchecked") {
		t.Fatal("checked time is still called unchecked")
	}

	// the device syncs the window again, hand time only: the checked AI
	// time stays
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)) + 1, Hash: h(99)}, live[1:], func(r *api.SyncReq) {
		r.From, r.To = t0.Add(-time.Hour), t0.Add(time.Hour)
	}); code != 200 {
		t.Fatal(code, out)
	}
	bs, _ = e.st.Buckets(ctx, u.ID, p.ID, t0.Add(-time.Hour), t0.Add(time.Hour))
	if _, again, _ := sum(bs); again != ai {
		t.Fatalf("a sync changed the checked AI time: %v, was %v", again, ai)
	}
	// counting the segment again writes the same minutes
	e.srv.showAllChecked(ctx)
	bs, _ = e.st.Buckets(ctx, u.ID, p.ID, t0.Add(-time.Hour), t0.Add(time.Hour))
	if _, again, _ := sum(bs); again != ai {
		t.Fatalf("showing the checked minutes again changed them: %v, was %v", again, ai)
	}
}

// The device sends the project's earlier prompts without knowing which
// passed; the judge is shown only those the server passed.
func TestEarlierPromptsAreTheServersPick(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	d, _ := e.enroll("dee")
	d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	good := checkReq(d, "EARLIER-GOOD add a pause key to the game")
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, good); code != 200 {
		t.Fatal(code, string(out))
	}
	j.p = map[string]float64{} // nothing passes
	for k := range passing() {
		j.p[k] = 0.1
	}
	bad := checkReq(d, "EARLIER-BAD ok")
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, bad); code != 200 {
		t.Fatal(code, string(out))
	}
	never := checkReq(d, "EARLIER-UNKNOWN never checked here")
	j.p = passing()
	next := checkReq(d, "now make the pause key configurable")
	for _, r := range []api.PromptCheckReq{good, bad, never} {
		next.Earlier = append(next.Earlier, promptcheck.Turn{Prompt: r.Turn.Prompt, CheckHash: r.CheckHash, Salt: r.Salt})
	}
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, next); code != 200 {
		t.Fatal(code, string(out))
	}
	st := j.states[len(j.states)-1]
	if !strings.Contains(st, "EARLIER-GOOD") || strings.Contains(st, "EARLIER-BAD") || strings.Contains(st, "EARLIER-UNKNOWN") {
		t.Fatalf("the judge was shown:\n%s", st)
	}
}

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// recChain is a device's chain in a test: records in their private form.
type recChain struct {
	t    *testing.T
	d    *device
	key  string
	evs  []event.Event
	prev []string // the chain head before each record
	head string
	sent int64 // records the device has sent
}

func newRecChain(t *testing.T, d *device, key string) *recChain {
	return &recChain{t: t, d: d, key: key, head: chain.Genesis}
}

// add appends a record of the chain's project (or of none: an idle record).
func (c *recChain) add(ev event.Event) {
	c.t.Helper()
	ev.Seq = int64(len(c.evs) + 1)
	if ev.Canon == 0 {
		ev.Canon = event.CanonKeyed
	}
	if ev.Kind != event.Idle && ev.Project == "" {
		ev.Project = c.key
	}
	if ev.Project == "-" {
		ev.Project = "" // a record of no project
	}
	c.prev = append(c.prev, c.head)
	var err error
	if c.head, err = chain.Next(c.head, ev); err != nil {
		c.t.Fatal(err)
	}
	c.evs = append(c.evs, ev)
}

// sync sends the checkpoint at the chain's head.
func (c *recChain) sync() {
	c.t.Helper()
	if code, out := c.d.sync(chain.Checkpoint{Seq: int64(len(c.evs)), Hash: c.head}, nil, nil); code != 200 {
		c.t.Fatal(code, out)
	}
}

func (c *recChain) req(lo, hi int64) api.RecordsReq {
	r := api.RecordsReq{DeviceID: c.d.id, StartHash: c.prev[lo-1]}
	for i := lo; i <= hi; i++ {
		r.Entries = append(r.Entries, chain.Entry{Seq: i, Event: &c.evs[i-1]})
	}
	return r
}

// push syncs and sends every record not sent yet, as the daemon does.
func (c *recChain) push() api.RecordsResp {
	c.t.Helper()
	c.sync()
	code, out := c.d.e.apiDo("POST", "/api/v1/records", c.d.token, c.req(c.sent+1, int64(len(c.evs))))
	var r api.RecordsResp
	if json.Unmarshal(out, &r); code != 200 || r.Head != int64(len(c.evs)) {
		c.t.Fatalf("records: %d %s", code, out)
	}
	c.sent = r.Head
	return r
}

// another enrolls a second device for the account br is signed in to.
func (e *tenv) another(br *browser, name string) *device {
	e.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	code, out := e.apiDo("POST", "/api/v1/device/start", "", api.DeviceStartReq{DeviceName: name, OS: "linux", PublicKey: base64.StdEncoding.EncodeToString(pub)})
	if code != 200 {
		e.t.Fatalf("start: %d %s", code, out)
	}
	var st api.DeviceStartResp
	json.Unmarshal(out, &st)
	tok := br.csrf("/device?code=" + st.UserCode)
	if c, body := br.post("/device/approve", url.Values{"csrf": {tok}, "code": {st.UserCode}}); c != 200 || !strings.Contains(body, "Approved") {
		e.t.Fatalf("approve: %d %s", c, body)
	}
	code, out = e.apiDo("POST", "/api/v1/device/poll", "", api.DevicePollReq{DeviceCode: st.DeviceCode})
	var pr api.DevicePollResp
	if json.Unmarshal(out, &pr); code != 200 || pr.Token == "" {
		e.t.Fatalf("poll: %d %s", code, out)
	}
	return &device{e, pub, priv, pr.Token, pr.DeviceID, pr.Username}
}

func (e *tenv) count() {
	e.t.Helper()
	if _, err := e.srv.CountStreams(context.Background(), nil); err != nil {
		e.t.Fatal(err)
	}
}

func aiSeconds(bs []ledger.Bucket) (ai float64) {
	for _, b := range bs {
		ai += b.AISeconds
	}
	return ai
}

// A device sends its records as it writes them, and nothing is checked
// until the server's next cycle. Then the stretch so far is counted, AI
// time and all; each later cycle counts the same stretch again with what
// came since; and once the stretch is over it is closed, and later work
// starts a new one.
func TestRecordsAreCountedAtTheCycle(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour) // ahead of the real clock, as the simulator runs
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, br := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	text := "make the parser keep comments so the formatter can put them back"

	es, salts := chained(0, promptcheck.ChainGenesis, key, t0, text)
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[0], salts[0], text)); code != 200 {
		t.Fatalf("check: %d %s", code, out)
	}
	c := newRecChain(t, d, key)
	no := false
	c.add(event.Event{TS: es[0].TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(text), Words: 12, New: &no, CheckHash: es[0].Commit})
	c.add(event.Event{TS: t0.Add(4 * time.Minute), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s"})
	now = t0.Add(5 * time.Minute)
	c.push()

	u, _ := e.st.UserByName(ctx, "cyd")
	segments := func() []store.Segment {
		t.Helper()
		sgs, err := e.st.ListSegments(ctx, u.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		return sgs
	}
	// the records are there; nothing is counted before the cycle
	if sgs := segments(); len(sgs) != 0 {
		t.Fatalf("counted before the cycle: %d segments", len(sgs))
	}

	// the first cycle: one open stretch, with the prompt's AI time
	now = t0.Add(time.Hour)
	e.count()
	p, err := e.st.ProjectByKey(ctx, u.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	shown := func() float64 {
		bs, _ := e.st.Buckets(ctx, u.ID, p.ID, t0.Add(-time.Hour), t0.Add(24*time.Hour))
		return aiSeconds(bs)
	}
	sgs := segments()
	if len(sgs) != 1 || !sgs[0].Stream || !sgs[0].Open || sgs[0].Status != store.SegVerified || sgs[0].RecKey != key {
		t.Fatalf("after the first cycle: %+v", sgs)
	}
	first := sgs[0]
	if first.Scored.AISeconds < 4*60-1 || shown() < 4*60-1 {
		t.Fatalf("AI time: counted %v, shown %v", first.Scored.AISeconds, shown())
	}
	if _, body := br.get("/@cyd"); strings.Contains(body, "unchecked") {
		t.Fatal("checked time is still called unchecked")
	}
	_, st := apiJSON[api.ProjectStats](t, e, "/api/v1/users/cyd/projects/zorblax/stats", "")
	if st.HumanSeconds < 4*60-1 {
		t.Fatalf("an application reads %v s", st.HumanSeconds)
	}

	// hand edits come; they are in no count until the next cycle, which
	// counts the same stretch again
	c.add(event.Event{TS: t0.Add(70 * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	c.add(event.Event{TS: t0.Add(71 * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 4})
	now = t0.Add(72 * time.Minute)
	c.push()
	if sgs = segments(); len(sgs) != 1 || sgs[0].Scored.Seconds() != first.Scored.Seconds() {
		t.Fatalf("counted between cycles: %+v", sgs)
	}
	now = t0.Add(2 * time.Hour)
	e.count()
	sgs = segments()
	if len(sgs) != 1 || sgs[0].ID != first.ID || !sgs[0].Open || sgs[0].Scored.CodingSeconds < 60 || sgs[0].Scored.AISeconds < 4*60-1 {
		t.Fatalf("after the second cycle: %+v", sgs)
	}
	if !sgs[0].Created.Equal(first.Created) || sgs[0].SeqHi != 4 {
		t.Fatalf("the stretch was not counted in place: %+v", sgs[0])
	}

	// the stretch is over: it is closed where a device would cut it
	now = t0.Add(71*time.Minute + audit.ReadyAfter + time.Minute).Truncate(time.Hour).Add(time.Hour)
	e.count()
	sgs = segments()
	if len(sgs) != 1 || sgs[0].Open || sgs[0].Until.IsZero() || sgs[0].Status != store.SegVerified {
		t.Fatalf("after the stretch was over: %+v", sgs)
	}
	closed := sgs[0]
	if want := t0.Add(71 * time.Minute).Add(audit.SplitPad).Truncate(time.Minute).Add(time.Minute); !closed.Until.Equal(want) {
		t.Fatalf("closed at %s, want %s", closed.Until, want)
	}
	// a cycle with nothing new counts nothing
	if n, err := e.srv.CountStreams(ctx, nil); err != nil || n != 0 {
		t.Fatalf("a cycle with nothing new counted %d devices (%v)", n, err)
	}

	// work after it is a new stretch, from where the last one ended
	later := closed.Until.Add(2 * time.Hour)
	c.add(event.Event{TS: later, Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	c.add(event.Event{TS: later.Add(time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	now = later.Add(2 * time.Minute)
	c.push()
	now = later.Add(10 * time.Minute)
	e.count()
	sgs = segments()
	if len(sgs) != 2 || sgs[0].ID != closed.ID || sgs[0].Scored.Seconds() != closed.Scored.Seconds() {
		t.Fatalf("the closed stretch changed: %+v", sgs)
	}
	if nw := sgs[1]; !nw.Open || !nw.Since.Equal(closed.Until) || nw.Scored.CodingSeconds < 60 || nw.SeqLo != 5 {
		t.Fatalf("the new stretch: %+v", nw)
	}
	// reviewers read every record once, from the stretches
	ev, err := e.srv.reviewEvidence(ctx, u, p, t0.Add(-time.Hour), later.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sg := range ev.Segments {
		n += len(sg.Records)
	}
	if n != len(c.evs) {
		t.Fatalf("reviewers read %d records, the device sent %d", n, len(c.evs))
	}
}

// A prompt still being checked does not hold a stretch back: what is
// checked counts, and the prompt's time is added at the first cycle after
// its check, not before.
func TestPromptCheckedLaterCountsAtTheNextCycle(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour) // ahead of the real clock, as the simulator runs
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	text := "make the parser keep comments so the formatter can put them back"
	es, salts := chained(0, promptcheck.ChainGenesis, key, t0.Add(10*time.Minute), text)
	now = t0.Add(10 * time.Minute)
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	c := newRecChain(t, d, key)
	no := false
	c.add(event.Event{TS: t0, Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	c.add(event.Event{TS: t0.Add(time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	c.add(event.Event{TS: es[0].TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(text), Words: 12, New: &no, CheckHash: es[0].Commit})
	now = t0.Add(11 * time.Minute)
	c.push()
	c.add(event.Event{TS: t0.Add(14 * time.Minute), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s"})
	now = t0.Add(15 * time.Minute)
	c.push()
	now = t0.Add(time.Hour)
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	p, _ := e.st.ProjectByKey(ctx, u.ID, key)
	sgs, _ := e.st.ListSegments(ctx, u.ID, p.ID)
	if len(sgs) != 1 || sgs[0].Status != store.SegVerified || sgs[0].Pending != 1 || sgs[0].Scored.CodingSeconds < 60 || sgs[0].Scored.AISeconds != 0 {
		t.Fatalf("with the prompt still being checked: %+v", sgs)
	}
	_, st := apiJSON[api.ProjectStats](t, e, "/api/v1/users/cyd/projects/zorblax/stats", "")
	if st.HumanSeconds < 60 {
		t.Fatalf("an application reads %v s of the open stretch", st.HumanSeconds)
	}
	// a reviewer sees that the prompt waits for its check
	waits := func() (n int) {
		ev, err := e.srv.reviewEvidence(ctx, u, p, t0.Add(-time.Hour), t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, sg := range ev.Segments {
			if !sg.Open {
				t.Errorf("the stretch is not shown as open: %+v", sg)
			}
			for _, r := range sg.Records {
				if r.Check != nil && r.Check.Waiting {
					n++
				}
			}
		}
		return n
	}
	if n := waits(); n != 1 {
		t.Fatalf("%d prompts shown as waiting, want 1", n)
	}
	// the check passes only after that cycle (the turn was still running)
	now = t0.Add(62 * time.Minute)
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[0], salts[0], text)); code != 200 {
		t.Fatalf("check: %d %s", code, out)
	}
	// nothing between cycles shows it: not a read by an application, not
	// the loop that counts uploaded segments again
	e.srv.recheckUser(ctx, u.ID)
	apiJSON[api.ProjectStats](t, e, "/api/v1/users/cyd/projects/zorblax/stats", "")
	if sgs, _ = e.st.ListSegments(ctx, u.ID, p.ID); sgs[0].Scored.AISeconds != 0 {
		t.Fatalf("the prompt's time showed before the cycle: %+v", sgs[0].Scored)
	}
	if pend, _ := e.st.PendingSegments(ctx, 10); len(pend) != 0 {
		t.Fatalf("the recheck loop would take %d of the server's own stretches", len(pend))
	}
	now = t0.Add(2 * time.Hour)
	e.count()
	if sgs, _ = e.st.ListSegments(ctx, u.ID, p.ID); len(sgs) != 1 || sgs[0].Pending != 0 || sgs[0].Scored.AISeconds < 4*60-1 {
		t.Fatalf("after the cycle: %+v", sgs)
	}
	if n := waits(); n != 0 {
		t.Fatalf("%d prompts still shown as waiting", n)
	}
}

// What the server keeps is the chain the device's checkpoints commit to:
// records that do not replay to a checkpoint are refused, what runs past
// the last checkpoint is not kept, sending again changes nothing, and a
// record that is not in its private form is kept as its leaf hash only.
func TestRecordsMustReplayToACheckpoint(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	t0 := time.Now().UTC().Truncate(time.Minute).Add(-30 * time.Minute)
	c := newRecChain(t, d, key)
	for i := 0; i < 6; i++ {
		c.add(event.Event{TS: t0.Add(time.Duration(i) * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	}
	post := func(r api.RecordsReq) (int, api.RecordsResp) {
		t.Helper()
		code, out := e.apiDo("POST", "/api/v1/records", d.token, r)
		var resp api.RecordsResp
		json.Unmarshal(out, &resp)
		return code, resp
	}
	// no checkpoint yet: nothing is kept, and the server says so
	if code, r := post(c.req(1, 6)); code != 409 || r.Head != 0 || r.Next != 0 {
		t.Fatalf("with no checkpoint: %d %+v", code, r)
	}
	// a checkpoint at 4 only: the server asks for up to 4, and keeps 1..4 of 1..6
	head4 := c.prev[4]
	if code, out := d.sync(chain.Checkpoint{Seq: 4, Hash: head4}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	if code, r := post(api.RecordsReq{DeviceID: d.id, From: 1}); code != 200 || r.Head != 0 || r.Next != 4 {
		t.Fatalf("where to go on from: %d %+v", code, r)
	}
	// a record changed after the fact does not replay
	bad := c.req(1, 4)
	changed := *bad.Entries[1].Event
	changed.Lines = 3000
	bad.Entries[1].Event = &changed
	if code, _ := post(bad); code != 400 {
		t.Fatalf("a changed record: %d", code)
	}
	if code, r := post(c.req(1, 6)); code != 200 || r.Head != 4 {
		t.Fatalf("records past the checkpoint: %d %+v", code, r)
	}
	if code, r := post(c.req(1, 4)); code != 200 || r.Head != 4 {
		t.Fatalf("sent again: %d %+v", code, r)
	}
	// records that do not go on from the server's head
	c.sync()
	wrong := c.req(5, 6)
	wrong.StartHash = h(7)
	if code, r := post(wrong); code != 409 || r.Head != 4 || r.Next != 6 {
		t.Fatalf("another chain's records: %d %+v", code, r)
	}
	// a record with text in it, and one of no project: leaf hashes only
	c.add(event.Event{TS: t0.Add(7 * time.Minute), Kind: event.Prompt, Harness: "claude-code", Text: "SECRET-TEXT in the clear", Canon: event.CanonPrivate})
	c.add(event.Event{TS: t0.Add(8 * time.Minute), Kind: event.SessionStart, Harness: "claude-code", Project: "-"})
	c.sync()
	if code, r := post(c.req(5, 8)); code != 200 || r.Head != 8 {
		t.Fatalf("the rest: %d %+v", code, r)
	}
	rows, err := e.st.RecordRows(ctx, d.id, 1, 100)
	if err != nil || len(rows) != 8 {
		t.Fatalf("%d records kept (%v)", len(rows), err)
	}
	for _, r := range rows {
		if strings.Contains(r.Data, "SECRET-TEXT") {
			t.Fatal("text reached the database")
		}
		if full := r.Seq <= 6; full != (r.Data != "") {
			t.Errorf("record %d: kept in full = %v", r.Seq, r.Data != "")
		}
		if r.Seq > 1 && r.Prev != c.prev[r.Seq-1] {
			t.Errorf("record %d: chain head before it", r.Seq)
		}
	}
	// another device's token cannot send for this one
	other, _ := e.enroll("eve")
	if code, _ := e.apiDo("POST", "/api/v1/records", other.token, c.req(1, 4)); code != 403 {
		t.Fatalf("another device's token: %d", code)
	}
}

// A device that lost part of its log goes on after a hole: the stretch
// that was open ends there, and the records after the hole are counted on
// their own.
func TestRecordsGoOnAfterAHole(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	c := newRecChain(t, d, key)
	edit := func(min int) {
		c.add(event.Event{TS: t0.Add(time.Duration(min) * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	}
	edit(0)
	edit(1)
	now = t0.Add(2 * time.Minute)
	c.push()
	now = t0.Add(3 * time.Minute)
	e.count()
	// records 3 and 4 are lost with the log; the device syncs at 6 and
	// sends 5 and 6
	for m := 2; m < 6; m++ {
		edit(m)
	}
	now = t0.Add(7 * time.Minute)
	c.sync()
	code, out := e.apiDo("POST", "/api/v1/records", d.token, c.req(5, 6))
	var r api.RecordsResp
	if json.Unmarshal(out, &r); code != 200 || r.Head != 6 {
		t.Fatalf("after a hole: %d %s", code, out)
	}
	st, err := e.st.RecordStream(ctx, d.id)
	if err != nil || st.Head != 6 || st.RunLo != 5 {
		t.Fatalf("stream: %+v %v", st, err)
	}
	now = t0.Add(8 * time.Minute)
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	p, _ := e.st.ProjectByKey(ctx, u.ID, key)
	sgs, _ := e.st.ListSegments(ctx, u.ID, p.ID)
	if len(sgs) != 2 || sgs[0].Open || !sgs[1].Open || sgs[0].SeqHi != 2 || sgs[1].SeqLo != 5 {
		t.Fatalf("stretches: %+v", sgs)
	}
	for i, sg := range sgs {
		if sg.Status != store.SegVerified || sg.Scored.Seconds() <= 0 {
			t.Errorf("stretch %d: %s %v %v s", i, sg.Status, sg.Problems, sg.Scored.Seconds())
		}
	}
}

// A device that uploaded its stretches itself (an older SeaTime) and then
// sends records: the server's stretches begin after what was uploaded, so
// no record is in two of them.
func TestRecordsAfterUploadedSegments(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	c := newRecChain(t, d, key)
	edit := func(at time.Time) {
		c.add(event.Event{TS: at, Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	}
	edit(t0)
	edit(t0.Add(time.Minute))
	now = t0.Add(2 * time.Minute)
	c.sync()
	// the old way: the device uploads records 1..2 as a segment
	b := api.Bundle{DeviceID: d.id, Project: key, StartHash: chain.Genesis, ScoreVersion: ledger.Version,
		Entries: []chain.Entry{{Seq: 1, Event: &c.evs[0]}, {Seq: 2, Event: &c.evs[1]}}, Claimed: audit.Claim(c.evs, key)}
	d.resign(&b)
	if code, out := e.apiDo("POST", "/api/v1/segments", d.token, b); code != 200 {
		t.Fatalf("segment: %d %s", code, out)
	}
	// updated: it sends its records from the first one, and works on
	later := t0.Add(5 * time.Hour)
	edit(later)
	edit(later.Add(time.Minute))
	now = later.Add(2 * time.Minute)
	c.push()
	now = later.Add(3 * time.Minute)
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	p, _ := e.st.ProjectByKey(ctx, u.ID, key)
	sgs, _ := e.st.ListSegments(ctx, u.ID, p.ID)
	if len(sgs) != 2 || sgs[0].Stream || !sgs[1].Stream {
		t.Fatalf("segments: %+v", sgs)
	}
	if !sgs[1].FirstTS.Equal(later) || sgs[1].Scored.Seconds() <= 0 || sgs[1].Status != store.SegVerified {
		t.Fatalf("the server's stretch: %+v", sgs[1])
	}
	ev, err := e.srv.reviewEvidence(ctx, u, p, t0.Add(-time.Hour), later.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sg := range ev.Segments {
		n += len(sg.Records)
	}
	if n != 4 {
		t.Fatalf("reviewers read %d records, want 4", n)
	}
}

// The computer is closed right after the work. The server closes the
// stretch on its own; when the computer is back, the idle timer's record
// of those minutes arrives late, and the stretch is counted again with it.
func TestLateRecordCountsAClosedStretchAgain(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	text := "make the parser keep comments so the formatter can put them back"
	es, salts := chained(0, promptcheck.ChainGenesis, key, t0, text)
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[0], salts[0], text)); code != 200 {
		t.Fatalf("check: %d %s", code, out)
	}
	c := newRecChain(t, d, key)
	no := false
	c.add(event.Event{TS: es[0].TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(text), Words: 12, New: &no, CheckHash: es[0].Commit})
	c.add(event.Event{TS: t0.Add(2 * time.Minute), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s"})
	now = t0.Add(3 * time.Minute)
	c.push()
	now = t0.Add(5 * time.Hour)
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	sgs, _ := e.st.ListSegments(ctx, u.ID, 0)
	if len(sgs) != 1 || sgs[0].Open || sgs[0].Scored.AISeconds > 2*60+1 {
		t.Fatalf("closed with the computer off: %+v", sgs)
	}
	// the idle timer watched the 12 minutes from the prompt and saw input
	// throughout; its record was written but not sent before the lid closed
	c.add(event.Event{TS: t0.Add(12 * time.Minute), Kind: event.Idle, Harness: "seatime", WindowMS: (12 * time.Minute).Milliseconds(), IdleSrc: "ioreg"})
	now = t0.Add(20 * time.Hour)
	c.push()
	now = t0.Add(21 * time.Hour)
	e.count()
	sgs, _ = e.st.ListSegments(ctx, u.ID, 0)
	if len(sgs) != 1 || sgs[0].Open || sgs[0].Status != store.SegVerified || sgs[0].Scored.AISeconds < 10*60-1 { // the prompt's 10 minutes, while the person was there
		t.Fatalf("after the late record: %+v", sgs)
	}
}

// Work with no pause of three hours for days is still cut, so that no
// count reads more than two days of a device's records.
func TestOpenStretchIsCutAfterTwoDays(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	c := newRecChain(t, d, "github.com/cyd/zorblax")
	// an edit every two hours for three days, synced as it is made; one
	// pause of two and a half hours on the second day
	at := t0
	for i := 0; i < 36; i++ {
		c.add(event.Event{TS: at, Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
		now = at.Add(time.Minute)
		c.push()
		at = at.Add(2 * time.Hour)
		if i == 14 {
			at = at.Add(30 * time.Minute)
		}
	}
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	sgs, _ := e.st.ListSegments(ctx, u.ID, 0)
	if len(sgs) != 2 || sgs[0].Open || !sgs[1].Open || !sgs[0].Until.Equal(sgs[1].Since) {
		t.Fatalf("stretches: %+v", sgs)
	}
	// the cut is in the longest pause
	if cut := sgs[0].Until; !cut.After(c.evs[14].TS) || !cut.Before(c.evs[15].TS) {
		t.Fatalf("cut at %s, want inside the pause from %s to %s", cut, c.evs[14].TS, c.evs[15].TS)
	}
	if span := sgs[1].LastTS.Sub(sgs[1].Since); span > openMax {
		t.Fatalf("the open stretch spans %s", span)
	}
}

// A minute never counts twice. A changed client sends its records and
// also uploads the same records as a segment of its own, after the open
// stretch was first counted: when the stretch grows over those minutes,
// the segment gives them up.
func TestStretchAndUploadedSegmentDoNotBothCount(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	key := "github.com/cyd/zorblax"
	c := newRecChain(t, d, key)
	edit := func(min int) {
		c.add(event.Event{TS: t0.Add(time.Duration(min) * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	}
	edit(0)
	edit(1)
	now = t0.Add(2 * time.Minute)
	c.push()
	now = t0.Add(3 * time.Minute)
	e.count() // the open stretch: one minute
	for m := 2; m <= 20; m++ {
		edit(m)
	}
	now = t0.Add(21 * time.Minute)
	c.push()
	// the same records as a segment of the device's own, made after the stretch
	b := api.Bundle{DeviceID: d.id, Project: key, StartHash: chain.Genesis, ScoreVersion: ledger.Version, Claimed: audit.Claim(c.evs, key)}
	for i := range c.evs {
		b.Entries = append(b.Entries, chain.Entry{Seq: c.evs[i].Seq, Event: &c.evs[i]})
	}
	d.resign(&b)
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, b)
	var seg api.SegmentResp
	if json.Unmarshal(out, &seg); code != 200 || seg.Status != api.StatusVerified || seg.Scored.Seconds() < 18*60 {
		t.Fatalf("segment: %d %s", code, out)
	}
	now = t0.Add(time.Hour)
	e.count() // the stretch grows over the segment's minutes
	u, _ := e.st.UserByName(ctx, "cyd")
	p, _ := e.st.ProjectByKey(ctx, u.ID, key)
	ms, err := e.st.VerifiedMinutes(ctx, p.ID, t0.Add(-time.Hour), t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, m := range ms {
		total += m.Human
		if m.Human > 60.5 {
			t.Errorf("minute %s holds %.0f s", time.Unix(m.StartMin*60, 0).UTC().Format("15:04"), m.Human)
		}
	}
	if total < 19*60 || total > 20*60+1 {
		t.Fatalf("an application reads %.0f s for 20 minutes of work", total)
	}
}

// Two computers of one account are used in the same minutes. Each device's
// stretch is counted in full only once: the one made later gives up what
// the other holds, also when the other's records come after it was counted.
func TestTwoDevicesInTheSameMinutes(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	a, br := e.enroll("cyd")
	b := e.another(br, "second")
	ctx := context.Background()
	ca, cb := newRecChain(t, a, "github.com/cyd/one"), newRecChain(t, b, "github.com/cyd/two")
	work := func(c *recChain, from, to int) {
		for m := from; m <= to; m++ {
			c.add(event.Event{TS: t0.Add(time.Duration(m) * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
		}
	}
	// the first computer starts; its stretch is made first
	work(ca, 0, 2)
	now = t0.Add(3 * time.Minute)
	ca.push()
	e.count()
	// both work through the same ten minutes; the second one's records
	// arrive and are counted while the first is offline
	work(cb, 10, 20)
	now = t0.Add(21 * time.Minute)
	cb.push()
	e.count()
	work(ca, 10, 20)
	now = t0.Add(22 * time.Minute)
	ca.push()
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	sgs, _ := e.st.ListSegments(ctx, u.ID, 0)
	perMin := map[int64]float64{}
	for _, sg := range sgs {
		ms, _ := e.st.SegmentMinutesOf(ctx, sg.ID)
		for _, m := range ms {
			perMin[m.StartMin] += m.Human
		}
	}
	total := 0.0
	for m, sec := range perMin {
		total += sec
		if sec > 60.5 {
			t.Errorf("minute %s holds %.0f s over the account's stretches", time.Unix(m*60, 0).UTC().Format("15:04"), sec)
		}
	}
	// minutes 0 to 2 and 10 to 20, and the two minutes before a heartbeat
	// that ends a long gap: 14 minutes, whichever computer they are on
	if len(sgs) != 2 || total < 14*60-1 || total > 14*60+1 {
		t.Fatalf("%d stretches hold %.0f s for 14 minutes of work", len(sgs), total)
	}
}

// Records that arrive after the last count and before a hole are counted:
// the stretch that was open ends at the last record the server holds of
// it, not at the last one it had counted.
func TestRecordsBeforeAHoleAreCounted(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	c := newRecChain(t, d, "github.com/cyd/zorblax")
	edit := func(min int) {
		c.add(event.Event{TS: t0.Add(time.Duration(min) * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	}
	edit(0)
	edit(1)
	now = t0.Add(2 * time.Minute)
	c.push()
	e.count()
	for m := 2; m <= 30; m++ { // 29 more minutes, sent as they are made
		edit(m)
		now = t0.Add(time.Duration(m)*time.Minute + 30*time.Second)
		c.push()
	}
	// the log is lost: records 32..35 never reach the server, 36..40 do
	for m := 31; m <= 39; m++ {
		edit(m)
	}
	now = t0.Add(40 * time.Minute)
	c.sync()
	if code, out := e.apiDo("POST", "/api/v1/records", d.token, c.req(36, 40)); code != 200 {
		t.Fatalf("after a hole: %d %s", code, out)
	}
	now = t0.Add(time.Hour)
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	sgs, _ := e.st.ListSegments(ctx, u.ID, 0)
	if len(sgs) != 2 || sgs[0].Open || sgs[0].SeqHi != 31 || sgs[1].SeqLo != 36 {
		t.Fatalf("stretches: %+v", sgs)
	}
	if got := sgs[0].Scored.Seconds(); got < 29*60 {
		t.Fatalf("the stretch before the hole holds %.0f s for 30 minutes of work", got)
	}
	if got := sgs[1].Scored.Seconds(); got < 3*60 {
		t.Fatalf("the stretch after the hole holds %.0f s", got)
	}
}

// A record dated far from now is kept as its leaf hash only, so that no
// count has to read years of empty time.
func TestRecordsFarFromNowAreNotKeptInFull(t *testing.T) {
	e := newEnv(t)
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("cyd")
	ctx := context.Background()
	c := newRecChain(t, d, "github.com/cyd/zorblax")
	for _, at := range []time.Time{t0.Add(time.Minute), t0.AddDate(100, 0, 0), t0.AddDate(-5, 0, 0), t0.Add(2 * time.Minute)} {
		c.add(event.Event{TS: at, Kind: event.Edit, Harness: "editor:vscode", Category: "coding", Lines: 3})
	}
	now = t0.Add(3 * time.Minute)
	c.push()
	rows, _ := e.st.RecordRows(ctx, d.id, 1, 10)
	if len(rows) != 4 || rows[0].Data == "" || rows[1].Data != "" || rows[2].Data != "" || rows[3].Data == "" {
		t.Fatalf("kept in full: %v %v %v %v", rows[0].Data != "", rows[1].Data != "", rows[2].Data != "", rows[3].Data != "")
	}
	now = t0.Add(4 * time.Minute)
	e.count()
	u, _ := e.st.UserByName(ctx, "cyd")
	if sgs, _ := e.st.ListSegments(ctx, u.ID, 0); len(sgs) != 1 || sgs[0].Status != store.SegVerified || sgs[0].Scored.Seconds() < 59 {
		t.Fatalf("stretches: %+v", sgs)
	}
}

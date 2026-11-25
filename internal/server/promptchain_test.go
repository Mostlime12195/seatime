package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/checks"
	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/stats"
	clientstore "seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// chained makes chain entries for texts after (seq, link) in project, one
// a second from t0, returning them with their salts.
func chained(seq int64, link, project string, t0 time.Time, texts ...string) ([]promptcheck.ChainEntry, []string) {
	var es []promptcheck.ChainEntry
	var salts []string
	for i, tx := range texts {
		salt := promptcheck.NewSalt()
		e := promptcheck.ChainEntry{Seq: seq + 1, TS: t0.Add(time.Duration(i) * time.Second).UTC().Truncate(time.Millisecond), Project: project, Commit: promptcheck.Hash(salt, tx)}
		e.Link = promptcheck.ChainLink(link, e)
		es, salts = append(es, e), append(salts, salt)
		seq, link = e.Seq, e.Link
	}
	return es, salts
}

func (d *device) pushHashes(es []promptcheck.ChainEntry) (int, api.PromptHashesResp) {
	code, out := d.e.apiDo("POST", "/api/v1/prompt-hashes", d.token, api.PromptHashesReq{DeviceID: d.id, Entries: es})
	var r api.PromptHashesResp
	json.Unmarshal(out, &r)
	return code, r
}

// chainedReq is a check request for a chained prompt.
func chainedReq(d *device, e promptcheck.ChainEntry, salt, text string) api.PromptCheckReq {
	r := checkReq(d, text)
	r.CheckHash, r.Salt, r.PromptTS = e.Commit, salt, e.TS
	return r
}

func syncResp(t *testing.T, d *device, seq int64) api.SyncResp {
	t.Helper()
	code, out := d.sync(chain.Checkpoint{Seq: seq, Hash: h(int(seq))}, nil, nil)
	var r api.SyncResp
	if code != 200 || json.Unmarshal([]byte(out), &r) != nil {
		t.Fatalf("sync: %d %s", code, out)
	}
	return r
}

func setCheck(t *testing.T, br *browser, on bool) {
	t.Helper()
	form := url.Values{"csrf": {br.csrf("/settings")}}
	if on {
		form.Set("prompt_check", "1")
	}
	if code, body := br.post("/settings/prompt-check", form); code != 200 || !strings.Contains(body, "Check my prompts with AI") {
		t.Fatalf("settings: %d", code)
	}
}

// The server takes each device's hashes in chain order only: a retry is
// skipped, a gap, a changed entry or a reordered one is refused with the
// head it holds.
func TestPromptHashChain(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	d, _ := e.enroll("ada")
	t0 := time.Now().Add(-time.Hour)
	es, _ := chained(0, promptcheck.ChainGenesis, "local:abc", t0, "one", "two", "three")
	if code, r := d.pushHashes(es); code != 200 || r.Seq != 3 || r.Taken != 3 || r.Link != es[2].Link {
		t.Fatalf("push: %d %+v", code, r)
	}
	if code, r := d.pushHashes(es); code != 200 || r.Taken != 0 || r.Seq != 3 {
		t.Fatalf("retry: %d %+v", code, r)
	}
	// a gap
	gap, _ := chained(4, es[2].Link, "local:abc", t0, "five")
	if code, r := d.pushHashes(gap); code != 409 || r.Seq != 3 || r.Link != es[2].Link {
		t.Fatalf("gap: %d %+v", code, r)
	}
	// entry 3 changed after the fact (another text, same place)
	changed, _ := chained(2, es[1].Link, "local:abc", t0, "three, edited")
	if code, _ := d.pushHashes(changed); code != 409 {
		t.Fatalf("changed: %d", code)
	}
	// two new entries swapped
	next, _ := chained(3, es[2].Link, "local:abc", t0.Add(10*time.Minute), "four", "five")
	swapped := []promptcheck.ChainEntry{next[1], next[0]}
	swapped[0].Seq, swapped[1].Seq = 4, 5
	if code, _ := d.pushHashes(swapped); code != 409 {
		t.Fatalf("reordered: %d", code)
	}
	// a link that does not hash its own fields (a moved time)
	moved := next[0]
	moved.TS = moved.TS.Add(-time.Hour)
	if code, _ := d.pushHashes([]promptcheck.ChainEntry{moved}); code != 409 {
		t.Fatalf("moved time: %d", code)
	}
	future, _ := chained(3, es[2].Link, "local:abc", time.Now().Add(time.Hour), "later")
	if code, _ := d.pushHashes(future); code != 400 {
		t.Fatalf("future: %d", code)
	}
	if code, r := d.pushHashes(next); code != 200 || r.Seq != 5 {
		t.Fatalf("in order: %d %+v", code, r)
	}
	// another device's chain is its own
	d2, _ := e.enroll("ada2")
	if code, r := d2.pushHashes(es[:1]); code != 200 || r.Seq != 1 {
		t.Fatalf("second device: %d %+v", code, r)
	}
}

// A chained prompt's check is tied to its entry: the text must match, the
// time is the chain's, and a mismatch closes the prompt for good (it then
// counts as not passing, on the server and, after a sync, on the device).
func TestChainedCheckTamper(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	d, _ := e.enroll("ada")
	t0 := time.Now().Add(-time.Hour)
	texts := []string{"PROMPT-CANARY make the zorblax counter purple", "PROMPT-CANARY now make it bounce", "PROMPT-CANARY a third one", "PROMPT-CANARY a fourth one"}
	es, salts := chained(0, promptcheck.ChainGenesis, "local:abc", t0, texts...)
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	ctx := context.Background()

	// the text as hashed: judged, at the chain's time
	ok := chainedReq(d, es[0], salts[0], texts[0])
	ok.PromptTS = time.Time{} // an older request shape: the chain gives the time
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, ok); code != 200 {
		t.Fatalf("check: %d %s", code, out)
	}
	if ps, _ := e.st.PassingPrompts(ctx, d.id, t0.Add(-time.Second), t0.Add(time.Second)); len(ps) != 1 || !ps[0].TS.Equal(es[0].TS) {
		t.Fatalf("verdict time: %+v", ps)
	}
	// edited text: refused unjudged, and the entry is closed
	bad := chainedReq(d, es[1], salts[1], texts[1]+" (edited)")
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, bad); code != 422 {
		t.Fatalf("tampered: %d", code)
	}
	if row, _ := e.st.PromptChainEntry(ctx, d.id, es[1].Commit); row == nil || row.Status != store.ChainMismatch {
		t.Fatalf("not closed: %+v", row)
	}
	// the right text after that is refused too: closed is closed
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[1], salts[1], texts[1])); code != 422 {
		t.Fatalf("after the mismatch: %d", code)
	}
	// a time that is not the chain's (the prompt moved): refused and closed
	moved := chainedReq(d, es[2], salts[2], texts[2])
	moved.PromptTS = es[2].TS.Add(-30 * time.Minute)
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, moved); code != 422 {
		t.Fatalf("moved: %d", code)
	}
	// a hash the chain does not hold yet: wait (409), not refused
	missing := checkReq(d, "PROMPT-CANARY not pushed yet")
	missing.PromptTS = time.Now().Add(-time.Minute)
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, missing); code != 409 {
		t.Fatalf("unchained: %d", code)
	}
	if j.calls != 1 {
		t.Fatalf("the model was asked %d times", j.calls)
	}
	// the sync answer lists both closed prompts; the server counts them as not passing
	r := syncResp(t, d, 1)
	if r.Chain != 1 || len(r.Closed) != 2 {
		t.Fatalf("closed in the answer: %+v", r.PromptChainResp)
	}
	pc, err := e.srv.promptChecks(ctx, d.id, &api.Bundle{Entries: []chain.Entry{{Seq: 1, Event: &event.Event{Kind: event.Prompt, CheckHash: es[1].Commit}}}})
	if err != nil {
		t.Fatal(err)
	}
	if pass, ok := pc.Pass[es[1].Commit]; !ok || pass {
		t.Fatalf("a closed prompt counts: %v %v", pass, ok)
	}
}

// The account's own setting: off stops checks (their prompts wait on the
// device), a pull asks for them regardless, and what a pull asked for and
// never came is closed as missing at its deadline.
func TestUserSettingAndPulls(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	d, br := e.enroll("ada")
	ctx := context.Background()
	if r := syncResp(t, d, 1); !r.PromptCheck || len(r.UserPeriods) != 0 {
		t.Fatalf("on by default: %+v", r)
	}
	if code, body := br.get("/settings"); code != 200 || !strings.Contains(body, "The prompts that you send while this is off are checked later") || !strings.Contains(body, `name="prompt_check" value="1" checked`) {
		t.Fatalf("settings page: %d", code)
	}
	setCheck(t, br, false)
	r := syncResp(t, d, 2)
	if r.PromptCheck || len(r.UserPeriods) != 1 || r.UserPeriods[0].On {
		t.Fatalf("off: %+v", r)
	}
	if !r.ServerCheck || r.CheckPeriods[len(r.CheckPeriods)-1].On {
		t.Fatalf("an older device must see it off: %+v", r.CheckPeriods)
	}
	t0 := r.UserPeriods[0].From.Add(time.Millisecond)
	texts := []string{"PROMPT-CANARY in the shipped project", "PROMPT-CANARY in another project", "PROMPT-CANARY never sent"}
	es, salts := chained(0, promptcheck.ChainGenesis, "github.com/ada/zorblax", t0, texts[0])
	more, msalts := chained(1, es[0].Link, "local:other", t0.Add(time.Millisecond), texts[1:]...)
	es, salts = append(es, more...), append(salts, msalts...)
	if code, _ := d.pushHashes(es); code != 200 {
		t.Fatal(code)
	}
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[0], salts[0], texts[0])); code != 404 || j.calls != 0 {
		t.Fatalf("checked while off: %d", code)
	}

	// shipping the project asks for its prompts and for every prompt of the
	// stretch it was off
	u, _ := e.st.UserByName(ctx, "ada")
	pulls, err := ShipPull(ctx, e.st, u.ID, "github.com/ada/zorblax", "test", time.Now(), time.Hour)
	if err != nil || len(pulls) != 2 || pulls[1].Project != "" {
		t.Fatalf("pulls %+v %v", pulls, err)
	}
	r = syncResp(t, d, 3)
	if len(r.Pulls) != 2 {
		t.Fatalf("pulls in the answer: %+v", r.Pulls)
	}
	for i := 0; i < 2; i++ {
		if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[i], salts[i], texts[i])); code != 200 {
			t.Fatalf("pulled check %d: %d %s", i, code, out)
		}
	}
	// the third never comes: past the deadline it is missing, and the pulls close
	e.srv.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	r = syncResp(t, d, 4)
	if len(r.Pulls) != 0 || len(r.Closed) != 1 || r.Closed[0].Commit != es[2].Commit || r.Closed[0].Status != store.ChainMissing {
		t.Fatalf("after the deadline: %+v", r.PromptChainResp)
	}
	// every prompt of the stretch is settled: it counts by the check now
	pc, err := e.srv.promptChecks(ctx, d.id, &api.Bundle{Entries: []chain.Entry{{Seq: 1, Event: &event.Event{Kind: event.Prompt, CheckHash: es[0].Commit}}}})
	if err != nil || !pc.OnAt(t0) || !pc.OnAt(es[2].TS) || !pc.Pass[es[0].Commit] {
		t.Fatalf("the stretch did not switch: %+v %v", pc, err)
	}
}

// An older device keeps no chain and sends no hashes: its checks work as
// before while the account has the check on, stop while it is off, and its
// time while off is never switched to the check.
func TestOldClientWithoutChain(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	d, br := e.enroll("ada")
	ctx := context.Background()
	req := checkReq(d, secretPrompt)
	req.PromptTS = time.Now().Add(-time.Minute)
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, req); code != 200 {
		t.Fatalf("old client check: %d %s", code, out)
	}
	setCheck(t, br, false)
	r := syncResp(t, d, 1)
	if r.PromptCheck || r.CheckPeriods[len(r.CheckPeriods)-1].On {
		t.Fatalf("an older device must see it off: %+v", r)
	}
	req2 := checkReq(d, "PROMPT-CANARY while off")
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, req2); code != 404 {
		t.Fatalf("checked while off: %d", code)
	}
	time.Sleep(5 * time.Millisecond)
	setCheck(t, br, true)
	u, _ := e.st.UserByName(ctx, "ada")
	ps := e.srv.userCheckPeriods(ctx, u.ID)
	pc, err := e.srv.promptChecks(ctx, d.id, &api.Bundle{})
	if err != nil || len(ps) != 2 || pc.OnAt(ps[0].From.Add(time.Millisecond)) {
		t.Fatalf("an older device's off stretch switched: %+v %v", pc, err)
	}
}

// The judge failing or timing out stores nothing: the prompt stays open
// on the server and waiting on the device, and goes through later.
func TestChainedCheckJudgeDown(t *testing.T) {
	j := &fakeJudge{p: passing(), err: errors.New("upstream timeout")}
	e, _, _ := newCheckEnv(t, true, j)
	d, _ := e.enroll("ada")
	es, salts := chained(0, promptcheck.ChainGenesis, "local:abc", time.Now().Add(-time.Hour), secretPrompt)
	d.pushHashes(es)
	req := chainedReq(d, es[0], salts[0], secretPrompt)
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, req); code != 503 {
		t.Fatalf("judge down: %d", code)
	}
	if row, _ := e.st.PromptChainEntry(context.Background(), d.id, es[0].Commit); row.Status != store.ChainOpen {
		t.Fatalf("a failed call closed the prompt: %+v", row)
	}
	j.err = nil
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, req); code != 200 {
		t.Fatalf("judge back: %d", code)
	}
}

// End to end with the real device code: the check off, prompts kept and
// their hashes sent; turned on halfway, the device sends the backlog, and
// the device and the server switch the stretch together. Then the device's
// store is lost: its new prompts go after the server's chain.
func TestOffThenOnEndToEnd(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	d, br := e.enroll("ada")
	home := t.TempDir()
	t.Setenv("SEATIME_HOME", filepath.Join(home, "data"))
	proj := filepath.Join(home, "zorblax")
	os.MkdirAll(proj, 0o755)
	open := func(name string) (*clientstore.Store, *emit.Pipeline, *syncer.Syncer) {
		st, err := clientstore.Open(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		p := emit.New(st, home)
		p.Snap = func(string) *promptcheck.Snapshot { return &promptcheck.Snapshot{} }
		sy := &syncer.Syncer{Store: st, Version: "test", Auth: &config.Auth{Token: d.token, DeviceID: d.id, Server: e.ts.URL,
			PrivateKey: base64.StdEncoding.EncodeToString(d.priv), PublicKey: base64.StdEncoding.EncodeToString(d.pub)}}
		return st, p, sy
	}
	st, p, sy := open("c.db")
	ctx := context.Background()
	turn := func(p *emit.Pipeline, text string) event.Event {
		ev, err := p.EmitEvent(event.Event{Kind: event.Prompt, Text: text, Harness: "claude-code", CWD: proj, SessionID: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.EmitEvent(event.Event{Kind: event.AgentStop, Harness: "claude-code", CWD: proj, SessionID: "s1"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
		return ev
	}
	setCheck(t, br, false)
	time.Sleep(3 * time.Millisecond)
	a := turn(p, "PROMPT-CANARY make the zorblax counter purple")
	b := turn(p, "PROMPT-CANARY and make it bounce when it hits ten")
	if _, err := sy.SyncOnce(ctx); err != nil { // learns the server takes hashes
		t.Fatal(err)
	}
	if n, err := sy.PushHashes(ctx); err != nil || n != 2 { // as the daemon does, moments later
		t.Fatalf("push: %d %v", n, err)
	}
	if n, err := sy.PushHashes(ctx); err != nil || n != 0 {
		t.Fatalf("push: %d %v", n, err)
	}
	if seq, _, _ := e.st.PromptChainHead(ctx, d.id); seq != 2 {
		t.Fatalf("server chain: %d", seq)
	}
	if n, _ := sy.CheckOnce(ctx); n != 0 || j.calls != 0 {
		t.Fatal("checked while off")
	}
	time.Sleep(3 * time.Millisecond)
	setCheck(t, br, true)
	turn(p, "PROMPT-CANARY now with the check back on")
	if _, err := sy.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := sy.CheckOnce(ctx); n != 3 || err != nil {
		t.Fatalf("backlog sent %d: %v", n, err)
	}
	u, _ := e.st.UserByName(ctx, "ada")
	srvPeriods, err := e.srv.effectivePeriods(ctx, d.id, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	dev := stats.PromptChecks(st)
	for _, at := range []time.Time{a.TS, b.TS} {
		if !(&promptcheck.Set{Periods: srvPeriods}).OnAt(at) || !dev.OnAt(at) {
			t.Fatalf("the stretch did not switch on both sides at %v: server %+v device %+v", at, srvPeriods, dev.Periods)
		}
	}

	// the store is lost: a new one starts its chain at 1, the server holds 3
	st2, p2, sy2 := open("c2.db")
	st2.SetMeta(clientstore.MetaChain, "1")
	turn(p2, "PROMPT-CANARY after the store was lost")
	if n, err := sy2.PushHashes(ctx); err != nil || n != 1 {
		t.Fatalf("push after loss: %d %v", n, err)
	}
	if seq, _, _ := e.st.PromptChainHead(ctx, d.id); seq != 4 {
		t.Fatalf("server chain after loss: %d", seq)
	}
	if es, _ := st2.UnsentChain(10); len(es) != 0 {
		t.Fatalf("still unsent: %+v", es)
	}
	// and its check goes through against the relinked entry
	st2.SetMeta(clientstore.MetaPromptCheck, "1")
	turn(p2, "PROMPT-CANARY one more after the loss") // ends the turn before it
	if _, err := sy2.PushHashes(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := checks.Drain(ctx, st2, d.id, func(ctx context.Context, req api.PromptCheckReq) (api.PromptCheckResp, int, error) {
		code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, req)
		var r api.PromptCheckResp
		json.Unmarshal(out, &r)
		if code != 200 {
			return r, code, errors.New(string(out))
		}
		return r, code, nil
	}, time.Now(), 20); n < 1 || err != nil {
		t.Fatalf("check after loss: %d %v", n, err)
	}
}

// An admin asks for a shipped project's prompts from /admin; anyone else
// gets a 404.
func TestAdminPromptPull(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	d, _ := e.enroll("ada")
	es, _ := chained(0, promptcheck.ChainGenesis, "github.com/ada/zorblax", time.Now().Add(-time.Minute), "PROMPT-CANARY one")
	d.pushHashes(es)
	user := e.browser()
	user.login("bob", "user")
	if code, _ := user.post("/admin/prompt-pull", url.Values{"csrf": {user.csrf("/settings")}, "username": {"ada"}, "project": {"github.com/ada/zorblax"}}); code != 404 {
		t.Fatalf("a user asked for prompts: %d", code)
	}
	admin := e.browser()
	admin.login("root", "admin")
	code, body := admin.post("/admin/prompt-pull", url.Values{"csrf": {admin.csrf("/admin")}, "username": {"ada"}, "project": {"github.com/ada/zorblax"}})
	if code != 200 || !strings.Contains(body, "Asked @ada") {
		t.Fatalf("admin pull: %d", code)
	}
	if r := syncResp(t, d, 1); len(r.Pulls) != 1 || r.Pulls[0].Project != "github.com/ada/zorblax" {
		t.Fatalf("pulls %+v", r.Pulls)
	}
}

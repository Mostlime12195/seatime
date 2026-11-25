package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/internal/server/store/storetest"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// An admin turns the check on from /admin, with no redeploy; the sync
// answer follows at once; the change is logged; a reset goes back to the
// environment's default. Only admins can change settings, and a bad value
// is refused.
func TestAdminSettingsChangeTheSwitch(t *testing.T) {
	e := newEnv(t)
	e.srv.SetJudge(&fakeJudge{p: passing()})
	d, _ := e.enroll("lena")
	seq := int64(0)
	switchOn := func() bool {
		seq++
		_, out := d.sync(chain.Checkpoint{Seq: seq, Hash: h(int(seq))}, nil, nil)
		var r api.SyncResp
		json.Unmarshal([]byte(out), &r)
		return r.PromptCheck
	}
	if switchOn() {
		t.Fatal("on by default")
	}
	admin := e.browser()
	admin.login("boss", "admin")
	tok := admin.csrf("/admin")
	if c, body := admin.post("/admin/settings", url.Values{"csrf": {tok}, "key": {"prompt_check"}, "value": {"on"}}); c != 200 || !strings.Contains(body, "Saved") {
		t.Fatalf("save: %d", c)
	}
	if !switchOn() {
		t.Fatal("the sync answer did not follow the setting")
	}
	_, page := admin.get("/admin")
	if !strings.Contains(page, "The check is on.") || !strings.Contains(page, "@boss set Prompt check") {
		t.Fatalf("admin page does not show the change")
	}
	if c, _ := admin.post("/admin/settings", url.Values{"csrf": {tok}, "key": {"prompt_check"}, "value": {"maybe"}}); c != 400 {
		t.Fatalf("bad value: %d", c)
	}
	if c, _ := admin.post("/admin/settings", url.Values{"csrf": {"nope"}, "key": {"prompt_check"}, "value": {"off"}}); c != 403 {
		t.Fatalf("bad csrf: %d", c)
	}
	user := e.browser()
	user.login("pleb", "user")
	if c, _ := user.post("/admin/settings", url.Values{"csrf": {user.csrf("/settings")}, "key": {"prompt_check"}, "value": {"off"}}); c != 404 {
		t.Fatalf("a non-admin changed a setting: %d", c)
	}
	if !switchOn() {
		t.Fatal("a refused change took effect")
	}
	admin.post("/admin/settings", url.Values{"csrf": {tok}, "key": {"prompt_check"}, "action": {"reset"}})
	if switchOn() {
		t.Fatal("reset did not go back to the default")
	}
	log, _ := e.st.SettingsLog(context.Background(), 10)
	if len(log) != 2 || log[0].New != nil || log[1].By != "boss" {
		t.Fatalf("log %+v", log)
	}
}

// A stored value that no longer parses does not stop the server: its
// default is used.
func TestBadStoredSettingFallsBack(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := "sometimes"
	st.PutSetting(context.Background(), "prompt_check", &v, "x", time.Now())
	srv, err := New(Config{Dev: true, Secret: "s", PromptCheck: true, PromptCheckRules: testRulesJSON()}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || !srv.pol().PromptCheck {
		t.Fatalf("%v %v", err, srv.pol().PromptCheck)
	}
}

// The judges are asked in order: when the first fails, the next answers
// and is recorded on the verdict. With no judge answering (or no key) the
// device is told to wait (503 with Retry-After), never that the check is
// off, and nothing is decided.
func TestJudgeFallbackAndOutage(t *testing.T) {
	j := &fakeJudge{p: passing(), down: map[string]bool{"primary/model": true}}
	e, _, _ := newCheckEnv(t, true, j)
	e.srv.cfg.PromptCheckURL = "https://a.example/decisions,https://b.example/decisions"
	if err := e.srv.setSetting(context.Background(), "prompt_check_models", ptr("primary/model\nbackup/model https://b.example/decisions"), "boss"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.setSetting(context.Background(), "prompt_check_models", ptr("x https://evil.example/"), "boss"); err == nil {
		t.Fatal("a judge address outside SEATIME_PROMPT_CHECK_URL was accepted")
	}
	d, _ := e.enroll("ana")
	d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	req := checkReq(d, "PROMPT-CANARY add a pause key")
	code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, req)
	var resp api.PromptCheckResp
	json.Unmarshal(out, &resp)
	stored, _ := e.st.PromptVerdict(context.Background(), d.id, req.CheckHash)
	if code != 200 || resp.CheckHash != req.CheckHash || stored == nil || !stored.Pass || stored.Model != "backup/model-test" || len(j.targets) != 2 || j.targets[1].URL != "https://b.example/decisions" || j.targets[0].URL != "https://a.example/decisions" {
		t.Fatalf("fallback: %d %s %+v", code, out, j.targets)
	}
	j.down["backup/model"] = true
	req2 := checkReq(d, "PROMPT-CANARY add a high score")
	resp2, code := e.apiResp("POST", "/api/v1/prompt-check", d.token, req2)
	if code != 503 || resp2.Header.Get("Retry-After") == "" {
		t.Fatalf("outage: %d", code)
	}
	if _, err := e.st.PromptVerdict(context.Background(), d.id, req2.CheckHash); err == nil {
		t.Fatal("an outage decided a prompt")
	}
	lastOK, _, fails := e.srv.judgeStatus()
	if lastOK.IsZero() || fails < 3 {
		t.Fatalf("health: %v %d", lastOK, fails)
	}
}

// No key: the switch stays on, checks answer 503 and wait.
func TestNoKeyWaitsNotOff(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, nil)
	e.srv.judge = &promptcheck.Client{} // no key
	d, _ := e.enroll("kim")
	_, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	if !strings.Contains(out, `"prompt_check":true`) {
		t.Fatalf("no key turned the switch off: %s", out)
	}
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, checkReq(d, "PROMPT-CANARY a")); code != 503 {
		t.Fatalf("no key: %d, want 503", code)
	}
}

func ptr(s string) *string { return &s }

// apiResp is apiDo that also returns the response (for its headers).
func (e *tenv) apiResp(method, path, token string, in any) (*http.Response, int) {
	e.t.Helper()
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, resp.StatusCode
}

// An admin changes the minutes in the rules; the next verdict
// carries them (the device counts with them, no release), an earlier
// verdict keeps its own, and each records the rules it was judged with.
// Fields over the caps are cut and extra context turns dropped, not
// refused.
func TestRulesSettingSetsVerdictMinutes(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, _, _ := newCheckEnv(t, true, j)
	ctx := context.Background()
	d, _ := e.enroll("ana")
	d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	first := checkReq(d, "PROMPT-CANARY add a pause key")
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, first); code != 200 {
		t.Fatal(code)
	}
	r := testRules()
	r.Version = "v7.2"
	r.Kinds[1].Minutes = 7 // big
	r.Caps.Prompt, r.Caps.ContextTurns = 100, 1
	if err := e.srv.setSetting(ctx, "prompt_check_rules", ptr(string(r.JSON())), "boss"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.setSetting(ctx, "prompt_check_rules", ptr(`{"version":"x"}`), "boss"); err == nil {
		t.Fatal("rules that cannot decide were accepted")
	}
	second := checkReq(d, "PROMPT-CANARY "+strings.Repeat("make the board bigger ", 20))
	second.Context = append(second.Context, promptcheck.Turn{Prompt: "older", Activity: "start of session"}, promptcheck.Turn{Prompt: "LAST-CONTEXT", Activity: "start of session"})
	code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, second)
	var resp api.PromptCheckResp
	json.Unmarshal(out, &resp)
	edited, _ := e.st.PromptVerdict(ctx, d.id, second.CheckHash)
	if code != 200 || resp.CheckHash != second.CheckHash || edited == nil || edited.Minutes != 7 || edited.Rules != r.ID() {
		t.Fatalf("new rules: %d %s %+v", code, out, edited)
	}
	st := j.states[len(j.states)-1]
	if strings.Count(st, "=== TURN") != 2 || !strings.Contains(st, "LAST-CONTEXT") || strings.Contains(st, strings.Repeat("make the board bigger ", 20)) {
		t.Fatalf("caps not applied:\n%s", st)
	}
	old, _ := e.st.PromptVerdict(ctx, d.id, first.CheckHash)
	if old.Minutes != 10 || old.Rules != testRules().ID() {
		t.Fatalf("the old verdict changed: %+v", old)
	}
	set, _ := e.srv.promptChecks(ctx, d.id, &api.Bundle{Entries: []chain.Entry{
		{Seq: 1, Event: &event.Event{Kind: event.Prompt, CheckHash: first.CheckHash}},
		{Seq: 2, Event: &event.Event{Kind: event.Prompt, CheckHash: second.CheckHash}}}})
	if set.Min[first.CheckHash] != 10 || set.Min[second.CheckHash] != 7 {
		t.Fatalf("set minutes %v", set.Min)
	}
}

// The paid judge is bounded per account (rate and daily calls), by a
// daily dollar budget and by calls in flight; past a bound the check
// waits (429/503 with Retry-After), nothing is decided, and a device past
// the per-account cap is not connected.
func TestJudgeSpendLimits(t *testing.T) {
	j := &fakeJudge{p: passing(), cost: 0.01}
	e, _, _ := newCheckEnv(t, true, j)
	ctx := context.Background()
	d, _ := e.enroll("farm")
	d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	send := func(n int) int {
		resp, code := e.apiResp("POST", "/api/v1/prompt-check", d.token, checkReq(d, fmt.Sprintf("PROMPT-CANARY ask %d", n)))
		if code != 200 && resp.Header.Get("Retry-After") == "" {
			t.Fatalf("%d without Retry-After", code)
		}
		return code
	}
	e.srv.setSetting(ctx, "prompt_check_user_day", ptr("2"), "boss")
	if send(1) != 200 || send(2) != 200 || send(3) != 429 {
		t.Fatal("per-account daily limit")
	}
	e.srv.setSetting(ctx, "prompt_check_user_day", ptr("100"), "boss")
	e.srv.setSetting(ctx, "prompt_check_budget_usd_day", ptr("0.025"), "boss")
	if send(4) != 200 || send(5) != 503 {
		t.Fatal("daily budget")
	}
	if _, _, cost, _ := e.st.CheckSpendDay(ctx, utcDay(time.Now())); cost < 0.029 || cost > 0.031 {
		t.Fatalf("spend %v", cost)
	}
	e.srv.setSetting(ctx, "prompt_check_budget_usd_day", ptr("100"), "boss")
	e.srv.inflight.Store(8)
	if send(6) != 503 {
		t.Fatal("concurrency cap")
	}
	e.srv.inflight.Store(0)
	e.srv.setSetting(ctx, "prompt_check_user_per_min", ptr("0.1"), "boss") // a burst of 1
	send(7)
	if send(8) != 429 {
		t.Fatal("per-account rate")
	}
	if j.calls != 4 {
		t.Fatalf("%d paid calls", j.calls)
	}
	_, page := func() (int, string) { b := e.browser(); b.login("boss", "admin"); return b.get("/admin") }()
	if !strings.Contains(page, "4 paid judge calls") {
		t.Fatal("admin page does not show today's calls")
	}

	// devices per account
	e.srv.setSetting(ctx, "max_devices", ptr("1"), "boss")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, out := e.apiDo("POST", "/api/v1/device/start", "", api.DeviceStartReq{DeviceName: "second", OS: "linux", PublicKey: base64.StdEncoding.EncodeToString(pub)})
	var st api.DeviceStartResp
	json.Unmarshal(out, &st)
	br := e.browser()
	br.login("farm", "user")
	tok := br.csrf("/device?code=" + st.UserCode)
	if _, body := br.post("/device/approve", url.Values{"csrf": {tok}, "code": {st.UserCode}}); !strings.Contains(body, "the most it may have") {
		t.Fatal("a device past the cap was approved")
	}
}

// Each flip of the switch is recorded; the sync answer carries the
// history so devices count each stretch by its own rule.
func TestSwitchHistoryRecorded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.srv.setSetting(ctx, "prompt_check", ptr("on"), "boss")
	e.srv.setSetting(ctx, "prompt_check", ptr("on"), "boss") // no change, no period
	e.srv.setSetting(ctx, "prompt_check", ptr("off"), "boss")
	ps, _ := e.st.RulePeriods(ctx)
	if len(ps) != 3 || ps[0].On || !ps[1].On || ps[2].On || !ps[0].From.Equal(time.UnixMilli(0).UTC()) {
		t.Fatalf("periods %+v", ps)
	}
	d, _ := e.enroll("hist")
	_, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	var r api.SyncResp
	json.Unmarshal([]byte(out), &r)
	if len(r.CheckPeriods) != 3 || r.PromptCheck {
		t.Fatalf("sync answer %s", out)
	}
	// with the switch off now, verdicts still count for the stretch it was on
	if set, _ := e.srv.promptChecks(ctx, d.id, &api.Bundle{}); set == nil || set.On || !set.OnAt(ps[1].From) {
		t.Fatalf("set %+v", set)
	}
}

// The server reads the client version, refuses older ones once a
// minimum is set (keeping the checkpoint), shows the rollout on /admin,
// and after the device updates takes the minutes of the refused stretch.
func TestMinClientVersionAndRecovery(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d, _ := e.enroll("old")
	ver := func(v string) func(*api.SyncReq) { return func(r *api.SyncReq) { r.ClientVersion = v } }
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, ver("v0.9.0")); code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("no minimum: %s", out)
	}
	if err := e.srv.setSetting(ctx, "min_client_version", ptr("v1.0.0"), "boss"); err != nil {
		t.Fatal(err)
	}
	if e.srv.setSetting(ctx, "min_client_version", ptr("soon"), "boss") == nil {
		t.Fatal("a bad version was accepted")
	}
	// make the earlier accepted sync an hour old: what the device works
	// on while told to update must not be lost
	e.st.TouchDevice(ctx, d.id, time.Now().Add(-90*time.Minute))
	b := []ledger.Bucket{{Project: "github.com/old/app", Start: minute(60), CodingSeconds: 60}}
	if _, out := d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, b, ver("v0.9.0")); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "v1.0.0 or newer") {
		t.Fatalf("old client: %s", out)
	}
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, checkReq(d, "x")); code != 404 && code != 426 {
		t.Fatalf("old client check: %d", code)
	}
	admin := e.browser()
	admin.login("boss", "admin")
	if _, page := admin.get("/admin"); !strings.Contains(page, "v0.9.0: 1") || !strings.Contains(page, "1 device is older") {
		t.Fatal("/admin does not show the versions")
	}
	// updated: the refused stretch's minutes are taken
	if _, out := d.sync(chain.Checkpoint{Seq: 3, Hash: h(3)}, b, ver("v1.0.1")); !strings.Contains(out, `"ok":true`) || strings.Contains(out, "not stored") {
		t.Fatalf("updated: %s", out)
	}
	u, _ := e.st.UserByName(ctx, "old")
	if got, _ := e.st.DeviceBuckets(ctx, u.ID, d.id, minute(61), minute(0)); len(got) != 1 {
		t.Fatalf("the refused stretch was lost: %+v", got)
	}
	for v, old := range map[string]bool{"v0.9.9": true, "v1.0.0": false, "1.2.0-rc1": false, "dev": false, "": true} {
		if e.srv.clientTooOld(v) != old {
			t.Errorf("clientTooOld(%q) = %v", v, !old)
		}
	}
}

// Owner's req. 4: after a multi-day judge outage, a late verdict still
// raises the synced minutes it covers, up to prompt_check_late_days back;
// nothing older, and nothing it does not cover.
func TestLateVerdictAfterLongOutage(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	ctx := context.Background()
	d, _ := e.enroll("outage")
	threeDays, twentyDays := minute(3*24*60), minute(20*24*60)
	for i, at := range []time.Time{threeDays, twentyDays} {
		v := promptcheck.Verdict{Labels: promptcheck.Labels{}, Pass: true, Kind: "small", Model: "fake", Minutes: 10}
		if err := e.st.PutPromptVerdict(ctx, d.id, h(70+i), v, at.Add(-time.Minute), time.Now(), promptcheck.Evidence{}); err != nil {
			t.Fatal(err)
		}
	}
	p := "github.com/outage/app"
	bs := []ledger.Bucket{{Project: p, Start: threeDays, AISeconds: 60}, {Project: p, Start: threeDays.Add(-time.Hour), AISeconds: 60}, {Project: p, Start: twentyDays, AISeconds: 60}}
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, bs, nil); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	u, _ := e.st.UserByName(ctx, "outage")
	got, _ := e.st.DeviceBuckets(ctx, u.ID, d.id, minute(30*24*60), minute(0))
	if len(got) != 1 || !got[0].Start.Equal(threeDays) {
		t.Fatalf("stored %+v; want only the minute the 3-day-old verdict covers", got)
	}
}

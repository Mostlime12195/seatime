package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

// fakeJudge answers from a fixed table and remembers what it was asked.
// No test calls the real model.
type fakeJudge struct {
	mu      sync.Mutex
	calls   int
	states  []string
	targets []promptcheck.Target
	qs      map[string]promptcheck.Question
	p       map[string]float64
	err     error
	down    map[string]bool // models that fail
	cost    float64
}

func (f *fakeJudge) Judge(_ context.Context, t promptcheck.Target, state string, qs map[string]promptcheck.Question) (promptcheck.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.states = append(f.states, state)
	f.targets = append(f.targets, t)
	f.qs = qs
	if f.err != nil {
		return promptcheck.Answer{}, f.err
	}
	if f.down[t.Model] {
		return promptcheck.Answer{}, errors.New("promptcheck: decisions API answered 500")
	}
	cost := f.cost
	if cost == 0 {
		cost = 0.00001
	}
	return promptcheck.Answer{P: f.p, Model: t.Model + "-test", Cost: cost}, nil
}

func passing() map[string]float64 {
	p := map[string]float64{}
	for k := range testRules().Questions {
		p[k] = 0.1
	}
	p["adds"], p["about"], p["big"] = 0.9, 0.9, 0.8
	p["unseen"] = 0.95 // only matters when it is asked
	return p
}

// testRules is a rule set for tests: the shape of a real one (effort,
// block, need, a question that needs the front time, kinds with their
// minutes) under made-up names.
func testRules() *promptcheck.Rules {
	r := promptcheck.DefaultRules()
	r.Version = "test-1"
	r.Questions = map[string]string{
		"adds":   "Does the prompt add something of the person's own?",
		"choice": "Does the person choose how to build part of the project?",
		"about":  "Is the prompt about the project? The PROJECT section shows what the project is.",
		"again":  "Does the prompt ask again for something an earlier prompt asked?",
		"unseen": "Does the prompt report something the ACTIVITY shows no chance to see?",
		"big":    "Does the prompt ask for a lot of new code?",
	}
	r.Effort, r.Need, r.Block, r.Unbacked = []string{"adds", "choice"}, []string{"about"}, []string{"again", "unseen"}, "unseen"
	r.Kinds = []promptcheck.KindRule{{Name: "choice", Label: "choice", Minutes: 15}, {Name: "big", Label: "big", Minutes: 10}, {Name: "small", Minutes: 10}}
	return r
}

func testRulesJSON() string { return string(testRules().JSON()) }

// Planted text: none of it may reach the database or the log.
const (
	secretPrompt = "PROMPT-CANARY make the zorblax counter purple"
	secretReply  = "REPLY-CANARY I made the button blue"
	secretDiff   = "diff --git a/z.html b/z.html\n--- a/z.html\n+++ b/z.html\n@@ -1 +1 @@\n-<b>blue</b>\n+<b>DIFF-CANARY</b>\n"
	secretCtx    = "CONTEXT-CANARY an earlier prompt"
	secretName   = "NAME-CANARY"
	secretFiles  = "README.md\nsrc/TREE-CANARY.go"
	secretReadme = "# README-CANARY\nA zorblax counter."
)

func newCheckEnv(t *testing.T, on bool, j promptcheck.Judge) (*tenv, *bytes.Buffer, string) {
	t.Helper()
	db := storetest.DSN(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var logs bytes.Buffer
	var mu sync.Mutex
	srv, err := New(Config{Dev: true, BaseURL: "http://example.test", Secret: "test-secret",
		Admins: map[string]bool{}, PromptCheck: on, PromptCheckKey: "sk-or-TEST-KEY", PromptCheckRules: testRulesJSON()},
		st, slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	if on {
		srv.SetJudge(j)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &tenv{t, srv, ts, st}, &logs, db
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func checkReq(d *device, prompt string) api.PromptCheckReq {
	salt := promptcheck.NewSalt()
	return api.PromptCheckReq{DeviceID: d.id, CheckHash: promptcheck.Hash(salt, prompt), Salt: salt,
		Turn:    promptcheck.Turn{PrevReply: secretReply, Activity: promptcheck.Activity{Gap: 130 * time.Second, Input: -1, Front: -1}.String(), Prompt: prompt, Diff: secretDiff},
		Context: []promptcheck.Turn{{Prompt: secretCtx, Diff: promptcheck.NoChange, Activity: "start of session"}},
		Project: &promptcheck.Project{Name: secretName, Files: secretFiles, Total: 2, Readme: secretReadme},
		GitIns:  1, GitDel: 1, GitKnown: true}
}

func TestPromptCheckOffByDefault(t *testing.T) {
	if ConfigFromEnv().PromptCheck && os.Getenv("SEATIME_PROMPT_CHECK") != "1" {
		t.Fatal("the switch is on without SEATIME_PROMPT_CHECK=1")
	}
	e := newEnv(t) // the usual test config: switch not set
	d, _ := e.enroll("off")
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, checkReq(d, secretPrompt)); code != 404 {
		t.Fatalf("switch off: %d", code)
	}
	code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	var r api.SyncResp
	json.Unmarshal([]byte(out), &r)
	if code != 200 || r.PromptCheck {
		t.Fatalf("sync says the check is on: %s", out)
	}
	if pc, err := e.srv.promptChecks(context.Background(), d.id, &api.Bundle{}); pc != nil || err != nil {
		t.Fatal("verdicts are used while the switch is off")
	}
}

func TestPromptCheckKeepsLabelsNotText(t *testing.T) {
	j := &fakeJudge{p: passing()}
	e, logs, db := newCheckEnv(t, true, j)
	d, _ := e.enroll("tester")

	// the sync answer tells the device the check is on
	_, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil)
	var sr api.SyncResp
	json.Unmarshal([]byte(out), &sr)
	if !sr.PromptCheck {
		t.Fatalf("sync answer: %s", out)
	}

	// text that does not match the hash in the log is refused, unjudged
	bad := checkReq(d, secretPrompt)
	bad.Turn.Prompt += " (edited after the fact)"
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, bad); code != 422 || strings.Contains(string(out), "CANARY") {
		t.Fatalf("mismatched text: %d %s", code, out)
	}
	// an excerpt with more lines than git counted is refused
	big := checkReq(d, secretPrompt)
	big.Turn.Diff = "diff --git a/z b/z\n--- a/z\n+++ b/z\n@@ -0,0 +1,40 @@\n" + strings.Repeat("+DIFF-CANARY\n", 40)
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, big); code != 422 {
		t.Fatalf("excerpt past git's counts: %d", code)
	}
	// another device's id
	other := checkReq(d, secretPrompt)
	other.DeviceID = "someone-else"
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, other); code != 403 {
		t.Fatalf("device mismatch: %d", code)
	}
	if j.calls != 0 {
		t.Fatalf("the model was asked %d times about refused requests", j.calls)
	}

	req := checkReq(d, secretPrompt)
	code, out2 := e.apiDo("POST", "/api/v1/prompt-check", d.token, req)
	if code != 200 {
		t.Fatalf("check: %d %s", code, out2)
	}
	var resp api.PromptCheckResp
	json.Unmarshal(out2, &resp)
	if resp.CheckHash != req.CheckHash {
		t.Fatalf("%+v", resp)
	}
	// the device learns that the prompt was checked: not the decision, the
	// minutes, or what was asked and answered
	for _, w := range []string{"pass", "minutes", "rules", "labels", "kind", "model", "adds", "about", "big", "unseen", "liquid"} {
		if strings.Contains(string(out2), w) {
			t.Fatalf("the answer to the device names %q: %s", w, out2)
		}
	}
	// the model saw two turns and was asked only about the second
	if j.calls != 1 || !strings.Contains(j.states[0], "=== TURN 2 ===") || !strings.Contains(j.states[0], secretPrompt) || !strings.Contains(j.states[0], secretCtx) {
		t.Fatalf("state:\n%s", j.states[0])
	}
	// and read the project's section in front of them: name, files, README
	if !strings.HasPrefix(j.states[0], "=== PROJECT (") || !strings.Contains(j.states[0], "PROJECT NAME: "+secretName+"\nPROJECT FILES (2):\n"+secretFiles+"\nPROJECT README (its first lines):\n"+secretReadme+"\n=== END OF PROJECT ===") ||
		!strings.Contains(j.qs["about"].Instructions, "PROJECT section") {
		t.Fatalf("project section:\n%s", j.states[0])
	}
	// the front window was not measured: the question that needs it is not asked
	if _, asked := j.qs["unseen"]; asked || len(j.qs) != len(testRules().Questions)-1 || !strings.HasPrefix(j.qs["again"].Instructions, "About PROMPT 2 only: ") {
		t.Fatalf("questions %+v", j.qs)
	}
	// measured: it is asked, and its yes fails the prompt
	measured := checkReq(d, "PROMPT-CANARY the counter resets when I reload")
	measured.FrontMeasured = true
	measured.Turn.Activity = promptcheck.Activity{Gap: 130 * time.Second, Input: 20 * time.Second, Front: 5 * time.Second}.String()
	code, out3 := e.apiDo("POST", "/api/v1/prompt-check", d.token, measured)
	var resp3 api.PromptCheckResp
	json.Unmarshal(out3, &resp3)
	v3, _ := e.st.PromptVerdict(context.Background(), d.id, measured.CheckHash)
	if _, asked := j.qs["unseen"]; code != 200 || !asked || resp3.CheckHash != measured.CheckHash || v3 == nil || v3.Pass || !v3.Labels["unseen"] {
		t.Fatalf("measured: %d %+v %+v", code, resp3, v3)
	}
	// a retry gets the stored verdict; the model is not asked again
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, req); code != 200 || j.calls != 2 || !strings.Contains(string(out), req.CheckHash) {
		t.Fatalf("retry: %d %s, %d calls", code, out, j.calls)
	}
	// a device older than the project facts sends none and is judged all
	// the same; facts past the caps are cut, not refused
	oldDev := checkReq(d, "PROMPT-CANARY from a device that sends no project facts")
	oldDev.Project = nil
	if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, oldDev); code != 200 || j.calls != 3 || !strings.Contains(string(out), oldDev.CheckHash) ||
		!strings.HasPrefix(j.states[2], "=== PROJECT (") || !strings.Contains(j.states[2], "(the tracker sent no facts about the project)\n=== END OF PROJECT ===") || strings.Contains(j.states[2], "PROJECT NAME") {
		t.Fatalf("older device: %d %s\n%s", code, out, j.states[len(j.states)-1])
	}
	huge := checkReq(d, "PROMPT-CANARY from a project with a long file list")
	huge.Project.Files, huge.Project.Readme = strings.Repeat("src/TREE-CANARY/file.go\n", 2000), strings.Repeat("README-CANARY ", 2000)
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, huge); code != 200 || j.calls != 4 || len(j.states[3]) > len(j.states[0])+promptcheck.MaxTree+promptcheck.MaxReadme {
		t.Fatalf("long project facts: %d, state of %d bytes", code, len(j.states[len(j.states)-1]))
	}
	v, err := e.st.PromptVerdict(context.Background(), d.id, req.CheckHash)
	if err != nil || !v.Pass || v.Kind != "big" || v.Model != "liquid/d1.2-ultra-test" || len(v.Labels) != len(testRules().Questions)-1 || !v.Labels["adds"] || v.Labels["again"] ||
		len(v.Skipped) != 1 || v.Skipped[0] != "unseen" || v.Minutes != 10 || v.Rules != testRules().ID() {
		t.Fatalf("stored %+v %v", v, err)
	}

	// a failed call stores nothing; the device tries again later
	j.err = errors.New("upstream timeout")
	req2 := checkReq(d, "PROMPT-CANARY a second prompt")
	if code, _ := e.apiDo("POST", "/api/v1/prompt-check", d.token, req2); code != 503 {
		t.Fatalf("failed call: %d", code)
	}
	if _, err := e.st.PromptVerdict(context.Background(), d.id, req2.CheckHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a failed check stored a verdict")
	}

	// the count uses the stored verdicts: passing, and still waiting
	b := &api.Bundle{Entries: []chain.Entry{
		{Seq: 1, Event: &event.Event{Seq: 1, Kind: event.Prompt, CheckHash: req.CheckHash}},
		{Seq: 2, Event: &event.Event{Seq: 2, Kind: event.Prompt, CheckHash: req2.CheckHash}},
	}}
	pc, err := e.srv.promptChecks(context.Background(), d.id, b)
	if err != nil || !pc.On || !pc.Pass[req.CheckHash] || pc.Min[req.CheckHash] != 10 {
		t.Fatalf("%+v %v", pc, err)
	}
	if _, waiting := pc.Pass[req2.CheckHash]; waiting {
		t.Fatal("a prompt with no verdict has one")
	}

	// nothing of the text, and not the key, is in the database or the log
	e.st.Close()
	raw := storetest.Contents(t, db)
	if len(raw) == 0 {
		t.Fatal("read nothing of the database")
	}
	for _, leak := range []string{"CANARY", "zorblax", "sk-or-TEST-KEY"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("the database holds %q", leak)
		}
	}
	for _, leak := range []string{"CANARY", "zorblax", "sk-or-TEST-KEY"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("the log holds %q:\n%s", leak, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "prompt check") {
		t.Fatal("the check left no log line at all (numbers only are expected)")
	}
	// the log says whether a check came with project facts, never what they were
	if n, m := strings.Count(logs.String(), "project_facts=true"), strings.Count(logs.String(), "project_facts=false"); n != 3 || m != 1 {
		t.Fatalf("log lines with project facts %d, without %d; want 3 and 1:\n%s", n, m, logs.String())
	}
}

// A verdict that came back late may raise the synced minutes its prompt
// covers, though they are older than the sync floor; nothing else may.
func TestLateVerdictRaisesOldMinutes(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	d, _ := e.enroll("late")
	ctx := context.Background()
	covered, other := minute(120), minute(300)
	v := promptcheck.Verdict{Labels: promptcheck.Labels{}, Pass: true, Kind: "small", Model: "fake"}
	if err := e.st.PutPromptVerdict(ctx, d.id, h(7), v, covered.Add(-time.Minute), time.Now(), promptcheck.Evidence{}); err != nil {
		t.Fatal(err)
	}
	bs := []ledger.Bucket{{Project: "github.com/late/app", Start: covered, AISeconds: 60}, {Project: "github.com/late/app", Start: other, AISeconds: 60}}
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, bs, nil); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	u, _ := e.st.UserByName(ctx, "late")
	got, err := e.st.DeviceBuckets(ctx, u.ID, d.id, minute(400), minute(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Start.Equal(covered) {
		t.Fatalf("stored %+v; want only the minute the verdict covers", got)
	}
}

// Time waiting for the check shows on the profile and project pages as
// plain text; a newer report replaces the older one.
func TestPendingOnProfile(t *testing.T) {
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: passing()})
	d, br := e.enroll("wait")
	m := minute(3)
	bs := []ledger.Bucket{{Project: "github.com/wait/app", Start: m, AISeconds: 60}}
	meta := func(pend float64) func(*api.SyncReq) {
		return func(r *api.SyncReq) {
			r.Projects = []api.ProjectMeta{{Key: "github.com/wait/app", Name: "app", Remote: "https://github.com/wait/app"}}
			r.Pending = &api.PendingTime{Projects: []api.ProjectPending{{Project: "github.com/wait/app", Seconds: pend}}}
		}
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, bs, meta(12*60)); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	for _, path := range []string{"/@wait", "/@wait/app"} {
		if c, body := br.get(path); c != 200 || !strings.Contains(body, "12 min waiting on check") {
			t.Errorf("%s: %d, no pending line", path, c)
		}
	}
	d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, bs, meta(0))
	if _, body := br.get("/@wait"); strings.Contains(body, "waiting on check") {
		t.Error("the pending line stayed after the device reported none")
	}
	// with the switch off nothing is kept or shown
	off := newEnv(t)
	d2, br2 := off.enroll("offwait")
	d2.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, bs, meta(600))
	if _, body := br2.get("/@offwait"); strings.Contains(body, "waiting on check") {
		t.Error("pending shown with the switch off")
	}
}

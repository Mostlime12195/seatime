package server

import (
	"context"
	"testing"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// Two prompts of 900 words pass the check as the 15-minute kind, and the
// agent works for 20 minutes on each. The first came after 10 s of
// keyboard and mouse input: at 200 words a minute that is 33 words, so it
// was not typed, and it counts for the shortest kind's 5 minutes. The
// second came after 10 minutes of input and counts its 15. A reviewer
// reads both numbers and the mark. With the setting at 0 nothing is
// compared, and both count 15.
func TestPromptThatWasNotTypedCountsAsTheShortestKind(t *testing.T) {
	p := passing()
	p["choice"] = 0.9
	e, _, _ := newCheckEnv(t, true, &fakeJudge{p: p})
	ctx := context.Background()
	rules := testRules()
	rules.Version = "test-typed"
	rules.Kinds = []promptcheck.KindRule{{Name: "choice", Label: "choice", Minutes: 15}, {Name: "small", Minutes: 5}}
	if err := e.srv.setSetting(ctx, "prompt_check_rules", ptr(string(rules.JSON())), "boss"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Hour)
	now := t0
	e.srv.SetClock(func() time.Time { return now })
	d, _ := e.enroll("pat")
	key := "github.com/pat/zorblax"
	texts := []string{"a long plan that another program wrote", "a long plan the person typed"}
	pasted, salts := chained(0, promptcheck.ChainGenesis, key, t0.Add(10*time.Minute), texts[0])
	typed, salts2 := chained(1, pasted[0].Link, key, t0.Add(40*time.Minute), texts[1])
	es, salts := append(pasted, typed...), append(salts, salts2...)
	c := newRecChain(t, d, key)
	no := false
	ms := func(s int64) *int64 { v := s * 1000; return &v }
	for _, ev := range []event.Event{
		{TS: es[0].TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(texts[0]), Words: 900, New: &no, CheckHash: es[0].Commit,
			InputBeforeMS: ms(1), InputMS: ms(10), InputOverMS: 600000},
		{TS: t0.Add(30 * time.Minute), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s"},
		{TS: es[1].TS, Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: chain.Commit(texts[1]), Words: 900, New: &no, CheckHash: es[1].Commit,
			InputBeforeMS: ms(1), InputMS: ms(600), InputOverMS: 1800000},
		{TS: t0.Add(60 * time.Minute), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s"},
	} {
		c.add(ev)
		now = ev.TS.Add(time.Minute) // the daemon syncs as it goes
		if ev.Kind == event.Prompt {
			if code, _ := d.pushHashes(es[len(c.evs)/2 : len(c.evs)/2+1]); code != 200 {
				t.Fatal(code)
			}
		}
		c.push()
	}
	for i := range es {
		if code, out := e.apiDo("POST", "/api/v1/prompt-check", d.token, chainedReq(d, es[i], salts[i], texts[i])); code != 200 {
			t.Fatalf("check %d: %d %s", i, code, out)
		}
	}
	u, _ := e.st.UserByName(ctx, "pat")
	counted := func() float64 {
		t.Helper()
		e.count()
		proj, err := e.st.ProjectByKey(ctx, u.ID, key)
		if err != nil {
			t.Fatal(err)
		}
		sgs, err := e.st.ListSegments(ctx, u.ID, proj.ID)
		if err != nil || len(sgs) != 1 {
			t.Fatalf("segments: %+v %v", sgs, err)
		}
		return sgs[0].Scored.AISeconds
	}
	prompts := func() (out []api.ReviewRecord) {
		t.Helper()
		proj, err := e.st.ProjectByKey(ctx, u.ID, key)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := e.srv.reviewEvidence(ctx, u, proj, t0.Add(-time.Hour), t0.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, sg := range ev.Segments {
			for _, r := range sg.Records {
				if r.Kind == event.Prompt {
					out = append(out, r)
				}
			}
		}
		return out
	}
	// as shipped, a prompt that was not typed is only marked
	now = t0.Add(90 * time.Minute)
	if ai := counted(); ai != (15+15)*60 {
		t.Fatalf("with the cut off: %v s of AI time, want 15 + 15 minutes", ai)
	}
	if rs := prompts(); len(rs) != 2 || !rs[0].NotTyped || rs[0].Check == nil || rs[0].Check.Minutes != 15 || rs[1].NotTyped {
		t.Fatalf("with the cut off, a reviewer reads: %+v", rs)
	}
	if err := e.srv.setSetting(ctx, "prompt_typed_cut", ptr("on"), "boss"); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(2 * time.Hour)
	if ai := counted(); ai != (5+15)*60 {
		t.Fatalf("counted %v s of AI time, want 5 + 15 minutes", ai)
	}
	rs := prompts()
	if len(rs) != 2 || !rs[0].NotTyped || rs[0].Check == nil || rs[0].Check.Minutes != 5 || rs[0].InputMS == nil || *rs[0].InputMS != 10000 || rs[0].InputOverMS != 600000 {
		t.Fatalf("the prompt that was not typed, as a reviewer reads it: %+v", rs)
	}
	if rs[1].NotTyped || rs[1].Check == nil || rs[1].Check.Minutes != 15 || *rs[1].InputMS != 600000 {
		t.Fatalf("the typed prompt, as a reviewer reads it: %+v", rs[1])
	}
	// the stored verdict keeps the minutes the check gave
	if v, err := e.st.PromptVerdict(ctx, d.id, es[0].Commit); err != nil || v.Minutes != 15 {
		t.Fatalf("stored verdict: %+v %v", v, err)
	}
	if err := e.srv.setSetting(ctx, "prompt_typed_wpm", ptr("0"), "boss"); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(3 * time.Hour)
	if ai := counted(); ai != (15+15)*60 {
		t.Fatalf("with the setting at 0: %v s of AI time, want 15 + 15 minutes", ai)
	}
	if rs := prompts(); rs[0].NotTyped || rs[0].Check.Minutes != 15 {
		t.Fatalf("with the setting at 0, a reviewer reads: %+v", rs[0])
	}
	if err := e.srv.setSetting(ctx, "prompt_typed_wpm", ptr("-3"), "boss"); err == nil {
		t.Fatal("a negative typing speed was taken")
	}
}

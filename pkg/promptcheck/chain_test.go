package promptcheck

import (
	"testing"
	"time"
)

func TestChainLinkCoversEveryField(t *testing.T) {
	e := ChainEntry{Seq: 1, TS: time.UnixMilli(1700000000123), Project: "local:abc", Commit: Hash("salt", "text")}
	base := ChainLink(ChainGenesis, e)
	for name, f := range map[string]func(*ChainEntry){
		"seq":     func(c *ChainEntry) { c.Seq = 2 },
		"time":    func(c *ChainEntry) { c.TS = c.TS.Add(time.Millisecond) },
		"project": func(c *ChainEntry) { c.Project = "local:abd" },
		"commit":  func(c *ChainEntry) { c.Commit = Hash("salt", "text!") },
		"repeat":  func(c *ChainEntry) { c.Repeat = true },
	} {
		c := e
		f(&c)
		if ChainLink(ChainGenesis, c) == base {
			t.Errorf("the link does not cover %s", name)
		}
	}
	if ChainLink(Hash("x", "y"), e) == base {
		t.Error("the link does not cover the previous link")
	}
	if Hash("salt1", "text") == Hash("salt2", "text") {
		t.Error("the salt does not change the hash")
	}
}

func TestVerifyAndRelink(t *testing.T) {
	var es []ChainEntry
	seq, link := int64(0), ChainGenesis
	for i := 0; i < 4; i++ {
		e := ChainEntry{Seq: seq + 1, TS: time.UnixMilli(int64(1e12) + int64(i)), Project: "p", Commit: Hash("s", string(rune('a'+i)))}
		e.Link = ChainLink(link, e)
		es, seq, link = append(es, e), e.Seq, e.Link
	}
	if err := VerifyChain(0, ChainGenesis, es); err != nil {
		t.Fatal(err)
	}
	sw := []ChainEntry{es[0], es[2], es[1], es[3]}
	if VerifyChain(0, ChainGenesis, sw) == nil {
		t.Fatal("a reordered chain verified")
	}
	moved := Relink(7, Hash("x", "y"), es)
	if moved[0].Seq != 8 || VerifyChain(7, Hash("x", "y"), moved) != nil {
		t.Fatalf("relink: %+v", moved)
	}
}

func TestEffective(t *testing.T) {
	at := func(m int) time.Time { return time.Unix(0, 0).Add(time.Duration(m) * time.Minute).UTC() }
	server := []Period{{From: at(0), On: true}}
	user := []Period{{From: at(10), On: false}, {From: at(20), On: true}}
	var asked [][2]time.Time
	resolve := func(n, settled int) Resolver {
		return func(a, b time.Time) (int, int) { asked = append(asked, [2]time.Time{a, b}); return n, settled }
	}
	on := func(ps []Period, m int) bool { return (&Set{Periods: ps}).OnAt(at(m)) }

	// no chain: the off stretch stays off
	ps := Effective(server, true, user, time.Time{}, resolve(1, 1))
	if !on(ps, 5) || on(ps, 15) || !on(ps, 25) || len(asked) != 0 {
		t.Fatalf("no chain: %+v", ps)
	}
	// a chain from minute 12: [10,12) stays off; [12,20) switches when all settled
	ps = Effective(server, true, user, at(12), resolve(3, 3))
	if on(ps, 11) || !on(ps, 13) || !on(ps, 25) || len(asked) != 1 || !asked[0][0].Equal(at(12)) || !asked[0][1].Equal(at(20)) {
		t.Fatalf("settled: %+v %v", ps, asked)
	}
	if ps = Effective(server, true, user, at(12), resolve(3, 2)); on(ps, 13) {
		t.Fatalf("half settled: %+v", ps)
	}
	if ps = Effective(server, true, user, at(12), resolve(0, 0)); on(ps, 13) {
		t.Fatalf("no prompts: %+v", ps)
	}
	// the server's switch off: nothing applies, whatever the account says
	if ps = Effective([]Period{{From: at(0), On: false}}, false, nil, at(0), resolve(1, 1)); on(ps, 5) {
		t.Fatalf("server off: %+v", ps)
	}
	// no account history: the server's own
	if ps = Effective(server, true, nil, time.Time{}, nil); !on(ps, 5) || len(ps) != 1 {
		t.Fatalf("default on: %+v", ps)
	}
	// still off now: the open stretch switches once all of it is settled
	asked = nil
	if ps = Effective(server, true, user[:1], at(10), resolve(2, 2)); !on(ps, 1e6) || !asked[0][1].Equal(Forever) {
		t.Fatalf("open stretch: %+v", ps)
	}
}

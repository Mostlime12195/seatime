package ledger

import "testing"

// The TestOneTurn day in minute buckets: 8 one-minute buckets of AI from
// 10:00 to 10:08 and 2 min from 10:10, the capped gap before the 10:12
// prompt, also AI. They add up to the result: 8 + 2 = 10 min.
func TestBucketsAddUpToTheResult(t *testing.T) {
	l := oneTurn()
	r := count(l)
	bs := Buckets(r, l)
	s := Sum(bs, A)
	if s.AISeconds != 10*60 || s.CodingSeconds != 0 || s.Prompts != 2 {
		t.Fatalf("sum %+v, want 600 s AI, 0 s hand, 2 prompts", s)
	}
	for _, b := range bs {
		if b.Start.Unix()%60 != 0 || b.Seconds() > 60 {
			t.Errorf("bad bucket %+v", b)
		}
	}
}

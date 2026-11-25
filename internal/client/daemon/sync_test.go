package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/syncer"
)

func noJitter(int64) int64 { return 0 }

// Server down or slow: exponential backoff with jitter, never more than
// 10 minutes; success resets it.
func TestSyncBackoffCapsAtTenMinutes(t *testing.T) {
	down := errors.New("dial tcp: connection refused")
	fails := 0
	var waits []time.Duration
	for i := 0; i < 10; i++ {
		var w time.Duration
		w, fails = syncWait(down, fails, noJitter)
		waits = append(waits, w)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] < waits[i-1] {
			t.Fatalf("backoff shrank: %v", waits)
		}
	}
	if waits[len(waits)-1] > 10*time.Minute || waits[0] < 90*time.Second {
		t.Fatalf("waits %v", waits)
	}
	if w, f := syncWait(nil, fails, noJitter); w != 90*time.Second || f != 0 {
		t.Fatal("success must reset the backoff")
	}
	// jitter stays inside +-20%
	for i := 0; i < 50; i++ {
		w, _ := syncWait(down, 9, func(n int64) int64 { return n - 1 })
		if w > 10*time.Minute || w < 8*time.Minute {
			t.Fatalf("jittered wait %v", w)
		}
	}
}

func TestSyncErrorsOnlyTheUserCanFix(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&syncer.HTTPError{Status: 401}, "seatime login"},
		{&syncer.HTTPError{Status: 409, Body: "checkpoint seq is behind"}, "different history"},
		{&syncer.HTTPError{Status: 503}, ""},
		{errors.New("timeout"), ""},
	} {
		msg := blockedMessage(c.err)
		if c.want == "" && msg != "" || c.want != "" && !strings.Contains(msg, c.want) {
			t.Errorf("%v: %q", c.err, msg)
		}
		if c.want != "" {
			if w, _ := syncWait(c.err, 0, noJitter); w != time.Hour {
				t.Errorf("%v: waits %v, should not hammer the server", c.err, w)
			}
		}
	}
	if w, _ := syncWait(syncer.ErrNotLoggedIn, 3, noJitter); w != 5*time.Minute {
		t.Fatal(w)
	}
}

func TestSleepWallWaitsAndStops(t *testing.T) {
	start := time.Now()
	if woke := sleepWall(context.Background(), 120*time.Millisecond, 30*time.Millisecond); woke {
		t.Fatal("no sleep happened")
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepWall(ctx, time.Hour, time.Second) {
		t.Fatal("cancelled")
	}
}

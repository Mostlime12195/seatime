package store

import (
	"context"
	"seatime/internal/server/store/storetest"
	"testing"
	"time"
)

// The Hackatime trust table round-trips.
func TestTrustTable(t *testing.T) {
	s, err := Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	if err := s.PutHackatimeTrust(ctx, HackatimeTrust{SlackID: "U0RED0001", Level: "red", Value: 1, Found: true, Checked: now}); err != nil {
		t.Fatal(err)
	}
	if tr, err := s.HackatimeTrust(ctx, "U0RED0001"); err != nil || tr.Level != "red" || !tr.Checked.Equal(now) {
		t.Fatalf("trust: %+v %v", tr, err)
	}
}

package store

import (
	"context"
	"testing"
	"time"

	"seatime/internal/server/store/storetest"
)

// Migration 9 on a database that holds segments devices uploaded: they
// stay as they are (not the server's own, not open), and the records and
// the count cycle start empty.
func TestMigration9KeepsUploadedSegments(t *testing.T) {
	dsn := storetest.DSN(t)
	all := migrations
	migrations = all[:8]
	s, err := Open(dsn)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "m9"}, "lia", "", "", "", "")
	if err := s.CreateDevice(ctx, Device{ID: "mac", UserID: u.ID, Name: "mac", PublicKey: "k"}, "tok-mac"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Hour)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(user_id,key,slug,created) VALUES (?,?,?,?)`, u.ID, "github.com/lia/app", "app", t0.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var pid int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE user_id=?`, u.ID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO segments(id,user_id,project_id,device_id,status,problems,claimed,scored,seq_lo,seq_hi,score_ver,first_ts,last_ts,pending,created,checked)
  VALUES ('old1',?,?,'mac','verified','[]','{}','{}',1,40,'heartbeat/2',?,?,0,?,?)`, u.ID, pid, t0.UnixMilli(), t0.Add(time.Hour).UnixMilli(), t0.Add(4*time.Hour).UnixMilli(), t0.Add(4*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO segment_blobs(id,data) VALUES ('old1',?)`, []byte("gz")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO segment_minutes(segment_id,user_id,project_id,start_min,human,dark) VALUES ('old1',?,?,?,50,0)`, u.ID, pid, t0.Unix()/60); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if s, err = Open(dsn); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sg, err := s.Segment(ctx, "old1")
	if err != nil || sg.Stream || sg.Open || sg.RecKey != "" || !sg.Since.IsZero() || !sg.Until.IsZero() || sg.Status != SegVerified || sg.SeqHi != 40 {
		t.Fatalf("the uploaded segment after the migration: %+v %v", sg, err)
	}
	if ms, err := s.VerifiedMinutes(ctx, pid, t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil || len(ms) != 1 || ms[0].Human != 50 {
		t.Fatalf("its minutes: %+v %v", ms, err)
	}
	if segs, err := s.StreamSegments(ctx, "mac", false); err != nil || len(segs) != 0 {
		t.Fatalf("server-made segments: %+v %v", segs, err)
	}
	if _, err := s.RecordStream(ctx, "mac"); err != ErrNotFound {
		t.Fatalf("a stream before any records: %v", err)
	}
	// the cycle's turn: one at a time, given back when a mark is done
	now := time.Now()
	if done, err := s.CountDone(ctx); err != nil || done.Unix() != 0 {
		t.Fatalf("count done: %v %v", done, err)
	}
	if ok, err := s.CountClaim(ctx, now, 10*time.Minute); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, _ := s.CountClaim(ctx, now.Add(time.Minute), 10*time.Minute); ok {
		t.Fatal("a second server took the turn while the first holds it")
	}
	if ok, _ := s.CountClaim(ctx, now.Add(11*time.Minute), 10*time.Minute); !ok {
		t.Fatal("the turn of a server that stopped was not taken over")
	}
	mark := now.UTC().Truncate(time.Hour)
	if err := s.SetCountDone(ctx, mark); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.CountDone(ctx); !done.Equal(mark) {
		t.Fatalf("count done: %v, want %v", done, mark)
	}
	if ok, _ := s.CountClaim(ctx, now.Add(12*time.Minute), 10*time.Minute); !ok {
		t.Fatal("the turn was not given back")
	}
	// records: kept once, in order, with a hole starting a new run
	rec := func(seq int64) Record {
		return Record{Seq: seq, TS: t0.Add(time.Duration(seq) * time.Minute), Project: "github.com/lia/app", Kind: "edit", Leaf: "l", Prev: "p", Data: `{"kind":"edit"}`}
	}
	if err := s.AddRecords(ctx, "mac", u.ID, []Record{rec(41), rec(42)}, t0.Add(41*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRecords(ctx, "mac", u.ID, []Record{rec(42), rec(43)}, t0.Add(42*time.Minute), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	st, err := s.RecordStream(ctx, "mac")
	if err != nil || st.Head != 43 || st.RunLo != 41 || !st.Dirty.Equal(t0.Add(41*time.Minute)) {
		t.Fatalf("stream: %+v %v", st, err)
	}
	if err := s.ClearDirty(ctx, "mac", now); err != nil { // read before the second batch came
		t.Fatal(err)
	}
	if ds, _ := s.DirtyStreams(ctx); len(ds) != 1 {
		t.Fatal("a stream that got records while it was counted was marked counted")
	}
	if err := s.ClearDirty(ctx, "mac", st.Received); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.DirtyStreams(ctx); len(ds) != 0 {
		t.Fatalf("still dirty: %+v", ds)
	}
	if err := s.AddRecords(ctx, "mac", u.ID, []Record{rec(50)}, t0.Add(50*time.Minute), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.RecordStream(ctx, "mac"); st.Head != 50 || st.RunLo != 50 {
		t.Fatalf("after a hole: %+v", st)
	}
	rows, err := s.RecordRows(ctx, "mac", 1, 100)
	if err != nil || len(rows) != 4 {
		t.Fatalf("%d records (%v)", len(rows), err)
	}
	if ts, err := s.RecordTimes(ctx, "mac", "github.com/lia/app", 41, t0.Add(42*time.Minute), t0.Add(50*time.Minute)); err != nil || len(ts) != 2 || ts[0].Seq != 42 {
		t.Fatalf("record times: %+v %v", ts, err)
	}
	// a stretch that is open when the records go on after a hole is closed
	// at the last record the server holds of it, and a count that read it
	// as open cannot store it again
	open := &Segment{ID: "st1", UserID: u.ID, ProjectID: pid, DeviceID: "mac", Status: SegVerified, Stream: true, Open: true, RecKey: "github.com/lia/app",
		SeqLo: 41, SeqHi: 42, FirstTS: t0.Add(41 * time.Minute), LastTS: t0.Add(42 * time.Minute), Since: t0, Created: now, Checked: now}
	if err := s.PutSegment(ctx, open, []byte("gz"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseStreamSegments(ctx, "mac"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Segment(ctx, "st1"); got.Open || !got.Until.Equal(t0.Add(50*time.Minute).Add(time.Millisecond)) {
		t.Fatalf("closed at %v (open %v), want just past the last record held", got.Until, got.Open)
	}
	if err := s.ReplaceSegment(ctx, open, true, []byte("gz"), nil); err != ErrConflict {
		t.Fatalf("a stale open stretch was stored again: %v", err)
	}
	open.Open, open.Until = false, t0.Add(51*time.Minute)
	if err := s.ReplaceSegment(ctx, open, false, []byte("gz2"), nil); err != nil {
		t.Fatal(err)
	}
	// deleting the account takes the records with it
	if err := s.DeleteAccount(ctx, u.ID, nil, now); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.RecordRows(ctx, "mac", 1, 100); len(rows) != 0 {
		t.Fatalf("%d records left after the account was deleted", len(rows))
	}
	if _, err := s.RecordStream(ctx, "mac"); err != ErrNotFound {
		t.Fatalf("the stream after the account was deleted: %v", err)
	}
}

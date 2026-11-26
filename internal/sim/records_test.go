package sim

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/segment"
	srvstore "seatime/internal/server/store"
	"seatime/pkg/api"
)

// The real client sends its records after each sync, and the server counts
// them at its cycles: while the work goes on, and after the computer is
// gone. What it counts is what the device would have claimed for the same
// records in a segment of its own, and every record is in one stretch.
func TestRecordsFromADeviceAreCountedAtTheCycle(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("lia", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	d.Daemon.Auth = &d.Auth
	ctx := context.Background()
	u, _ := w.Store.UserByName(ctx, "lia")
	srv := serverSide{t, w, u}
	// the daemon's loop: a sync, then the records behind its checkpoint
	sent := 0
	stop := w.Every(90*time.Second, func() {
		if err := d.SyncNow(); err != nil {
			t.Errorf("sync: %v", err)
		}
		n, err := d.Daemon.RecordsOnce(ctx)
		if err != nil {
			t.Errorf("records: %v", err)
		}
		sent += n
	})
	d.SyncEvery = 0 // the loop above syncs
	cycle := func() int {
		t.Helper()
		n, err := w.Srv.CountStreams(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	a := NewActor(d, 11).In(d.Project("", "snake"))
	beginnerPython(a) // about 40 minutes with an AI tool and by hand
	w.Advance(3 * time.Minute)
	head, _, _ := d.Store.Head()
	if sent == 0 || int64(sent) != head {
		t.Fatalf("sent %d records of %d", sent, head)
	}
	ps := srv.projects()
	if len(ps) != 1 {
		t.Fatalf("projects: %+v", ps)
	}
	if segs, _ := w.Store.ListSegments(ctx, u.ID, 0); len(segs) != 0 {
		t.Fatalf("%d segments before any cycle", len(segs))
	}
	// the cycle, while the stretch is nowhere near over
	if n := cycle(); n != 1 {
		t.Fatalf("the cycle counted %d devices", n)
	}
	segs, _ := w.Store.ListSegments(ctx, u.ID, ps[0].ID)
	if len(segs) != 1 || !segs[0].Stream || !segs[0].Open || segs[0].Status != srvstore.SegVerified {
		t.Fatalf("after the cycle: %+v", segs)
	}
	// the same records as a segment of the device's own: the same count
	ss, _ := d.Store.SyncState()
	key := ProjectKey("lia", "snake")
	prep, err := segment.Build(d.Store, d.Auth, segment.Options{Project: key, MaxSeq: ss.Seq})
	if err != nil {
		t.Fatal(err)
	}
	got, want := segs[0].Scored, prep.Summary.Claimed
	if math.Abs(got.CodingSeconds-want.CodingSeconds) > 1 || math.Abs(got.AISeconds-want.AISeconds) > 1 || got.Seconds() < 20*60 {
		t.Fatalf("the server counted %.0f s by hand and %.0f s of AI; the device's own segment would claim %.0f and %.0f",
			got.CodingSeconds, got.AISeconds, want.CodingSeconds, want.AISeconds)
	}
	st := srv.stats("snake")
	if math.Abs(st.HumanSeconds-got.Seconds()) > 1 {
		t.Fatalf("an application reads %.0f s, the stretch holds %.0f", st.HumanSeconds, got.Seconds())
	}

	// the computer is closed. Hours later the stretch is over: the server
	// closes it with no word from the device
	stop()
	w.Advance(4 * time.Hour)
	if n := cycle(); n != 1 {
		t.Fatalf("the cycle counted %d devices", n)
	}
	segs, _ = w.Store.ListSegments(ctx, u.ID, ps[0].ID)
	if len(segs) != 1 || segs[0].Open || segs[0].Status != srvstore.SegVerified || math.Abs(segs[0].Scored.Seconds()-got.Seconds()) > 1 {
		t.Fatalf("after the stretch was over: %+v", segs)
	}
	if st := srv.stats("snake"); st.Status != api.StatusVerified {
		t.Fatalf("status %s (%s)", st.Status, st.StatusReason)
	}
	if n := cycle(); n != 0 {
		t.Fatalf("a cycle with nothing new counted %d devices", n)
	}
}

// A device that uploaded its stretches itself, then updates: its records
// go up from the first one no segment of its own covers, and the server
// counts from there. Nothing is counted twice and nothing is left out.
func TestRecordsTakeOverFromUploadedSegments(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("lia", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	d.Daemon.Auth = &d.Auth
	ctx := context.Background()
	u, _ := w.Store.UserByName(ctx, "lia")
	srv := serverSide{t, w, u}
	root := filepath.Join(d.Home, "code", "app")
	gitFolder(d, root, "")
	// the older SeaTime: a stretch, uploaded once it is over
	handWork(d, root, 10)
	w.Advance(3 * time.Hour)
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Daemon.SegmentsOnce(ctx, w.Now()); err != nil || n != 1 {
		t.Fatalf("segments: %d (%v)", n, err)
	}
	// work goes on, and SeaTime updates in the middle of it
	handWork(d, root, 10)
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	first, _, _ := d.Store.Head()
	n, err := d.Daemon.RecordsOnce(ctx)
	if err != nil || n == 0 || int64(n) >= first {
		t.Fatalf("records sent: %d of %d (%v)", n, first, err)
	}
	handWork(d, root, 10)
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Daemon.RecordsOnce(ctx); err != nil || n == 0 {
		t.Fatalf("records sent: %d (%v)", n, err)
	}
	if n, err := d.Daemon.RecordsOnce(ctx); err != nil || n != 0 {
		t.Fatalf("records sent twice: %d (%v)", n, err)
	}
	if _, err := w.Srv.CountStreams(ctx, nil); err != nil {
		t.Fatal(err)
	}
	ps := srv.projects()
	segs, _ := w.Store.ListSegments(ctx, u.ID, ps[0].ID)
	if len(segs) != 2 || segs[0].Stream || !segs[1].Stream || !segs[1].FirstTS.After(segs[0].LastTS) {
		t.Fatalf("segments: %+v", segs)
	}
	for i, sg := range segs {
		if sg.Status != srvstore.SegVerified {
			t.Errorf("segment %d: %s %v", i, sg.Status, sg.Problems)
		}
	}
	st := srv.stats("app")
	if total := srv.counted(ps[0].ID); math.Abs(st.HumanSeconds-total) > 1 || total < 25*60 {
		t.Errorf("checked %.0f s of %.0f counted", st.HumanSeconds, total)
	}
}

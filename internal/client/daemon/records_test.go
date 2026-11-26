package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/records"
	"seatime/internal/client/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
)

// A server that has never taken this device's records and does not know
// the address is an older one: the daemon then uploads stretches itself
// (segmentLoop). Once a server takes records, it never does again.
func TestOlderServerGetsSegments(t *testing.T) {
	t.Setenv("SEATIME_HOME", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.TouchProject(store.Project{Key: "local:app", Name: "app"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "local:app", TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	seq, _, _ := st.Head()
	st.SetSyncState(seq, time.Now())
	takes := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !takes || r.URL.Path != "/api/v1/records" {
			http.NotFound(w, r)
			return
		}
		var req api.RecordsReq
		json.NewDecoder(r.Body).Decode(&req)
		resp := api.RecordsResp{Next: seq}
		if len(req.Entries) > 0 {
			resp = api.RecordsResp{Head: seq}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()
	d := &Daemon{Store: st, Version: "test", Auth: &config.Auth{DeviceID: "dev1", Token: "tok", Server: ts.URL}}
	ctx := context.Background()
	old := func() string { v, _ := st.GetMeta(metaOldServer); return v }
	if _, err := d.RecordsOnce(ctx); !errors.Is(err, records.ErrUnsupported) || old() != "1" {
		t.Fatalf("older server: %v, flag %q", err, old())
	}
	takes = true
	if n, err := d.RecordsOnce(ctx); err != nil || n != 1 || old() != "" {
		t.Fatalf("a server that takes records: sent %d (%v), flag %q", n, err, old())
	}
	takes = false // it is rolled back: the device waits; no segments beside the records
	if _, err := d.RecordsOnce(ctx); err != nil || old() != "" {
		t.Fatalf("after records were taken: %v, flag %q", err, old())
	}
	st.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "local:app", TS: time.Now()})
	seq, _, _ = st.Head()
	st.SetSyncState(seq, time.Now())
	if _, err := d.RecordsOnce(ctx); !errors.Is(err, records.ErrUnsupported) || old() != "" {
		t.Fatalf("rolled back with records to send: %v, flag %q", err, old())
	}
}

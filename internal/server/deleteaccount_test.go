package server

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/ledger"
)

// An owner deletes their account from Settings after typing the username.
// Everything of theirs goes at once; a computer that can remove SeaTime is
// asked to (with its data) and may only check in until it has; an older
// one is disconnected. Once the last computer is done, the account row
// goes too, and signing in again starts a new, empty account. Other
// accounts are untouched.
func TestDeleteAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d, br := e.enroll("zoe")
	old, _ := e.enroll("zoe") // a second computer, with an older SeaTime that never checks in
	other, _ := e.enroll("lou")
	d.checkIn(t, api.CheckIn{Can: allCan})
	for _, x := range []*device{d, other} {
		if code, out := x.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, []ledger.Bucket{{Project: "p", Start: minute(1), CodingSeconds: 30}}, nil); code != 200 {
			t.Fatalf("sync: %d %s", code, out)
		}
	}
	zoe, err := e.st.UserByName(ctx, "zoe")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := br.get("/@zoe"); code != 200 {
		t.Fatalf("profile before: %d", code)
	}
	tok := br.csrf("/settings")

	if _, body := br.post("/settings/delete-account", url.Values{"csrf": {tok}, "confirm": {"zo"}}); !strings.Contains(body, "Type your username to delete your account.") {
		t.Fatal("a wrong username deleted or did not explain")
	}
	if _, err := e.st.UserByName(ctx, "zoe"); err != nil {
		t.Fatal("deleted without the username")
	}

	code, body := br.post("/settings/delete-account", url.Values{"csrf": {tok}, "confirm": {"@Zoe"}})
	if code != 200 || !strings.Contains(body, "Your SeaTime account is deleted.") {
		t.Fatalf("delete: %d", code)
	}
	if _, page := br.get("/settings"); strings.Contains(page, "Delete my SeaTime account") {
		t.Fatal("still signed in after deleting")
	}
	if code, _ := br.get("/@zoe"); code != 404 {
		t.Fatalf("profile after: %d", code)
	}
	if code, _ := other.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, nil, nil); code != 200 {
		t.Fatalf("another account was touched: %d", code)
	}
	if code, _ := old.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil); code != 401 {
		t.Fatalf("the older computer still syncs: %d", code)
	}
	if code, _ := d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, nil, nil); code != 403 {
		t.Fatalf("a computer of a deleted account still syncs: %d", code)
	}
	if p, err := e.st.ListProjects(ctx, zoe.ID); err != nil || len(p) != 0 {
		t.Fatalf("projects left: %v %v", p, err)
	}

	r := d.checkIn(t, api.CheckIn{Can: allCan})
	if len(r.Requests) != 1 || r.Requests[0].Kind != api.RequestUninstall || !r.Requests[0].Purge {
		t.Fatalf("requests: %+v", r.Requests)
	}
	if code, out := e.apiDo("POST", "/api/v1/device/requests/"+strconv.FormatInt(r.Requests[0].ID, 10), d.token, api.RequestResult{DeviceID: d.id, OK: true}); code != 200 {
		t.Fatalf("result: %d %s", code, out)
	}
	if _, err := e.st.UserByID(ctx, zoe.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the account row is still there: %v", err)
	}

	br2 := e.browser()
	br2.login("zoe", "user")
	again, err := e.st.UserByName(ctx, "zoe")
	if err != nil || again.ID == zoe.ID {
		t.Fatalf("signing in again: %+v %v", again, err)
	}
	if p, _ := e.st.ListProjects(ctx, again.ID); len(p) != 0 {
		t.Fatal("the new account has the old projects")
	}
}

// A deleted account whose computers never come back is removed after the
// grace period.
func TestDeletedAccountPurgedAfterGrace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d, br := e.enroll("amy")
	d.checkIn(t, api.CheckIn{Can: allCan})
	amy, _ := e.st.UserByName(ctx, "amy")
	br.post("/settings/delete-account", url.Values{"csrf": {br.csrf("/settings")}, "confirm": {"amy"}})
	if n, err := e.st.PurgeDeleted(ctx, time.Now().Add(-deletedGrace)); err != nil || n != 0 {
		t.Fatalf("purged early: %d %v", n, err)
	}
	if _, err := e.st.UserByID(ctx, amy.ID); err != nil {
		t.Fatal("the row went before its computer removed SeaTime")
	}
	if n, err := e.st.PurgeDeleted(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("not purged after the grace period: %d %v", n, err)
	}
	if code, _ := e.apiDo("POST", "/api/v1/device/check-in", d.token, api.CheckIn{DeviceID: d.id}); code != 401 {
		t.Fatalf("its computer still checks in: %d", code)
	}
}

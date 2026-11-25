package server

// Security regression tests: each one pins a fixed behaviour of the web and
// API layer (role binding, audit access, hidden projects, rate limits, cache
// bounds, oversized segments).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// sendLocal syncs and uploads a small verified "local:" project as one
// segment and returns its id.
func sendLocal(t *testing.T, e *tenv, d *device, project string) string {
	t.Helper()
	evs, heads := localChain(t, project)
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: heads[len(heads)-1]}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, d.localBundle(evs, project))
	var r api.SegmentResp
	json.Unmarshal(out, &r)
	if code != 200 || r.Status != api.StatusVerified {
		t.Fatalf("segment: %d %s", code, out)
	}
	return r.ID
}

// fixed: SEATIME_ADMINS matched the sanitized username, and usernames are
// first-come and derived from user-chosen strings (HCA nickname, GitHub
// login): "ALICE!!" signing up before the real admin got "alice" and the
// admin role. Roles now bind to verified login identities
// (SEATIME_ADMIN_IDS, "provider:subject"); a bare username never matches.
// Dev login stands in for HCA/GitHub (finishLogin is shared), posted with
// role=user so only the configured identity can grant the role.
func TestAdminRoleBindsToLoginIdentity(t *testing.T) {
	e := newEnv(t)
	// a bare username is refused outright
	if err := e.srv.setSetting(context.Background(), "admin_ids", ptr("alice,dev:alice"), "boss"); err == nil {
		t.Fatal("a bare username was accepted as an admin id")
	}
	if err := e.srv.setSetting(context.Background(), "admin_ids", ptr("dev:alice"), "boss"); err != nil { // the real admin's login identity
		t.Fatal(err)
	}

	atk := e.browser()
	atk.login("ALICE!!", "user") // identity dev:alice!!, username sanitizes to "alice"
	u, err := e.st.UserByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != store.RoleUser {
		t.Fatalf("the squatter was promoted: role %q", u.Role)
	}
	if code, _ := atk.get("/admin"); code != 404 {
		t.Fatalf("squatter reached /admin: %d", code)
	}
	// the real admin, arriving later, gets "alice-2" and the role by identity
	real := e.browser()
	real.login("alice", "user")
	if r2, err := e.st.UserByName(context.Background(), "alice-2"); err != nil || r2.Role != store.RoleAdmin {
		t.Fatalf("real admin: %+v %v", r2, err)
	}
	if code, _ := real.get("/admin"); code != 200 {
		t.Fatalf("real admin can't open /admin: %d", code)
	}

	got := idSet(" hca:abc-123, GitHub:42 , alice, dev:ALICE, :x, github: ")
	want := map[string]bool{"hackclub:abc-123": true, "github:42": true, "dev:alice": true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("idSet: %v, want %v", got, want)
	}
}

// A project the owner hid in /settings stays out of every public answer
// (the profile page, the Hackatime-compatible project list, its export).
func TestHiddenProjectStaysHidden(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("dan")
	sendLocal(t, e, d, "local:x")
	// the client names its project in a sync meta (names are what users see)
	if code, out := d.sync(chain.Checkpoint{Seq: 1000, Hash: h(1000)}, nil, func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{{Key: api.HashedProjectKey("local:x"), Name: "secret-thing"}}
	}); code != 200 {
		t.Fatal(code, out)
	}
	if code, out := e.apiDo("GET", "/api/v1/users/dan/projects", "", nil); code != 200 || !strings.Contains(string(out), "secret-thing") {
		t.Fatalf("visible project missing from the list: %d %s", code, out)
	}
	ctx := context.Background()
	u, _ := e.st.UserByName(ctx, "dan")
	ps, _ := e.st.ListProjects(ctx, u.ID)
	if len(ps) == 0 {
		t.Fatal("no project")
	}
	tok := br.csrf("/settings")
	for _, p := range ps {
		if c, body := br.post("/settings/project", url.Values{"csrf": {tok}, "id": {fmt.Sprint(p.ID)}, "hidden": {"1"}}); c != 200 {
			t.Fatalf("hide: %d %s", c, body)
		}
	}
	// the profile page hides it from strangers...
	_, page := e.browser().get("/@dan")
	if strings.Contains(page, "secret-thing") {
		t.Fatalf("profile leaks hidden project")
	}
	// ...and so does the API now
	code, out := e.apiDo("GET", "/api/v1/users/dan/projects", "", nil)
	if code != 200 || strings.Contains(string(out), "secret-thing") {
		t.Fatalf("hidden project in the public project list: %d %s", code, out)
	}
	if code, _ := e.apiDo("GET", "/api/v1/users/dan/projects/"+url.PathEscape(api.HashedProjectKey("local:x"))+"/ysws", "", nil); code != 404 {
		t.Fatalf("hidden project's export: %d", code)
	}
}

// fixed: POST /device/approve had no rate limit (the confirm step, POST
// /device, allowed 8 then 0.2/s per user), so user codes could be
// guessed as fast as the server answered. Both steps now draw on one
// per-user and one per-IP bucket.
func TestDeviceApproveRateLimited(t *testing.T) {
	e := newEnv(t)
	atk := e.browser()
	atk.login("mallory", "user")
	tok := atk.csrf("/device")
	limited := 0
	for i := 0; i < 40; i++ {
		_, body := atk.post("/device/approve", url.Values{"csrf": {tok}, "code": {fmt.Sprintf("BCDF-%04d", 2000+i)}})
		if strings.Contains(body, "Too many") {
			limited++
		}
	}
	if limited < 30 {
		t.Fatalf("approve is not rate limited: %d of 40 refused", limited)
	}
	// the confirm step shares the user's bucket
	for i := 0; i < 4; i++ {
		if _, body := atk.post("/device", url.Values{"csrf": {tok}, "code": {fmt.Sprintf("BCDF-%04d", 3000+i)}}); !strings.Contains(body, "Too many") {
			t.Fatalf("confirm step not limited after the approve guesses")
		}
	}
	// more accounts from the same IP hit the per-IP bucket
	limited = 0
	for a := 0; a < 5; a++ {
		b := e.browser()
		b.login(fmt.Sprintf("mallory%d", a), "user")
		bt := b.csrf("/device")
		for i := 0; i < 7; i++ { // under the per-user burst
			if _, body := b.post("/device/approve", url.Values{"csrf": {bt}, "code": {fmt.Sprintf("BCDF-%04d", 4000+10*a+i)}}); strings.Contains(body, "Too many") {
				limited++
			}
		}
	}
	if limited == 0 {
		t.Fatalf("no per-IP limit across accounts")
	}
}

// fixed: Server.cached never evicted, so enumerating keys grew seatimed's
// heap without bound; a panic in the background refresh killed seatimed.
// Now entries expire at 10x their ttl, the cache is bounded, and panics in
// a cached computation become errors.
func TestCacheEvictsAndIsBounded(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.srv.cached(fmt.Sprintf("probe-%d", i), time.Minute, func(context.Context) (any, error) { return []int{1, 2, 3}, nil })
	}
	facts := func() int {
		e.srv.cacheMu.Lock()
		defer e.srv.cacheMu.Unlock()
		n := 0
		for k := range e.srv.cache {
			if strings.HasPrefix(k, "probe-") {
				n++
			}
		}
		return n
	}
	if n := facts(); n != 3 {
		t.Fatalf("cached before: %d", n)
	}
	// a month later, the next cache write sweeps them out
	later := time.Now().Add(30 * 24 * time.Hour)
	e.srv.SetClock(func() time.Time { return later })
	e.srv.cached("sweep", time.Second, func(context.Context) (any, error) { return 1, nil })
	if n := facts(); n != 0 {
		t.Fatalf("cached a month later: %d", n)
	}
	// bounded however many keys are fetched
	for i := 0; i < maxCacheEntries+50; i++ {
		e.srv.cached(fmt.Sprintf("k%d", i), time.Hour, func(context.Context) (any, error) { return i, nil })
	}
	e.srv.cacheMu.Lock()
	n := len(e.srv.cache)
	e.srv.cacheMu.Unlock()
	if n > maxCacheEntries {
		t.Fatalf("cache holds %d entries (max %d)", n, maxCacheEntries)
	}
	// a panic is an error, for the caller and for anyone waiting on it
	boom := func(context.Context) (any, error) { panic("boom") }
	if _, err := e.srv.cached("boom", time.Minute, boom); err == nil {
		t.Fatal("panic swallowed without an error")
	}
	if _, err := e.srv.cached("boom", time.Minute, boom); err == nil {
		t.Fatal("second call after a panic")
	}
	// and the stale-while-refresh goroutine survives one (else this binary dies)
	e.srv.cached("stale", time.Minute, func(context.Context) (any, error) { return "old", nil })
	e.srv.SetClock(func() time.Time { return later.Add(2 * time.Minute) })
	if v, err := e.srv.cached("stale", time.Minute, boom); err != nil || v != "old" {
		t.Fatalf("stale value: %v %v", v, err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		e.srv.cacheMu.Lock()
		busy := e.srv.cache["stale"].busy
		e.srv.cacheMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh never finished")
		}
	}
}

// fixed (DoS, could OOM-kill seatimed): any enrolled device could submit a
// 2-event bundle (agent_start a month ago, agent_start now) and make the
// server expand the span between them. The rule works on 5 s slots over the
// records' span, so analyze refuses a span over audit.MaxSpan before
// counting, and two bare agent starts a month apart earn nothing.
func TestLongAgentSpanRefused(t *testing.T) {
	e := newEnv(t)
	const key = "github.com/dan/proj"
	for _, c := range []struct {
		name   string
		span   time.Duration
		refuse bool
	}{{"short", 30 * 24 * time.Hour, false}, {"long", 5 * 365 * 24 * time.Hour, true}} {
		d, _ := e.enroll(c.name)
		start := time.Now().UTC().Truncate(time.Millisecond)
		prev := chain.Genesis
		var evs []event.Event
		for i, ts := range []time.Time{start.Add(time.Minute), start.Add(c.span)} {
			ev := event.Event{Seq: int64(i + 1), TS: ts, Kind: event.AgentStart, Project: key}
			var err error
			if prev, err = chain.Next(prev, ev); err != nil {
				t.Fatal(err)
			}
			evs = append(evs, ev)
		}
		e.srv.SetClock(func() time.Time { return start.Add(c.span + time.Minute) })
		if code, out := d.sync(chain.Checkpoint{Seq: 2, Hash: prev}, nil, nil); code != 200 {
			t.Fatal(code, out)
		}
		b := api.Bundle{DeviceID: d.id, Project: key, StartHash: chain.Genesis, ScoreVersion: ledger.Version,
			Entries: []chain.Entry{{Seq: 1, Event: &evs[0]}, {Seq: 2, Event: &evs[1]}}}
		d.resign(&b)
		code, out := e.apiDo("POST", "/api/v1/segments", d.token, b)
		var r api.SegmentResp
		json.Unmarshal(out, &r)
		refused := strings.Contains(strings.Join(r.Problems, "\n"), "send a shorter range")
		if code != 200 || r.Scored.Seconds() != 0 || refused != c.refuse || (c.refuse && r.Status != api.StatusFailed) {
			t.Fatalf("%s: %d %.400s", c.name, code, out)
		}
	}
}

// "slack:<id>" in SEATIME_ADMIN_IDS grants the role only on a Hack Club Auth
// login, the one provider that vouches for a Slack ID.
func TestSlackIDGrantsRoleOnlyViaHCA(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.srv.setSetting(ctx, "admin_ids", ptr("slack:U0TEST123"), "boss"); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *store.User {
		e.browser().login(name, "user")
		u, err := e.st.UserByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	gh := mk("ghuser")
	e.srv.applyRoles(ctx, gh, store.Identity{Provider: "github", Subject: "1"}, "U0TEST123")
	if gh.Role == store.RoleAdmin {
		t.Fatal("a non-HCA login was promoted by a Slack ID")
	}
	hc := mk("hcuser")
	e.srv.applyRoles(ctx, hc, store.Identity{Provider: "hackclub", Subject: "ident!x"}, "U0TEST999")
	if hc.Role == store.RoleAdmin {
		t.Fatal("wrong Slack ID promoted")
	}
	e.srv.applyRoles(ctx, hc, store.Identity{Provider: "hackclub", Subject: "ident!x"}, "U0TEST123")
	if hc.Role != store.RoleAdmin {
		t.Fatalf("HCA login with the listed Slack ID: role %q", hc.Role)
	}
	if got := idSet("slack:U0TEST123"); !got["slack:U0TEST123"] {
		t.Fatalf("idSet dropped the slack key: %v", got)
	}
}

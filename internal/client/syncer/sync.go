package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/checks"
	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/project"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

var ErrNotLoggedIn = errors.New("not logged in (run `seatime login`)")

type Syncer struct {
	Store   *store.Store
	Version string
	Now     func() time.Time
	// Auth overrides the stored credentials (nil = config.LoadAuth()). The
	// simulator uses it to run several devices in one process.
	Auth *config.Auth
	// Live, when set, is the idle recorder's open window (the daemon's),
	// so a prompt check need not wait for it to be written.
	Live func() *event.Event
}

// ServerKey maps a local project key to the key sent to the server: public
// forge remotes are sent as-is, everything else becomes local:<sha256 prefix>.
func ServerKey(key string) string { return event.ServerKey(key) }

// NameKey is the store meta key holding the name the user chose for a
// project (`seatime name`).
func NameKey(local string) string { return "project_name:" + local }

// Plan is exactly what the next sync would send, request by request.
type Plan struct {
	Server   string
	Requests []api.SyncReq
	Head     int64
	Now      time.Time
	// Resync is the recount a late verdict asked for (zero if none).
	Resync time.Time
}

// Next builds the next sync's requests without sending anything (`seatime
// inspect` prints them). ok=false means there is nothing new to send.
func (s *Syncer) Next() (plan Plan, ok bool, err error) {
	auth, ok := config.LoadAuth()
	if s.Auth != nil {
		auth, ok = *s.Auth, s.Auth.Token != ""
	}
	if !ok {
		return plan, false, ErrNotLoggedIn
	}
	cfg := config.Load()
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	plan.Server, plan.Now = auth.APIBase(cfg), now
	head, hash, err := s.Store.Head()
	if err != nil {
		return plan, false, err
	}
	plan.Head = head
	st, err := s.Store.SyncState()
	if err != nil {
		return plan, false, err
	}
	resync, again := s.Store.ResyncFrom() // a verdict came back: recount what it covers
	if head == 0 || head == st.Seq && !again {
		return plan, false, nil
	}
	plan.Resync = resync
	minTS := now
	if head != st.Seq {
		if minTS, err = s.Store.MinTS(st.Seq); err != nil {
			return plan, false, err
		}
	}
	// Resend from Resend before the oldest new record: a new record changes
	// credit at most that far back (a return after an agent stop credits the
	// run before it; rule 4), and the server accepts that much before the
	// previous checkpoint.
	from := minTS.Add(-Resend)
	// a late verdict recounts back to its prompt, as far as the server takes
	// it (its setting prompt_check_late_days; 24 h for an older server)
	recount := max(s.Store.MetaDays(store.MetaRecountDays, LowerBack), LowerBack)
	if again && resync.Before(from) {
		from = resync
		if lim := now.Add(-recount); from.Before(lim) {
			from = lim
		}
	}
	// An idle record arrives when its window closes (up to an hour later)
	// and says there was no input since some earlier moment: recount from
	// there too, so the server drops what it no longer credits (rule 8 only
	// ever removes time). At most a day back.
	idles, err := s.Store.Events(store.Query{AfterSeq: st.Seq, UpToSeq: head, Kinds: []event.Kind{event.Idle}})
	if err != nil {
		return plan, false, err
	}
	for _, e := range idles {
		for _, ni := range e.NoInput {
			if t := time.UnixMilli(ni[0]); t.Before(from) && !t.Before(now.Add(-LowerBack)) {
				from = t
			}
		}
	}
	if lim := now.Add(-recount); from.Before(lim) {
		from = lim
	}
	from = from.Truncate(time.Minute)
	to := now.Add(time.Minute).Truncate(time.Minute)

	cp := chain.Checkpoint{Seq: head, Hash: hash}
	sig, err := SignCheckpoint(auth, cp)
	if err != nil {
		return plan, false, err
	}
	newCommits, err := s.Store.Events(store.Query{AfterSeq: st.Seq, UpToSeq: head, Kinds: []event.Kind{event.Commit}})
	if err != nil {
		return plan, false, err
	}
	var commits []api.CommitRef
	for _, c := range newCommits {
		commits = append(commits, api.CommitRef{Project: ServerKey(c.Project), SHA: c.SHA, TS: c.TS, Ins: c.Insertions, Del: c.Deletions})
	}
	// The same rule and range code as the dashboard (stats.Credit), cut into
	// minutes. Each request describes its whole window, so the server
	// replaces what this device sent for it before.
	res, evs, err := stats.Credit(s.Store, from, to)
	if err != nil {
		return plan, false, err
	}
	all := ledger.Buckets(res, evs)
	const step = 24 * time.Hour // a long offline gap stays small per request
	for w := from; ; w = w.Add(step) {
		wEnd := w.Add(step)
		if wEnd.After(to) {
			wEnd = to
		}
		req := api.SyncReq{
			DeviceID: auth.DeviceID, ClientVersion: s.Version, ScoreVersion: ledger.VersionFor(s.Store.PromptCheckOn()),
			Checkpoint: cp, CheckpointSig: sig, From: w.UTC(), To: wEnd.UTC(),
		}
		seen := map[string]bool{}
		for _, b := range all {
			if b.Project == "" || b.Start.Before(w) || !b.Start.Before(wEnd) {
				continue
			}
			b.Project = s.mapKey(b.Project, &req, seen)
			req.Buckets = append(req.Buckets, b)
		}
		if wEnd.Equal(to) { // commits + metas ride with the final request
			req.Commits = commits
			for _, e := range newCommits {
				s.mapKey(e.Project, &req, seen)
			}
			if s.Store.PromptCheckOn() {
				req.Pending = &api.PendingTime{}
				pend, _ := stats.PendingByProject(s.Store, now.Add(-24*time.Hour), now)
				for p, sec := range pend {
					req.Pending.Projects = append(req.Pending.Projects, api.ProjectPending{Project: s.mapKey(p, &req, seen), Seconds: sec})
				}
			}
		}
		plan.Requests = append(plan.Requests, req)
		if !wEnd.Before(to) {
			break
		}
	}
	return plan, true, nil
}

// Resend is how far before the oldest unsynced record a sync recounts.
const Resend = 45 * time.Minute

// LowerBack is how far back a sync may recount because of a late idle
// record. The server accepts only lower numbers that far back.
const LowerBack = 24 * time.Hour

// SyncOnce uploads buckets for everything that changed since the last
// successful sync. Returns false (nil error) if there was nothing new.
func (s *Syncer) SyncOnce(ctx context.Context) (bool, error) {
	plan, ok, err := s.Next()
	if err != nil || !ok {
		return false, err
	}
	auth, _ := config.LoadAuth()
	if s.Auth != nil {
		auth = *s.Auth
	}
	client := NewClient(plan.Server, auth.Token, s.Version)
	// the prompt hashes first: a check the server asks for in this answer
	// needs them there
	if s.Store.ChainOn() {
		if _, err := pushHashes(ctx, s.Store, client, auth.DeviceID); err != nil {
			logx.Printf("prompt hashes: %v", err)
		}
	}
	// remember what we are about to show the server: if the local log is
	// ever lost, a new one continues the chain from here (store.OpenChecked)
	if err := store.WriteAnchor(s.Store.Path, plan.Requests[0].Checkpoint, plan.Now); err != nil {
		return false, fmt.Errorf("writing the chain anchor: %w", err)
	}
	var lastResp api.SyncResp
	for _, req := range plan.Requests {
		lastResp, err = client.Sync(ctx, req)
		if client.OffsetOK {
			_ = s.Store.SetMeta("clock_offset_ms", strconv.FormatInt(client.Offset.Milliseconds(), 10))
			_ = s.Store.SetMeta("clock_offset_at", strconv.FormatInt(plan.Now.Unix(), 10))
		}
		if err != nil {
			return false, err
		}
		if !lastResp.OK && lastResp.Message != "" {
			return false, errors.New(lastResp.Message)
		}
	}
	if err := s.Store.SetSyncState(plan.Head, plan.Now); err != nil {
		return false, err
	}
	if !plan.Resync.IsZero() {
		_ = s.Store.ClearResync(plan.Resync)
	}
	_ = s.Store.SetMeta("server_message", lastResp.Message)
	on := "0"
	if lastResp.PromptCheck {
		on = "1"
	}
	_ = s.Store.SetMeta(store.MetaPromptCheck, on)
	if len(lastResp.CheckPeriods) > 0 {
		if b, err := json.Marshal(lastResp.CheckPeriods); err == nil {
			_ = s.Store.SetMeta(store.MetaCheckPeriods, string(b))
		}
	}
	if lastResp.QueueDays > 0 {
		_ = s.Store.SetMeta(store.MetaQueueDays, strconv.FormatFloat(lastResp.QueueDays, 'f', -1, 64))
	}
	if lastResp.RecountDays > 0 {
		_ = s.Store.SetMeta(store.MetaRecountDays, strconv.FormatFloat(lastResp.RecountDays, 'f', -1, 64))
	}
	if n := lastResp.EarlierPrompts; n != 0 { // -1: none
		_ = s.Store.SetMeta(store.MetaEarlier, strconv.Itoa(max(n, 0)))
	}
	if lastResp.CheckTimeoutS > 0 {
		_ = s.Store.SetMeta(MetaCheckTimeout, strconv.Itoa(lastResp.CheckTimeoutS))
	}
	if err := keepChainResp(s.Store, lastResp.PromptChainResp); err != nil {
		return false, err
	}
	_ = s.Store.SetMeta("last_sync_error", "")
	return true, nil
}

// mapKey converts a local project key to the server key and records a
// ProjectMeta for it once per request. The label is what the project's
// folder goes by now, which is the key until a remote is added or changes
// (store folders.go); the name and the remote come from it. The name is
// the one the user chose (`seatime name`), or a public repo's own name, or
// else the folder's name (event.FolderName): the name only, never the path.
func (s *Syncer) mapKey(local string, req *api.SyncReq, seen map[string]bool) string {
	sk := ServerKey(local)
	if !seen[sk] {
		seen[sk] = true
		label := s.Store.Label(local)
		meta := api.ProjectMeta{Key: sk, Label: ServerKey(label)}
		public := meta.Label == label && project.IsPublicForge(label)
		if n, _ := s.Store.GetMeta(NameKey(local)); n != "" {
			meta.Name = n
		} else if public {
			meta.Name = label[strings.LastIndex(label, "/")+1:]
		} else if n := event.FolderName(label); n != "" {
			meta.Name, meta.Folder = n, true
		}
		if public {
			meta.Remote = "https://" + label
		}
		req.Projects = append(req.Projects, meta)
	}
	return sk
}

// ClockOffset returns the server clock minus this machine's, as measured at
// the last sync that got an answer, and when that was.
func ClockOffset(st *store.Store) (off time.Duration, at time.Time, ok bool) {
	v, _ := st.GetMeta("clock_offset_ms")
	a, _ := st.GetMeta("clock_offset_at")
	ms, err1 := strconv.ParseInt(v, 10, 64)
	sec, err2 := strconv.ParseInt(a, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, time.Time{}, false
	}
	return time.Duration(ms) * time.Millisecond, time.Unix(sec, 0), true
}

// MetaCheckTimeout holds how long the server may take over one prompt
// check, in seconds (from the last sync answer).
const MetaCheckTimeout = "check_timeout_s"

// CheckTimeout is how long to wait for one prompt check: the server's
// timeout plus a margin, at least 3 minutes.
func CheckTimeout(st *store.Store) time.Duration {
	d := 3 * time.Minute
	if v, _ := st.GetMeta(MetaCheckTimeout); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d = max(d, time.Duration(n)*time.Second+time.Minute)
		}
	}
	return d
}

// CheckOnce sends the turns waiting for the prompt check (package checks),
// at most 20 a round: every one while the account has the check on, else
// only those the server asked for.
func (s *Syncer) CheckOnce(ctx context.Context) (int, error) {
	auth, ok := config.LoadAuth()
	if s.Auth != nil {
		auth, ok = *s.Auth, s.Auth.Token != ""
	}
	if !ok {
		return 0, ErrNotLoggedIn
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	client := NewClient(auth.APIBase(config.Load()), auth.Token, s.Version)
	client.CheckTimeout = CheckTimeout(s.Store)
	send := func(ctx context.Context, req api.PromptCheckReq) (api.PromptCheckResp, int, error) {
		r, err := client.PromptCheck(ctx, req)
		var he *HTTPError
		if errors.As(err, &he) {
			return r, he.Status, err
		}
		if err != nil {
			return r, 0, err
		}
		return r, 200, nil
	}
	var live []event.Event
	if s.Live != nil {
		if e := s.Live(); e != nil {
			live = append(live, *e)
		}
	}
	return checks.Drain(ctx, s.Store, auth.DeviceID, send, now, 20, live...)
}

// keepChainResp keeps what a sync answer said about the prompt chain. An
// older server says nothing (Chain 0): the device then stops sending hashes
// and counts with CheckPeriods alone.
func keepChainResp(st *store.Store, r api.PromptChainResp) error {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	_ = st.SetMeta(store.MetaChain, b(r.Chain >= 1))
	_ = st.SetMeta(store.MetaServerCheck, b(r.ServerCheck))
	_ = st.PutJSON(store.MetaServerPeriods, r.ServerPeriods)
	_ = st.PutJSON(store.MetaUserPeriods, r.UserPeriods)
	pulls := make([]store.Pull, 0, len(r.Pulls))
	for _, p := range r.Pulls {
		pulls = append(pulls, store.Pull{Project: p.Project, From: p.From, To: p.To})
	}
	_ = st.PutJSON(store.MetaPulls, pulls)
	for _, c := range r.Closed {
		if err := st.CloseByServer(c.Commit, c.Status); err != nil {
			return err
		}
	}
	return nil
}

// PushHashes sends the prompt hashes the server has not taken yet
// (pkg/promptcheck chain.go), while the server takes them. The daemon
// calls it every few seconds, so a hash reaches the server moments after
// its prompt was recorded.
func (s *Syncer) PushHashes(ctx context.Context) (int, error) {
	if !s.Store.ChainOn() {
		return 0, nil
	}
	if es, err := s.Store.UnsentChain(1); err != nil || len(es) == 0 {
		return 0, err
	}
	auth, ok := config.LoadAuth()
	if s.Auth != nil {
		auth, ok = *s.Auth, s.Auth.Token != ""
	}
	if !ok {
		return 0, ErrNotLoggedIn
	}
	return pushHashes(ctx, s.Store, NewClient(auth.APIBase(config.Load()), auth.Token, s.Version), auth.DeviceID)
}

func pushHashes(ctx context.Context, st *store.Store, client *Client, deviceID string) (int, error) {
	sent := 0
	relinked := false
	for {
		es, err := st.UnsentChain(api.MaxPromptHashes)
		if err != nil || len(es) == 0 {
			return sent, err
		}
		resp, err := client.PromptHashes(ctx, api.PromptHashesReq{DeviceID: deviceID, Entries: es})
		var he *HTTPError
		switch {
		case err == nil:
			if err := st.MarkChainSent(es[len(es)-1].Seq); err != nil {
				return sent, err
			}
			sent += len(es)
		case errors.As(err, &he) && he.Status == 409 && !relinked && resp.Link != "":
			// the server holds a different head (this store was lost or
			// restored): what it never took goes after its head
			if err := st.RelinkUnsent(resp.Seq, resp.Link); err != nil {
				return sent, err
			}
			relinked = true
		default:
			return sent, err
		}
	}
}

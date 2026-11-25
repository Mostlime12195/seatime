package server

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/ledger"
)

func hashHex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// decodeBody reads JSON (optionally gzip'd) with hard size limits.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, rawMax, inflatedMax int64) error {
	var rd io.Reader = http.MaxBytesReader(w, r.Body, rawMax)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return fmt.Errorf("bad gzip body")
		}
		defer gz.Close()
		rd = io.LimitReader(gz, inflatedMax+1)
		raw, err := io.ReadAll(rd)
		if err != nil {
			return err
		}
		if int64(len(raw)) > inflatedMax {
			return fmt.Errorf("body too large")
		}
		return json.Unmarshal(raw, v)
	}
	return json.NewDecoder(rd).Decode(v)
}

// ---------------- device code flow ----------------

const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ23456789"

func newUserCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = userCodeAlphabet[int(b[i])%len(userCodeAlphabet)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func normUserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(s))
	if len(s) != 8 {
		return ""
	}
	return s[:4] + "-" + s[4:]
}

func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !s.allowIP(r, "devstart", 10.0/60, 10) {
		jsonErr(w, 429, "rate limited")
		return
	}
	var req api.DeviceStartReq
	if err := decodeBody(w, r, &req, 16<<10, 16<<10); err != nil {
		jsonErr(w, 400, "bad request")
		return
	}
	if _, err := decodePub(req.PublicKey); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(req.DeviceName)
	if name == "" || len(name) > 80 {
		name = "unnamed device"
	}
	if len(req.OS) > 40 {
		req.OS = req.OS[:40]
	}
	prev := s.relinkFrom(r.Context(), req)
	dcode := randHex(24)
	const ttl = 15 * time.Minute
	for i := 0; ; i++ {
		uc := newUserCode()
		err := s.store.CreateDeviceCode(r.Context(), hashHex(dcode), store.DeviceCode{
			UserCode: uc, DeviceName: name, OS: req.OS, PublicKey: req.PublicKey, Expires: time.Now().Add(ttl), PrevDevice: prev}) // real clock: the store checks expiry against time.Now()
		if err == nil {
			jsonOut(w, 200, api.DeviceStartResp{DeviceCode: dcode, UserCode: uc,
				VerificationURL: s.cfg.BaseURL + "/device?code=" + uc, Interval: 3, ExpiresIn: int(ttl.Seconds())})
			return
		}
		if i > 3 {
			s.log.Error("device code", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
	}
}

func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req api.DevicePollReq
	if err := decodeBody(w, r, &req, 4<<10, 4<<10); err != nil || req.DeviceCode == "" {
		jsonErr(w, 400, "bad request")
		return
	}
	if !s.lim.Allow("devpoll:"+hashHex(req.DeviceCode), 1, 5) {
		jsonErr(w, 429, "slow_down")
		return
	}
	dc, err := s.store.ConsumeDeviceCode(r.Context(), hashHex(req.DeviceCode))
	switch {
	case errors.Is(err, store.ErrNotFound):
		jsonErr(w, 404, "unknown device code")
		return
	case errors.Is(err, store.ErrExpired):
		jsonErr(w, 410, "expired")
		return
	case err != nil:
		s.log.Error("poll", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	if dc.UserID == 0 {
		jsonErr(w, 202, "authorization_pending")
		return
	}
	u, err := s.store.UserByID(r.Context(), dc.UserID)
	if err != nil {
		jsonErr(w, 500, "internal error")
		return
	}
	tb := make([]byte, 32)
	rand.Read(tb)
	token := "grt_" + base64.RawURLEncoding.EncodeToString(tb)
	// a sign-in signed by an earlier device of the same account, with the
	// same key, reconnects that device: it keeps its id and its history
	if dc.PrevDevice != "" {
		if prev, err := s.store.DeviceByID(r.Context(), dc.PrevDevice); err == nil && prev.UserID == u.ID && prev.PublicKey == dc.PublicKey {
			if err := s.store.RelinkDevice(r.Context(), u.ID, prev.ID, hashHex(token)); err != nil {
				s.log.Error("relink device", "err", err)
				jsonErr(w, 500, "internal error")
				return
			}
			s.log.Info("device reconnected", "device", prev.ID, "user", u.Username)
			jsonOut(w, 200, api.DevicePollResp{Token: token, DeviceID: prev.ID, Username: u.Username})
			return
		}
	}
	dev := store.Device{ID: randHex(8), UserID: u.ID, Name: dc.DeviceName, OS: dc.OS, PublicKey: dc.PublicKey, Created: s.now()}
	if err := s.store.CreateDevice(r.Context(), dev, hashHex(token)); err != nil {
		s.log.Error("create device", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	jsonOut(w, 200, api.DevicePollResp{Token: token, DeviceID: dev.ID, Username: u.Username})
}

// authDevice resolves the bearer token. It writes the error response itself.
func (s *Server) authDevice(w http.ResponseWriter, r *http.Request) (*store.Device, *store.User, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		jsonErr(w, 401, "missing bearer token")
		return nil, nil, false
	}
	dev, err := s.store.DeviceByTokenHash(r.Context(), hashHex(tok))
	if err != nil {
		jsonErr(w, 401, "invalid token")
		return nil, nil, false
	}
	if dev.Revoked {
		// the device asks to reconnect without opening a browser
		// (api.DeviceStartReq Prev*); its owner approves it on the web
		jsonErr(w, 401, "device revoked")
		return nil, nil, false
	}
	if !s.lim.Allow("tok:"+dev.ID, 2, 20) {
		w.Header().Set("Retry-After", "5")
		jsonErr(w, 429, "rate limited")
		return nil, nil, false
	}
	u, err := s.store.UserByID(r.Context(), dev.UserID)
	if err != nil {
		jsonErr(w, 401, "unknown user")
		return nil, nil, false
	}
	if !u.Deleted.IsZero() && r.URL.Path != "/api/v1/device/check-in" && !strings.HasPrefix(r.URL.Path, "/api/v1/device/requests/") {
		// the account is deleted: the device may only check in and
		// report back, to fetch its request to remove SeaTime
		jsonErr(w, 403, "account deleted")
		return nil, nil, false
	}
	return dev, u, true
}

// ---------------- sync ----------------

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func validBucket(b ledger.Bucket, now time.Time) bool {
	return b.Project != "" && len(b.Project) <= 300 &&
		b.Start.Unix()%60 == 0 && b.Start.Unix() > 1.5e9 && b.Start.Before(now.Add(2*time.Minute)) &&
		finite(b.CodingSeconds) && finite(b.AISeconds) && finite(b.AINotCountedSeconds) &&
		b.CodingSeconds >= 0 && b.AISeconds >= 0 && b.Seconds() <= 60.5 &&
		b.AINotCountedSeconds >= 0 && b.AINotCountedSeconds <= 60.5 &&
		b.Prompts >= 0 && b.Edits >= 0 && b.Commits >= 0 && b.Runs >= 0 &&
		b.Prompts < 1000 && b.Edits < 100000 && b.Commits < 1000 && b.Runs < 1000
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	dev, user, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	var req api.SyncReq
	if err := decodeBody(w, r, &req, 4<<20, 16<<20); err != nil {
		jsonErr(w, 400, "bad request: "+err.Error())
		return
	}
	if req.DeviceID != dev.ID {
		jsonErr(w, 403, "device_id does not match token")
		return
	}
	if len(req.Buckets) > 5000 || len(req.Commits) > 500 || len(req.Projects) > 200 {
		jsonErr(w, 413, "too many buckets/commits/projects in one sync")
		return
	}
	// checkpoint signature
	if !hex64.MatchString(req.Checkpoint.Hash) || req.Checkpoint.Seq <= 0 {
		jsonErr(w, 400, "bad checkpoint")
		return
	}
	pub, err := decodePub(dev.PublicKey)
	sig, err2 := base64.StdEncoding.DecodeString(req.CheckpointSig)
	msg := fmt.Sprintf("seatime-checkpoint\n%s\n%d\n%s", dev.ID, req.Checkpoint.Seq, req.Checkpoint.Hash)
	if err != nil || err2 != nil || !verifyEd(pub, []byte(msg), sig) {
		jsonErr(w, 400, "bad checkpoint signature")
		return
	}
	ctx := r.Context()
	now := s.now()
	if v := clip(req.ClientVersion, 40); v != dev.ClientVersion {
		if err := s.store.SetClientVersion(ctx, dev.ID, v); err != nil {
			s.log.Error("client version", "err", err)
		}
	}
	// Buckets may not reach back before this device's previous checkpoint
	// (minus the 30 min the client re-sends): anything older was either synced
	// already or is being backfilled after the fact. A sync the server
	// refused (an old SeaTime told to update) moved no time, so the floor is
	// the last accepted sync, whichever is older: once the device updates it
	// recounts what it was told to wait with.
	floor := dev.Created
	if prev, err := s.store.CheckpointBefore(ctx, dev.ID, req.Checkpoint.Seq); err == nil {
		floor = prev.ServerTS
	}
	if !dev.LastSeen.IsZero() && dev.LastSeen.Before(floor) {
		floor = dev.LastSeen
	}
	floor = floor.Add(-bucketBackfill)
	if err := s.store.AddCheckpoint(ctx, dev.ID, req.Checkpoint, now); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			s.log.Warn("checkpoint conflict", "device", dev.ID, "seq", req.Checkpoint.Seq)
			jsonErr(w, 409, "checkpoint seq already recorded with a different hash")
		case errors.Is(err, store.ErrRegression):
			s.log.Warn("checkpoint regression", "device", dev.ID, "seq", req.Checkpoint.Seq)
			jsonErr(w, 409, "checkpoint seq is behind the server's latest for this device")
		default:
			s.log.Error("checkpoint", "err", err)
			jsonErr(w, 500, "internal error")
		}
		return
	}

	if s.clientTooOld(req.ClientVersion) {
		jsonOut(w, 200, api.SyncResp{OK: false, ServerTime: now, PromptCheck: s.pol().PromptCheck && s.userCheckOn(ctx, user.ID), Message: s.updateClientMsg()})
		return
	}

	// projects (metas first so remotes are known before buckets create bare ones)
	byKey := map[string]*store.Project{}
	for _, m := range req.Projects {
		if m.Key == "" || len(m.Key) > 300 || len(m.Name) > 200 || len(m.Remote) > 300 {
			continue
		}
		if m.Remote != "" && !strings.HasPrefix(m.Remote, "https://") && !strings.HasPrefix(m.Remote, "http://") {
			m.Remote = ""
		}
		if len(m.Label) > 300 {
			m.Label = ""
		}
		p, err := s.store.DeviceProject(ctx, user.ID, dev.ID, m)
		if err != nil {
			s.log.Error("upsert project", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
		byKey[m.Key] = p
	}
	// Each bucket goes to the project its key belongs to (store
	// projectkeys.go), under the key that project goes by now. A minute the
	// device recorded under two keys of one project is one minute.
	merged := make([]ledger.Bucket, 0, len(req.Buckets))
	at := map[string]int{}
	for _, b := range req.Buckets {
		if b.Project != "" && len(b.Project) <= 300 {
			p := byKey[b.Project]
			if p == nil {
				if p, err = s.store.DeviceProject(ctx, user.ID, dev.ID, api.ProjectMeta{Key: b.Project}); err != nil {
					s.log.Error("upsert project", "err", err)
					jsonErr(w, 500, "internal error")
					return
				}
				byKey[b.Project] = p
			}
			b.Project = p.Key
			k := fmt.Sprintf("%d %s", b.Start.Unix(), b.Project)
			if i, ok := at[k]; ok {
				merged[i].Add(b)
				continue
			}
			at[k] = len(merged)
		}
		merged = append(merged, b)
	}
	req.Buckets = merged
	skipped, late := 0, 0
	good := make([]ledger.Bucket, 0, len(req.Buckets))
	var before []ledger.Bucket // older than the floor: may only lower what is stored
	lowFloor := now.Add(-lowerBack)
	// a verdict that came back late may still raise what it covers this far
	// back (setting prompt_check_late_days); nothing else older than the floor can
	lateFloor := now.Add(-time.Duration(s.pol().LateDays * float64(24*time.Hour)))
	var stale []ledger.Bucket
	var struggle float64
	for _, b := range req.Buckets {
		if !validBucket(b, now) {
			skipped++
			continue
		}
		b.Start = b.Start.UTC()
		if len(b.Harnesses) > 8 {
			b.Harnesses = b.Harnesses[:8]
		}
		for i, h := range b.Harnesses {
			if len(h) > 40 {
				b.Harnesses[i] = h[:40]
			}
			b.Harnesses[i] = strings.ReplaceAll(b.Harnesses[i], ",", "_")
		}
		b.Models = cleanModels(b)
		switch {
		case b.Start.Before(lowFloor) && b.Start.Before(lateFloor):
			late++
		case b.Start.Before(lowFloor):
			stale = append(stale, b)
		case b.Start.Before(floor):
			before = append(before, b)
		default:
			good = append(good, b)
		}
	}
	// A window the client describes in full replaces what this device sent
	// for it before: minutes the rule later moved to another project or
	// removed (an idle record written after the fact) do not linger. Before
	// the floor a sync can only remove or lower time, never add it.
	if !req.From.IsZero() && req.To.After(req.From) && req.To.Sub(req.From) <= 8*24*time.Hour {
		from := req.From
		if from.Before(lowFloor) {
			from = lowFloor
		}
		keep := append(append([]ledger.Bucket(nil), good...), before...)
		if err := s.store.DeleteBuckets(ctx, user.ID, dev.ID, from.Truncate(time.Minute), req.To, keep); err != nil {
			s.log.Error("replace buckets", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
	}
	before, lateRaise, err := s.lateVerdicts(ctx, dev.ID, before)
	if err != nil {
		s.log.Error("late verdicts", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	good = append(good, lateRaise...)
	// older than a day: only what a late verdict covers is taken, and only
	// up (a recount after a judge outage); the rest is as stored
	if _, staleRaise, err := s.lateVerdicts(ctx, dev.ID, stale); err != nil {
		s.log.Error("late verdicts", "err", err)
		jsonErr(w, 500, "internal error")
		return
	} else if hi, n, err := s.raiseOnly(ctx, user.ID, dev.ID, staleRaise); err != nil {
		s.log.Error("late verdicts", "err", err)
		jsonErr(w, 500, "internal error")
		return
	} else {
		good = append(good, hi...)
		late += len(stale) - n
	}
	lowered, raised, err := s.lowerOnly(ctx, user.ID, dev.ID, before)
	if err != nil {
		s.log.Error("lower buckets", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	late += raised
	if err := s.capMinutes(ctx, user.ID, good); err != nil {
		s.log.Error("cap minutes", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	for _, b := range good {
		struggle += b.Seconds()
	}
	if err := s.store.UpsertBuckets(ctx, user.ID, dev.ID, append(good, lowered...)); err != nil {
		s.log.Error("upsert buckets", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	var cs []store.Commit
	for _, c := range req.Commits {
		sha := strings.ToLower(c.SHA)
		if !shaRe.MatchString(sha) || c.Project == "" {
			continue
		}
		p := byKey[c.Project]
		if p == nil {
			p, err = s.store.DeviceProject(ctx, user.ID, dev.ID, api.ProjectMeta{Key: c.Project})
			if err != nil {
				continue
			}
			byKey[c.Project] = p
		}
		ct := c.TS
		if ct.IsZero() || ct.After(now.Add(clockSkew)) {
			ct = now
		}
		cs = append(cs, store.Commit{ProjectID: p.ID, SHA: sha, TS: ct, Ins: c.Ins, Del: c.Del})
	}
	if err := s.store.UpsertCommits(ctx, cs); err != nil {
		s.log.Error("upsert commits", "err", err)
	}
	if s.pol().PromptCheck && req.Pending != nil && len(req.Pending.Projects) <= 200 {
		pend := map[string]float64{}
		for _, p := range req.Pending.Projects {
			if p.Project != "" && len(p.Project) <= 300 && p.Seconds > 0 && p.Seconds <= 24*3600 {
				pend[p.Project] += p.Seconds
			}
		}
		if err := s.store.SetPending(ctx, user.ID, dev.ID, pend, now); err != nil {
			s.log.Error("pending time", "err", err)
		}
	}
	s.store.TouchDevice(ctx, dev.ID, now)

	// live
	msgb, _ := json.Marshal(map[string]any{"type": "sync", "buckets": len(good), "struggle": struggle})
	s.hub.Publish("user:"+strings.ToLower(user.Username), msgb)
	s.hub.Publish("global", msgb)

	// PromptCheck and CheckPeriods are the server's switch and the account's
	// own setting together: an older device, which keeps no prompt chain,
	// sends checks and counts by them alone (promptchain.go)
	resp := api.SyncResp{OK: true, ServerTime: now, PromptCheck: s.pol().PromptCheck && s.userCheckOn(ctx, user.ID), CheckTimeoutS: int(s.pol().Timeout.Seconds()),
		CheckPeriods: s.combinedPeriods(ctx, user.ID), QueueDays: s.pol().QueueDays, RecountDays: s.pol().LateDays, EarlierPrompts: max(s.pol().Earlier, -1)}
	if cr, err := s.chainResp(ctx, dev, user.ID); err != nil {
		s.log.Error("prompt chain answer", "err", err)
	} else {
		resp.PromptChainResp = cr
	}
	if resp.EarlierPrompts == 0 {
		resp.EarlierPrompts = -1 // none (0 would read as "not said")
	}
	if skipped > 0 {
		resp.Message = fmt.Sprintf("%d invalid buckets were skipped", skipped)
	}
	if late > 0 {
		if resp.Message != "" {
			resp.Message += "; "
		}
		resp.Message += fmt.Sprintf("%d buckets older than this device's previous sync were not stored (submit the project to have that time checked)", late)
	}
	jsonOut(w, 200, resp)
}

// bucketBackfill is how far before the device's previous checkpoint a bucket
// may start (the client recounts the 45 min before its oldest unsynced event).
const bucketBackfill = 45 * time.Minute

// lowerBack is how far back a sync may lower or remove minutes it sent
// before (an idle record that arrived late; the client recounts at most a
// day back).
const lowerBack = 25 * time.Hour

// lowerOnly turns buckets older than the floor into updates that can only
// lower what this device stored for the same minute; a minute it never
// stored, or one the bucket would raise, is not taken (raised counts them).
func (s *Server) lowerOnly(ctx context.Context, userID int64, deviceID string, bs []ledger.Bucket) (out []ledger.Bucket, raised int, err error) {
	if len(bs) == 0 {
		return nil, 0, nil
	}
	lo, hi := bs[0].Start, bs[0].Start
	for _, b := range bs {
		if b.Start.Before(lo) {
			lo = b.Start
		}
		if b.Start.After(hi) {
			hi = b.Start
		}
	}
	old, err := s.store.DeviceBuckets(ctx, userID, deviceID, lo, hi.Add(time.Minute))
	if err != nil {
		return nil, 0, err
	}
	type key struct {
		p string
		m int64
	}
	have := map[key]ledger.Bucket{}
	for _, o := range old {
		have[key{o.Project, o.Start.Unix() / 60}] = o
	}
	for _, b := range bs {
		o, ok := have[key{b.Project, b.Start.Unix() / 60}]
		if !ok || b.CodingSeconds > o.CodingSeconds+0.001 || b.AISeconds > o.AISeconds+0.001 {
			raised++
			continue
		}
		if b.CodingSeconds < o.CodingSeconds-0.001 || b.AISeconds < o.AISeconds-0.001 {
			b.Prompts, b.Edits, b.Commits, b.Runs = o.Prompts, o.Edits, o.Commits, o.Runs
			out = append(out, b)
		}
	}
	return out, raised, nil
}

// raiseOnly keeps the buckets that would raise what this device stored for
// the same minute (or add a minute it never stored), for a late verdict's
// recount; it returns them and how many there are.
func (s *Server) raiseOnly(ctx context.Context, userID int64, deviceID string, bs []ledger.Bucket) ([]ledger.Bucket, int, error) {
	if len(bs) == 0 {
		return nil, 0, nil
	}
	lo, hi := bs[0].Start, bs[0].Start
	for _, b := range bs {
		lo, hi = minTime(lo, b.Start), maxTime(hi, b.Start)
	}
	old, err := s.store.DeviceBuckets(ctx, userID, deviceID, lo, hi.Add(time.Minute))
	if err != nil {
		return nil, 0, err
	}
	type key struct {
		p string
		m int64
	}
	have := map[key]ledger.Bucket{}
	for _, o := range old {
		have[key{o.Project, o.Start.Unix() / 60}] = o
	}
	var out []ledger.Bucket
	for _, b := range bs {
		if o, ok := have[key{b.Project, b.Start.Unix() / 60}]; !ok || b.Seconds() > o.Seconds()+0.001 {
			out = append(out, b)
		}
	}
	return out, len(out), nil
}

// capMinutes scales bs so no minute holds more than 60 s of one person
// across projects, counting what the user's other projects (any device)
// already hold for that minute. The honest client already splits overlapping
// projects; this stops a forged sync from filling 60 s/min in every project.
func (s *Server) capMinutes(ctx context.Context, userID int64, bs []ledger.Bucket) error {
	if len(bs) == 0 {
		return nil
	}
	byMin := map[int64][]int{}
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for i, b := range bs {
		m := b.Start.Unix() / 60
		byMin[m] = append(byMin[m], i)
		lo, hi = min(lo, m), max(hi, m)
	}
	existing, err := s.store.UserMinuteHuman(ctx, userID, lo, hi)
	if err != nil {
		return err
	}
	for m, idx := range byMin {
		inReq := map[string]bool{}
		sum := 0.0
		for _, i := range idx {
			inReq[bs[i].Project] = true
			sum += bs[i].Seconds()
		}
		other := 0.0
		for k, h := range existing[m] {
			if !inReq[k] {
				other += h
			}
		}
		allowed := math.Max(0, 60-other)
		if sum <= allowed+0.5 {
			continue
		}
		f := allowed / sum
		for _, i := range idx {
			bs[i].ScaleCounted(f)
		}
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

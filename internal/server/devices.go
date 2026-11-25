package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
)

// What an owner does for their computers on the web: the settings every
// computer applies, each computer's health, and requests a computer
// carries out at its next check-in (pkg/api device.go). A device too old to
// check in shows as needing an update, and is offered nothing it cannot do.

// onlineWithin is how recent a check-in must be for a computer to count as on.
const onlineWithin = 3 * time.Minute

// requestKinds are the requests a device may say it can carry out.
var requestKinds = []string{api.RequestForget, api.RequestUninstall, api.RequestSettings}

var (
	toolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,39}$`)
	codeRe     = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

// cleanCheckIn keeps what the server shows from a check-in: known request
// kinds, tool names and states, problem codes. Anything else is dropped.
func cleanCheckIn(in api.CheckIn) api.CheckIn {
	out := api.CheckIn{ClientVersion: clip(in.ClientVersion, 40), Settings: in.Settings}
	for _, c := range in.Can {
		if slices.Contains(requestKinds, c) && !slices.Contains(out.Can, c) {
			out.Can = append(out.Can, c)
		}
	}
	for _, t := range in.Tools {
		if len(out.Tools) >= 30 {
			break
		}
		if toolNameRe.MatchString(t.Name) && (t.State == api.ToolHooked || t.State == api.ToolFailed || t.State == api.ToolOptedOut) {
			out.Tools = append(out.Tools, t)
		}
	}
	for _, p := range in.Problems {
		if len(out.Problems) >= 30 {
			break
		}
		if codeRe.MatchString(p.Code) && (p.Tool == "" || toolNameRe.MatchString(p.Tool)) {
			out.Problems = append(out.Problems, p)
		}
	}
	return out
}

func (s *Server) handleCheckIn(w http.ResponseWriter, r *http.Request) {
	dev, user, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	var req api.CheckIn
	if err := decodeBody(w, r, &req, 64<<10, 64<<10); err != nil {
		jsonErr(w, 400, "bad request")
		return
	}
	if req.DeviceID != dev.ID {
		jsonErr(w, 403, "device_id does not match token")
		return
	}
	ctx := r.Context()
	now := s.now()
	rep := cleanCheckIn(req)
	b, _ := json.Marshal(rep)
	if err := s.store.PutDeviceHealth(ctx, dev.ID, string(b), now); err != nil {
		s.log.Error("device health", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	if rep.ClientVersion != "" && rep.ClientVersion != dev.ClientVersion {
		if err := s.store.SetClientVersion(ctx, dev.ID, rep.ClientVersion); err != nil {
			s.log.Error("client version", "err", err)
		}
	}
	var resp api.CheckInResp
	if slices.Contains(rep.Can, api.RequestSettings) {
		for _, k := range []string{api.SettingAlsoHackatime, api.SettingAutoUpdate} {
			if v, ok := s.accountSettings(ctx, user.ID)[k]; ok {
				resp.Settings = append(resp.Settings, api.AccountSetting{Key: k, Value: v.Value, Updated: v.Updated})
			}
		}
	}
	reqs, err := s.store.TakeDeviceRequests(ctx, dev.ID, now)
	if err != nil {
		s.log.Error("device requests", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	for _, q := range reqs {
		resp.Requests = append(resp.Requests, api.DeviceRequest{ID: q.ID, Kind: q.Kind, Project: q.Project, Purge: q.Purge})
	}
	jsonOut(w, 200, resp)
}

// resultErrors are the error codes a device may answer with.
var resultErrors = []string{"unknown_project", "failed", "unsupported"}

func (s *Server) handleRequestResult(w http.ResponseWriter, r *http.Request) {
	dev, user, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, 404, "unknown request")
		return
	}
	var res api.RequestResult
	if err := decodeBody(w, r, &res, 4<<10, 4<<10); err != nil {
		jsonErr(w, 400, "bad request")
		return
	}
	if res.DeviceID != dev.ID {
		jsonErr(w, 403, "device_id does not match token")
		return
	}
	ctx := r.Context()
	q, err := s.store.DeviceRequest(ctx, id)
	if err != nil || q.DeviceID != dev.ID {
		jsonErr(w, 404, "unknown request")
		return
	}
	status, code := store.ReqDone, ""
	if !res.OK {
		status, code = store.ReqFailed, "failed"
		if slices.Contains(resultErrors, res.Error) {
			code = res.Error
		}
	}
	err = s.store.FinishDeviceRequest(ctx, id, dev.ID, status, code, max(0, min(res.Forgotten, 1<<30)), s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		jsonErr(w, 409, "request is not open")
		return
	case err != nil:
		s.log.Error("device request result", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	if q.Kind == api.RequestUninstall && res.OK {
		// SeaTime is gone from that computer: its token is of no further use
		if err := s.store.RevokeDevice(ctx, user.ID, dev.ID); err != nil {
			s.log.Error("revoke removed device", "err", err)
		}
		if !user.Deleted.IsZero() {
			s.purgeDeleted(ctx)
		}
	}
	s.log.Info("device request", "id", id, "kind", q.Kind, "device", dev.ID, "status", status)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// accountSettings are the account's settings for its computers; a key
// with no row was never changed and is on.
func (s *Server) accountSettings(ctx context.Context, userID int64) map[string]store.AccountSetting {
	m, err := s.store.AccountSettings(ctx, userID)
	if err != nil {
		s.log.Error("account settings", "err", err)
	}
	return m
}

func (s *Server) accountSettingOn(m map[string]store.AccountSetting, key string) bool {
	v, ok := m[key]
	return !ok || v.Value
}

// relinkFrom is the earlier device a sign-in reconnects: the request names
// it and is signed with its key, within 10 minutes of now. "" otherwise.
func (s *Server) relinkFrom(ctx context.Context, req api.DeviceStartReq) string {
	if req.PrevDeviceID == "" || len(req.PrevDeviceID) > 64 {
		return ""
	}
	if d := s.now().Sub(time.Unix(req.PrevSignedAt, 0)); d > 10*time.Minute || d < -10*time.Minute {
		return ""
	}
	dev, err := s.store.DeviceByID(ctx, req.PrevDeviceID)
	if err != nil || dev.PublicKey != req.PublicKey {
		return ""
	}
	pub, err := decodePub(dev.PublicKey)
	sig, err2 := base64.StdEncoding.DecodeString(req.PrevSig)
	if err != nil || err2 != nil || !verifyEd(pub, []byte(api.RelinkMessage(dev.ID, req.PublicKey, req.PrevSignedAt)), sig) {
		return ""
	}
	return dev.ID
}

// ---------------- the web ----------------

// deviceRow is one computer on /settings.
type deviceRow struct {
	store.Device
	// Reports: it checks in, so it shows its health and takes requests.
	// Older versions only sync: they show as needing an update.
	Reports   bool
	CheckedIn time.Time
	Online    bool
	Version   string
	Tools     []api.ToolHealth
	Problems  []string
	Settings  api.DeviceSettings
	CanForget bool
	CanRemove bool
	CanApply  bool
	// Relink is a sign-in from this computer that waits for approval.
	Relink *store.DeviceCode
	// Removed: SeaTime was removed from it from this page.
	Removed  bool
	Requests []requestRow
}

type requestRow struct {
	What, State string
	When        time.Time
}

func problemText(p api.Problem) string {
	tool := p.Tool
	if tool == "" {
		tool = "An AI tool"
	}
	switch p.Code {
	case api.ProblemHooksSilent:
		return tool + " is in use, but SeaTime hears nothing from it. SeaTime is putting its hooks back and fills the gap from the tool's own log."
	case api.ProblemHookFailed:
		return "SeaTime could not add its hooks to " + tool + "."
	case api.ProblemSyncBehind:
		return "Some work from more than 20 minutes ago has not reached this server yet."
	case api.ProblemSyncFailing:
		return "The last sync failed. SeaTime will try again."
	case api.ProblemNoService:
		return "SeaTime will not start again after this computer restarts."
	case api.ProblemAutoHookOff:
		return "SeaTime does not add its hooks to new AI tools here. Someone turned that off in its config file."
	case api.ProblemNoGit:
		return "Git is not installed, so SeaTime cannot see commits."
	case api.ProblemUpdateFailed:
		return "The last automatic update failed. SeaTime will try again later."
	}
	return "Problem: " + p.Code
}

func requestText(q store.DeviceRequest) requestRow {
	row := requestRow{When: q.Created}
	switch q.Kind {
	case api.RequestForget:
		row.What = "Delete prompt text"
	case api.RequestUninstall:
		row.What = "Remove SeaTime"
		if q.Purge {
			row.What = "Remove SeaTime and its data"
		}
	default:
		row.What = q.Kind
	}
	switch q.Status {
	case store.ReqQueued:
		row.State = "waiting for this computer"
	case store.ReqSent:
		row.State = "in progress"
	case store.ReqDone:
		row.State = "done"
		if q.Kind == api.RequestForget {
			row.State = fmt.Sprintf("done: %d record%s cleared", q.Count, pluralS(q.Count))
		}
	case store.ReqFailed:
		row.State = "failed"
		if q.Error == "unknown_project" {
			row.State = "failed: this computer has no such project"
		}
	}
	return row
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (s *Server) deviceRows(ctx context.Context, userID int64, devs []store.Device) []deviceRow {
	hs, err := s.store.DeviceHealths(ctx, userID)
	if err != nil {
		s.log.Error("device health", "err", err)
	}
	relinks, err := s.store.PendingRelinks(ctx, userID)
	if err != nil {
		s.log.Error("pending relinks", "err", err)
	}
	reqs, _ := s.store.UserDeviceRequests(ctx, userID, s.now().Add(-30*24*time.Hour))
	now := s.now()
	var out []deviceRow
	for _, d := range devs {
		row := deviceRow{Device: d, Version: d.ClientVersion}
		if h, ok := hs[d.ID]; ok {
			var rep api.CheckIn
			if json.Unmarshal([]byte(h.Report), &rep) == nil {
				row.Reports, row.CheckedIn = true, h.At
				row.Online = now.Sub(h.At) <= onlineWithin
				if rep.ClientVersion != "" {
					row.Version = rep.ClientVersion
				}
				row.Tools, row.Settings = rep.Tools, rep.Settings
				for _, p := range rep.Problems {
					row.Problems = append(row.Problems, problemText(p))
				}
				row.CanForget = slices.Contains(rep.Can, api.RequestForget)
				row.CanRemove = slices.Contains(rep.Can, api.RequestUninstall)
				row.CanApply = slices.Contains(rep.Can, api.RequestSettings)
			}
		}
		if dc, ok := relinks[d.ID]; ok {
			row.Relink = &dc
		}
		for _, q := range reqs {
			if q.DeviceID != d.ID {
				continue
			}
			if q.Kind == api.RequestUninstall && q.Status == store.ReqDone {
				row.Removed = true
			}
			if len(row.Requests) < 3 {
				row.Requests = append(row.Requests, requestText(q))
			}
		}
		out = append(out, row)
	}
	return out
}

// ownDevice is one of u's devices, by the form's id.
func (s *Server) ownDevice(ctx context.Context, u *store.User, id string) (*store.Device, bool) {
	d, err := s.store.DeviceByID(ctx, id)
	if err != nil || d.UserID != u.ID {
		return nil, false
	}
	return d, true
}

// handleSettingsComputers saves the settings every computer of the account applies.
func (s *Server) handleSettingsComputers(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	now := s.now()
	for _, k := range []string{api.SettingAlsoHackatime, api.SettingAutoUpdate} {
		if err := s.store.PutAccountSetting(r.Context(), u.ID, k, r.PostFormValue(k) == "1", now); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	}
	http.Redirect(w, r, "/settings?ok="+urlEscape("Saved. Each computer applies it the next time it checks in, within a minute or two.")+"#computers", http.StatusSeeOther)
}

// handleDeviceRemove asks a computer to remove SeaTime from itself.
func (s *Server) handleDeviceRemove(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	d, ok := s.ownDevice(ctx, u, r.PostFormValue("id"))
	if !ok || d.Revoked {
		http.NotFound(w, r)
		return
	}
	if row := s.deviceRows(ctx, u.ID, []store.Device{*d}); len(row) != 1 || !row[0].CanRemove {
		http.Error(w, "this computer runs an older SeaTime that cannot do this yet", 409)
		return
	}
	if _, err := s.store.AddDeviceRequest(ctx, store.DeviceRequest{UserID: u.ID, DeviceID: d.ID, Kind: api.RequestUninstall,
		Purge: r.PostFormValue("purge") == "1", Created: s.now()}); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	http.Redirect(w, r, "/settings?ok="+urlEscape("SeaTime will be removed from "+d.Name+" the next time it checks in.")+"#computers", http.StatusSeeOther)
}

// handleForget asks each computer that can to delete the prompt text it keeps.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	devs, _ := s.store.ListDevices(ctx, u.ID)
	n, old := 0, 0
	for _, row := range s.deviceRows(ctx, u.ID, devs) {
		if row.Revoked {
			continue
		}
		if !row.CanForget {
			old++
			continue
		}
		if _, err := s.store.AddDeviceRequest(ctx, store.DeviceRequest{UserID: u.ID, DeviceID: row.ID, Kind: api.RequestForget, Created: s.now()}); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		n++
	}
	msg := fmt.Sprintf("%d computer%s will delete its prompt text the next time it checks in.", n, pluralS(n))
	if n == 0 {
		msg = "None of your computers can do this yet."
	}
	if old > 0 {
		msg += fmt.Sprintf(" %d computer%s run%s an older SeaTime and must update first.", old, pluralS(old), map[bool]string{true: "s"}[old == 1])
	}
	http.Redirect(w, r, "/settings?ok="+urlEscape(strings.TrimSpace(msg))+"#prompt-text", http.StatusSeeOther)
}

package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
)

func (d *device) checkIn(t *testing.T, ci api.CheckIn) api.CheckInResp {
	t.Helper()
	if ci.DeviceID == "" {
		ci.DeviceID = d.id
	}
	code, out := d.e.apiDo("POST", "/api/v1/device/check-in", d.token, ci)
	if code != 200 {
		t.Fatalf("check-in: %d %s", code, out)
	}
	var r api.CheckInResp
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

var allCan = []string{api.RequestForget, api.RequestUninstall, api.RequestSettings}

// The settings page shows each computer's health in plain text, and an
// older SeaTime (one that never checks in) as needing an update.
func TestDeviceHealthOnSettings(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("ines")
	_, page := br.get("/settings")
	if !strings.Contains(page, "Update needed") {
		t.Error("a device that never checked in does not say it needs an update")
	}
	if strings.Contains(page, "Remove SeaTime from this computer") {
		t.Error("an old device is offered removal")
	}
	d.checkIn(t, api.CheckIn{ClientVersion: "1.2.3", Can: allCan,
		Tools:    []api.ToolHealth{{Name: "claude-code", State: api.ToolHooked}, {Name: "cursor", State: api.ToolFailed}, {Name: "/etc/passwd", State: api.ToolHooked}},
		Problems: []api.Problem{{Code: api.ProblemHooksSilent, Tool: "cursor"}, {Code: "Bad Code <script>"}},
		Settings: api.DeviceSettings{AutoUpdate: true, EditorPlugin: true, AlsoHackatime: true}})
	_, page = br.get("/settings")
	for _, want := range []string{"SeaTime 1.2.3", "claude-code", "cursor (hooks could not be added)", "cursor is in use, but SeaTime hears nothing from it", "reports to SeaTime, and to Hackatime", "Remove SeaTime from this computer"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	for _, bad := range []string{"/etc/passwd", "<script>", "Update needed"} {
		if strings.Contains(page, bad) {
			t.Errorf("settings page shows %q", bad)
		}
	}
}

// Settings chosen on the web reach each computer at its check-in, with
// the time they changed; an unchanged setting keeps its time.
func TestAccountSettingsReachDevices(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("jun")
	if r := d.checkIn(t, api.CheckIn{Can: allCan}); len(r.Settings) != 0 {
		t.Fatalf("settings before any change: %+v", r.Settings)
	}
	tok := br.csrf("/settings")
	if c, _ := br.post("/settings/computers", url.Values{"csrf": {tok}, "auto_update": {"1"}}); c != 200 {
		t.Fatal(c)
	}
	r := d.checkIn(t, api.CheckIn{Can: allCan})
	got := map[string]api.AccountSetting{}
	for _, s := range r.Settings {
		got[s.Key] = s
	}
	if v, ok := got[api.SettingAlsoHackatime]; !ok || v.Value {
		t.Fatalf("also_hackatime: %+v", got)
	}
	if v, ok := got[api.SettingAutoUpdate]; !ok || !v.Value {
		t.Fatalf("auto_update: %+v", got)
	}
	time.Sleep(5 * time.Millisecond)
	br.post("/settings/computers", url.Values{"csrf": {tok}, "auto_update": {"1"}})
	for _, s := range d.checkIn(t, api.CheckIn{Can: allCan}).Settings {
		if !s.Updated.Equal(got[s.Key].Updated) {
			t.Errorf("%s: saving the same value moved its time", s.Key)
		}
	}
	// a device that cannot apply settings is not sent them
	if r := d.checkIn(t, api.CheckIn{Can: []string{api.RequestForget}}); len(r.Settings) != 0 {
		t.Fatal("settings sent to a device that cannot apply them")
	}
}

// Delete prompt text and Remove SeaTime go to the computer at its next
// check-in; its answer shows on the page; removal disconnects it.
func TestDeviceRequests(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("kim")
	d.checkIn(t, api.CheckIn{Can: allCan})
	tok := br.csrf("/settings")
	if c, body := br.post("/settings/forget", url.Values{"csrf": {tok}}); c != 200 || !strings.Contains(body, "1 computer will delete") {
		t.Fatalf("forget: %d %s", c, body)
	}
	// asking twice queues one request
	br.post("/settings/forget", url.Values{"csrf": {tok}})
	r := d.checkIn(t, api.CheckIn{Can: allCan})
	if len(r.Requests) != 1 || r.Requests[0].Kind != api.RequestForget {
		t.Fatalf("requests: %+v", r.Requests)
	}
	if r2 := d.checkIn(t, api.CheckIn{Can: allCan}); len(r2.Requests) != 0 {
		t.Fatal("a request handed out was handed out again at once")
	}
	id := strconv.FormatInt(r.Requests[0].ID, 10)
	// another account's device cannot answer it
	other, _ := e.enroll("lou")
	if c, _ := e.apiDo("POST", "/api/v1/device/requests/"+id, other.token, api.RequestResult{DeviceID: other.id, OK: true}); c != 404 {
		t.Fatalf("another device answered: %d", c)
	}
	if c, out := e.apiDo("POST", "/api/v1/device/requests/"+id, d.token, api.RequestResult{DeviceID: d.id, OK: true, Forgotten: 7}); c != 200 {
		t.Fatalf("result: %d %s", c, out)
	}
	if c, _ := e.apiDo("POST", "/api/v1/device/requests/"+id, d.token, api.RequestResult{DeviceID: d.id, OK: true}); c != 409 {
		t.Fatalf("answered twice: %d", c)
	}
	if _, page := br.get("/settings"); !strings.Contains(page, "done: 7 records cleared") {
		t.Error("the page does not show the answer")
	}
	// removal
	if c, _ := br.post("/settings/device/remove", url.Values{"csrf": {tok}, "id": {d.id}, "purge": {"1"}}); c != 200 {
		t.Fatal(c)
	}
	r = d.checkIn(t, api.CheckIn{Can: allCan})
	if len(r.Requests) != 1 || r.Requests[0].Kind != api.RequestUninstall || !r.Requests[0].Purge {
		t.Fatalf("requests: %+v", r.Requests)
	}
	e.apiDo("POST", "/api/v1/device/requests/"+strconv.FormatInt(r.Requests[0].ID, 10), d.token, api.RequestResult{DeviceID: d.id, OK: true})
	if code, _ := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil); code != 401 {
		t.Fatalf("a removed device still syncs: %d", code)
	}
	if _, page := br.get("/settings"); !strings.Contains(page, "SeaTime was removed from this computer") {
		t.Error("the page does not say SeaTime was removed")
	}
	// someone else's device cannot be removed
	if c, _ := br.post("/settings/device/remove", url.Values{"csrf": {tok}, "id": {other.id}}); c != 404 {
		t.Fatalf("removed another account's device: %d", c)
	}
}

func (d *device) relinkStart(t *testing.T, priv ed25519.PrivateKey, signedAt time.Time) api.DeviceStartResp {
	t.Helper()
	pub := base64.StdEncoding.EncodeToString(d.pub)
	msg := api.RelinkMessage(d.id, pub, signedAt.Unix())
	req := api.DeviceStartReq{DeviceName: "laptop", OS: "linux", PublicKey: pub, PrevDeviceID: d.id, PrevSignedAt: signedAt.Unix(),
		PrevSig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(msg)))}
	code, out := d.e.apiDo("POST", "/api/v1/device/start", "", req)
	if code != 200 {
		t.Fatalf("start: %d %s", code, out)
	}
	var st api.DeviceStartResp
	json.Unmarshal(out, &st)
	return st
}

// A disconnected computer signs in again by itself; its owner approves it
// on the Settings page, and it comes back as the same device.
func TestRelinkSameDevice(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("mo")
	if code, _ := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil); code != 200 {
		t.Fatal(code)
	}
	tok := br.csrf("/settings")
	br.post("/settings/device", url.Values{"csrf": {tok}, "id": {d.id}})
	code, out := d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, nil, nil)
	if code != 401 || !strings.Contains(out, "revoked") {
		t.Fatalf("revoked device: %d %s", code, out)
	}
	st := d.relinkStart(t, d.priv, time.Now())
	_, page := br.get("/settings")
	if !strings.Contains(page, "asks to connect again") || !strings.Contains(page, st.UserCode) {
		t.Fatal("the settings page does not offer to connect the computer again")
	}
	if c, body := br.post("/device/approve", url.Values{"csrf": {tok}, "code": {st.UserCode}}); c != 200 || !strings.Contains(body, "Approved") {
		t.Fatalf("approve: %d %s", c, body)
	}
	code, pout := e.apiDo("POST", "/api/v1/device/poll", "", api.DevicePollReq{DeviceCode: st.DeviceCode})
	if code != 200 {
		t.Fatalf("poll: %d %s", code, pout)
	}
	var pr api.DevicePollResp
	json.Unmarshal(pout, &pr)
	if pr.DeviceID != d.id {
		t.Fatalf("came back as %s, not %s", pr.DeviceID, d.id)
	}
	d.token = pr.Token
	if code, out := d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, nil, nil); code != 200 {
		t.Fatalf("sync after reconnecting: %d %s", code, out)
	}
	devs, _ := e.st.ListDevices(t.Context(), mustUser(t, e, "mo"))
	if len(devs) != 1 {
		t.Fatalf("%d devices after reconnecting", len(devs))
	}
}

// A sign-in signed with the wrong key, or too long ago, is an ordinary new
// device: it is not offered as a reconnect and cannot take over the old id.
func TestRelinkNeedsTheDeviceKey(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("noa")
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	for name, st := range map[string]api.DeviceStartResp{
		"wrong key": d.relinkStart(t, stranger, time.Now()),
		"stale":     d.relinkStart(t, d.priv, time.Now().Add(-time.Hour)),
	} {
		if _, page := br.get("/settings"); strings.Contains(page, st.UserCode) {
			t.Errorf("%s: offered as a reconnect", name)
		}
		tok := br.csrf("/settings")
		br.post("/device/approve", url.Values{"csrf": {tok}, "code": {st.UserCode}})
		_, pout := e.apiDo("POST", "/api/v1/device/poll", "", api.DevicePollReq{DeviceCode: st.DeviceCode})
		var pr api.DevicePollResp
		json.Unmarshal(pout, &pr)
		if pr.DeviceID == "" || pr.DeviceID == d.id {
			t.Errorf("%s: device id %q", name, pr.DeviceID)
		}
	}
}

// A name chosen on the web stays when a device sends another one.
func TestProjectNameFromWeb(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("ola")
	meta := func(r *api.SyncReq) { r.Projects = []api.ProjectMeta{{Key: "local:abcdef123456", Name: "from-cli"}} }
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, meta); code != 200 {
		t.Fatal(code, out)
	}
	p, err := e.st.ProjectByKey(t.Context(), mustUser(t, e, "ola"), "local:abcdef123456")
	if err != nil || p.Name != "from-cli" {
		t.Fatal(p, err)
	}
	tok := br.csrf("/settings")
	if c, _ := br.post("/settings/project", url.Values{"csrf": {tok}, "id": {strconv.FormatInt(p.ID, 10)}, "name": {"Rocket"}}); c != 200 {
		t.Fatal(c)
	}
	d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, nil, meta)
	if p, _ = e.st.ProjectByID(t.Context(), p.ID); p.Name != "Rocket" {
		t.Fatalf("name %q after a sync", p.Name)
	}
}

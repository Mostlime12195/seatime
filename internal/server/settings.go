package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/promptcheck"
)

// Settings: policy values an admin changes on /admin without a redeploy or
// a client release. Each has a default from the environment (the variable
// named in Env), so an empty settings table behaves exactly as the env
// says; a value set on /admin overrides it until reset. Secrets and URLs
// are never settings: they stay in the environment. Every change is
// logged (settings_log) with who made it.
//
// The parsed values live in one immutable policy, swapped whole after each
// change, so a request reads one consistent set (s.pol()).

// policy is the parsed settings in force.
type policy struct {
	// PromptCheck is the switch (docs/RULE.md "When the AI check is on").
	PromptCheck bool
	// Models are the judges to ask, in order: the next is tried when one
	// fails (setting prompt_check_models).
	Models []promptcheck.Target
	// Admins are login ids ("provider:subject").
	Admins map[string]bool
	// Rules are the check's questions, decision, kinds with their minutes
	// and request caps (setting prompt_check_rules).
	Rules *promptcheck.Rules

	// Paid calls: a daily dollar budget across everyone, a daily
	// number of calls per account, a per-account rate, a ceiling on calls
	// in flight; and devices per account. Past any of them a check waits
	// (pending), it is never decided.
	BudgetUSDDay float64
	UserCallsDay int
	UserPerMin   float64
	Concurrency  int
	MaxDevices   int
	// Timeout bounds one judge call, and the request carrying it.
	Timeout time.Duration
	// LateDays is how far back a verdict that came back late may raise
	// synced minutes (and devices recount); QueueDays how long a device
	// keeps a turn waiting for its check.
	LateDays  float64
	QueueDays float64
	// Earlier is how many earlier prompts of the project that passed a
	// check shows the model (the undo red team's fix 2).
	Earlier int
	// MinClient is the oldest SeaTime version whose syncs, checks and
	// submissions are taken ("" none; C2).
	MinClient string

	src map[string]string // key -> the text value in force
}

// settingDef is one setting: how it is named, documented, defaulted and
// parsed. Parse fills p from v and refuses a value it cannot use.
type settingDef struct {
	Key     string
	Env     string // the environment variable that gives the default
	Label   string
	Help    string
	Long    bool     // a textarea on /admin
	Choices []string // a select on /admin
	Def     func(s *Server) string
	Parse   func(s *Server, p *policy, v string) error
}

// settingDefs are every setting, in the order /admin shows them.
var settingDefs []settingDef

func init() {
	settingDefs = []settingDef{
		{Key: "prompt_check", Env: "SEATIME_PROMPT_CHECK", Label: "Prompt check",
			Help:    "The AI check on new prompts (docs/RULE.md). Turn it on only after the devices have updated: see the client versions below.",
			Choices: []string{"off", "on"},
			Def:     func(s *Server) string { return onOff(s.cfg.PromptCheck) },
			Parse: func(_ *Server, p *policy, v string) (err error) {
				p.PromptCheck, err = parseOnOff(v)
				return err
			}},
		{Key: "prompt_check_models", Env: "SEATIME_PROMPT_CHECK_MODEL", Label: "Judge models", Long: true,
			Help: "One model per line, tried in order: when one fails, the next is asked. A line may add an API address after a space; it must be one of SEATIME_PROMPT_CHECK_URL's addresses (addresses and the key stay in the environment).",
			Def: func(s *Server) string {
				if s.cfg.PromptCheckModel != "" {
					return s.cfg.PromptCheckModel
				}
				return promptcheck.Model
			},
			Parse: func(s *Server, p *policy, v string) (err error) {
				p.Models, err = s.parseModels(v)
				return err
			}},
		{Key: "prompt_check_rules", Env: "SEATIME_PROMPT_CHECK_FILE", Label: "Check rules", Long: true,
			Help: "JSON: the questions, which answers count (effort, block, need), the yes threshold, the kinds of prompt in order with their minutes, and the request caps. Each verdict records the rules' version and the minutes it was given, and devices count with those minutes, so a change here needs no client release and never changes a verdict already given. Change the version when you change the rules.",
			Def:  func(s *Server) string { return s.rulesDefault },
			Parse: func(_ *Server, p *policy, v string) (err error) {
				p.Rules, err = promptcheck.ParseRules([]byte(v))
				return err
			}},
		numSetting("prompt_check_budget_usd_day", "SEATIME_PROMPT_CHECK_BUDGET_USD_DAY", "Daily judge budget (USD)",
			"Paid judge calls stop for the rest of the UTC day once their cost reaches this; checks then wait (their time stays pending) and go through the next day. 0 stops all paid calls.",
			"25", 0, 100000, func(p *policy, f float64) { p.BudgetUSDDay = f }),
		numSetting("prompt_check_user_day", "SEATIME_PROMPT_CHECK_USER_DAY", "Judge calls per account per day",
			"Past this an account's checks wait until the next UTC day.", "600", 1, 1e6, func(p *policy, f float64) { p.UserCallsDay = int(f) }),
		numSetting("prompt_check_user_per_min", "SEATIME_PROMPT_CHECK_USER_PER_MIN", "Judge calls per account per minute",
			"A sustained rate, across all of an account's devices (bursts of three minutes' worth are allowed).", "10", 0.1, 1000, func(p *policy, f float64) { p.UserPerMin = f }),
		numSetting("prompt_check_concurrency", "SEATIME_PROMPT_CHECK_CONCURRENCY", "Judge calls at once",
			"Across everyone; past it a check is told to try again shortly.", "8", 1, 256, func(p *policy, f float64) { p.Concurrency = int(f) }),
		numSetting("prompt_check_timeout_s", "SEATIME_PROMPT_CHECK_TIMEOUT_S", "Judge call timeout (seconds)",
			"Bounds one judge call; the request's write deadline and the devices' own timeout follow it.", "120", 10, 900, func(p *policy, f float64) { p.Timeout = time.Duration(f) * time.Second }),
		numSetting("prompt_check_late_days", "SEATIME_PROMPT_CHECK_LATE_DAYS", "Recount window for late checks (days)",
			"A check answered late (a judge outage, a device offline) still raises the synced time of its prompt if the prompt is no older than this; devices recount that far back. Submissions always use every verdict.",
			"14", 1, 62, func(p *policy, f float64) { p.LateDays = f }),
		numSetting("prompt_check_queue_days", "SEATIME_PROMPT_CHECK_QUEUE_DAYS", "How long devices keep a prompt waiting (days)",
			"A prompt that waits longer for its check has its text dropped on the device and stays pending: never counted as failed, never counted.",
			"30", 1, 365, func(p *policy, f float64) { p.QueueDays = f }),
		numSetting("prompt_check_earlier", "SEATIME_PROMPT_CHECK_EARLIER", "Earlier prompts shown to the judge",
			"How many of the project's earlier prompts that passed (text only, each cut short) go in front of the context turns, so a reworded repeat reads as asking again after a new chat or a run of throwaway prompts. Devices send this many. 0 sends none.",
			"20", 0, 60, func(p *policy, f float64) { p.Earlier = int(f) }),
		numSetting("max_devices", "SEATIME_MAX_DEVICES", "Devices per account",
			"Connecting one more is refused until one is revoked in Settings.", "10", 1, 1000, func(p *policy, f float64) { p.MaxDevices = int(f) }),
		{Key: "min_client_version", Env: "SEATIME_MIN_CLIENT_VERSION", Label: "Oldest SeaTime taken",
			Help: "A version like v0.9.0. Older devices are told to update: their syncs are not counted until they do (the time is recounted once they update, up to the recount window), and their checks and submissions are refused. Empty takes every version. Development builds (no version number) are always taken.",
			Def:  func(*Server) string { return envOr("SEATIME_MIN_CLIENT_VERSION", "") },
			Parse: func(_ *Server, p *policy, v string) error {
				v = strings.TrimSpace(v)
				if _, ok := parseVersion(v); v != "" && !ok {
					return fmt.Errorf("%q is not a version like v1.2.3", v)
				}
				p.MinClient = v
				return nil
			}},
		{Key: "admin_ids", Env: "SEATIME_ADMIN_IDS", Label: "Admins", Long: true,
			Help: "Login ids, one per line or comma-separated: hackclub:<sub>, github:<numeric id>, slack:<member id>. Removing someone takes the role away at once and signs them out.",
			Def:  func(s *Server) string { return joinIDs(s.cfg.Admins) },
			Parse: func(_ *Server, p *policy, v string) error {
				p.Admins = idSet(normIDs(v))
				return checkIDs(v)
			}},
	}
}

// numSetting is a number from lo to hi with an env default.
func numSetting(key, env, label, help, def string, lo, hi float64, set func(*policy, float64)) settingDef {
	return settingDef{Key: key, Env: env, Label: label, Help: help,
		Def: func(*Server) string { return envOr(env, def) },
		Parse: func(_ *Server, p *policy, v string) error {
			f, err := parseNum(v, lo, hi)
			if err == nil {
				set(p, f)
			}
			return err
		}}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// pol is the policy in force.
func (s *Server) pol() *policy {
	if p := s.policy.Load(); p != nil {
		return p
	}
	return &policy{}
}

// loadPolicy reads the settings table over the defaults and swaps the
// policy in. A stored value that no longer parses is logged and its
// default used, so a bad row cannot stop the server; a default that does
// not parse is an error (the environment is wrong).
func (s *Server) loadPolicy(ctx context.Context) error {
	stored := map[string]store.Setting{}
	if s.store != nil {
		var err error
		if stored, err = s.store.Settings(ctx); err != nil {
			return err
		}
	}
	p := &policy{src: map[string]string{}}
	for _, d := range settingDefs {
		v := d.Def(s)
		if st, ok := stored[d.Key]; ok {
			if err := d.Parse(s, p, st.Value); err == nil {
				p.src[d.Key] = st.Value
				continue
			} else {
				s.log.Error("setting no longer valid; using its default", "key", d.Key, "err", err)
			}
		}
		if err := d.Parse(s, p, v); err != nil {
			return fmt.Errorf("%s (default for setting %s): %w", d.Env, d.Key, err)
		}
		p.src[d.Key] = v
	}
	s.policy.Store(p)
	return nil
}

func settingDefFor(key string) *settingDef {
	for i := range settingDefs {
		if settingDefs[i].Key == key {
			return &settingDefs[i]
		}
	}
	return nil
}

// setSetting validates and stores one setting (nil: back to its default),
// reloads the policy and runs what the change sets off.
func (s *Server) setSetting(ctx context.Context, key string, value *string, by string) error {
	d := settingDefFor(key)
	if d == nil {
		return errors.New("no such setting")
	}
	if value != nil {
		v := strings.TrimSpace(strings.ReplaceAll(*value, "\r\n", "\n"))
		if err := d.Parse(s, &policy{}, v); err != nil {
			return err
		}
		value = &v
	}
	before := s.pol()
	if err := s.store.PutSetting(ctx, key, value, by, s.now()); err != nil {
		return err
	}
	if err := s.loadPolicy(ctx); err != nil {
		return err
	}
	s.log.Info("setting changed", "key", key, "by", by, "reset", value == nil)
	return s.policyChanged(ctx, before, s.pol())
}

// policyChanged runs what a settings change sets off (the switch's history,
// roles). It runs at start too, with before empty.
func (s *Server) policyChanged(ctx context.Context, before, after *policy) error {
	for _, f := range s.onPolicy {
		if err := f(ctx, before, after); err != nil {
			return err
		}
	}
	return nil
}

// ---------------- /admin ----------------

type settingRow struct {
	Def     *settingDef
	Value   string
	Default string
	Set     *store.Setting // nil: the default is in force
}

func (s *Server) settingRows(ctx context.Context) []settingRow {
	stored, _ := s.store.Settings(ctx)
	p := s.pol()
	var out []settingRow
	for i := range settingDefs {
		d := &settingDefs[i]
		r := settingRow{Def: d, Value: p.src[d.Key], Default: d.Def(s)}
		if st, ok := stored[d.Key]; ok {
			r.Set = &st
		}
		out = append(out, r)
	}
	return out
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	if u.Role != store.RoleAdmin {
		s.notFound(w, r)
		return
	}
	key := r.PostFormValue("key")
	var value *string
	if r.PostFormValue("action") != "reset" {
		v := r.PostFormValue("value")
		value = &v
		// an empty admin list would leave nobody able to reach /admin to undo it
		if key == "admin_ids" && len(idSet(normIDs(v))) == 0 {
			http.Error(w, "not saved: the admin list cannot be empty", 400)
			return
		}
	}
	if err := s.setSetting(r.Context(), key, value, u.Username); err != nil {
		http.Error(w, "not saved: "+err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/admin?ok=1#settings", http.StatusSeeOther)
}

// ---------------- parsing ----------------

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "1", "true", "yes":
		return true, nil
	case "off", "0", "false", "no", "":
		return false, nil
	}
	return false, fmt.Errorf("%q is not on or off", v)
}

func parseNum(v string, lo, hi float64) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < lo || f > hi {
		return 0, fmt.Errorf("%q is not a number from %g to %g", v, lo, hi)
	}
	return f, nil
}

// normIDs turns one id per line into idSet's comma form.
func normIDs(v string) string { return strings.NewReplacer("\n", ",", " ", ",").Replace(v) }

// checkIDs refuses an entry idSet would drop silently (no provider or
// subject), so a typo does not quietly remove someone.
func checkIDs(v string) error {
	for _, p := range strings.Split(normIDs(v), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if prov, sub, ok := strings.Cut(p, ":"); !ok || strings.TrimSpace(prov) == "" || strings.TrimSpace(sub) == "" {
			return fmt.Errorf("%q is not provider:subject", p)
		}
	}
	return nil
}

func joinIDs(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return strings.Join(ks, "\n")
}

var modelRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,120}$`)

// parseModels reads the judge list: one "model [address]" per line (or
// comma-separated). An address must be one SEATIME_PROMPT_CHECK_URL lists:
// the key is sent to it, so it cannot be set from a web page.
func (s *Server) parseModels(v string) ([]promptcheck.Target, error) {
	var out []promptcheck.Target
	for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == ',' }) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) > 2 || !modelRe.MatchString(f[0]) {
			return nil, fmt.Errorf("%q is not \"model\" or \"model address\"", strings.TrimSpace(line))
		}
		t := promptcheck.Target{Model: f[0], URL: s.judgeURLs()[0]}
		if len(f) == 2 {
			u := strings.TrimRight(f[1], "/")
			if !slices.Contains(s.judgeURLs(), u) {
				return nil, fmt.Errorf("%s is not one of SEATIME_PROMPT_CHECK_URL's addresses", u)
			}
			t.URL = u
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("name at least one model")
	}
	if len(out) > 5 {
		return nil, errors.New("at most 5 models")
	}
	return out, nil
}

// judgeURLs are the decisions API addresses the environment allows, the
// first being the default.
func (s *Server) judgeURLs() []string {
	var out []string
	for _, u := range strings.Split(s.cfg.PromptCheckURL, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		out = []string{promptcheck.DecisionsURL}
	}
	return out
}

// ---------------- judge health (shown on /admin) ----------------

// judgeHealth is what /admin says about the judge since this process
// started: the last answer, and the failures of the last hour.
type judgeHealth struct {
	lastOK   time.Time
	lastErr  string
	failures []time.Time
}

func (s *Server) judgeOK() {
	s.healthMu.Lock()
	s.health.lastOK = s.now()
	s.healthMu.Unlock()
}

// judgeFailed records a failed call. err never quotes the request
// (promptcheck.Client keeps text out of its errors).
func (s *Server) judgeFailed(err error) {
	now := s.now()
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	s.health.lastErr = err.Error()
	s.health.failures = append(slices.DeleteFunc(s.health.failures, func(t time.Time) bool { return now.Sub(t) > time.Hour }), now)
}

func (s *Server) judgeStatus() (lastOK time.Time, lastErr string, failsHour int) {
	now := s.now()
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	for _, t := range s.health.failures {
		if now.Sub(t) <= time.Hour {
			failsHour++
		}
	}
	return s.health.lastOK, s.health.lastErr, failsHour
}

// checkStatusView is the prompt check's state as /admin says it, in plain
// text: is it on, can it answer, and how much is waiting.
type checkStatusView struct {
	On          bool
	KeySet      bool
	Models      []promptcheck.Target
	LastOK      time.Time
	LastErr     string
	FailsHour   int
	PendingMin  int
	PendingUser int
	CallsToday  int
	FailsToday  int
	SpentToday  float64
	Budget      float64
	// Versions are the devices that synced in the last 7 days, by SeaTime
	// version: switch the check on once they have the release that can do
	// it. MinClient is the oldest version taken.
	Versions  []store.VersionCount
	MinClient string
	TooOld    int
}

func (s *Server) checkStatus(ctx context.Context) checkStatusView {
	p := s.pol()
	v := checkStatusView{On: p.PromptCheck, KeySet: s.cfg.PromptCheckKey != "", Models: p.Models}
	v.LastOK, v.LastErr, v.FailsHour = s.judgeStatus()
	v.Budget, v.MinClient = p.BudgetUSDDay, p.MinClient
	v.Versions, _ = s.store.ClientVersions(ctx, s.now().Add(-7*24*time.Hour))
	for _, c := range v.Versions {
		if s.clientTooOld(c.Version) {
			v.TooOld += c.Devices
		}
	}
	v.CallsToday, v.FailsToday, v.SpentToday, _ = s.store.CheckSpendDay(ctx, utcDay(s.now()))
	if sec, users, err := s.store.PendingAll(ctx, s.now().Add(-24*time.Hour)); err == nil {
		v.PendingMin, v.PendingUser = int(sec/60+0.5), users
	}
	return v
}

// settingChangeView is one line of the settings log for /admin.
type settingChangeView struct {
	store.SettingChange
	Label string
}

func (s *Server) settingsLog(ctx context.Context) []settingChangeView {
	log, _ := s.store.SettingsLog(ctx, 20)
	out := make([]settingChangeView, len(log))
	for i, c := range log {
		out[i] = settingChangeView{SettingChange: c, Label: c.Key}
		if d := settingDefFor(c.Key); d != nil {
			out[i].Label = d.Label
		}
	}
	return out
}

// parseVersion reads "v1.2.3" or "1.2.3" (anything after the third
// number, like "-rc1", is ignored).
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			return out, false
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// clientTooOld reports whether a device's SeaTime is older than the
// setting min_client_version. A version with no number (a development
// build, "dev") is taken; an empty one (a client from before versions were
// read) is not, once a minimum is set.
func (s *Server) clientTooOld(version string) bool {
	minV, ok := parseVersion(s.pol().MinClient)
	if !ok {
		return false
	}
	if version == "" {
		return true
	}
	v, ok := parseVersion(version)
	if !ok {
		return false
	}
	for i := range v {
		if v[i] != minV[i] {
			return v[i] < minV[i]
		}
	}
	return false
}

// UpdateClient is what a device older than min_client_version is told.
func (s *Server) updateClientMsg() string {
	return "This server needs SeaTime " + s.pol().MinClient + " or newer, so your new time is not being counted yet. Run `seatime update` (or reinstall SeaTime); your time since your last counted sync is recounted when you do."
}

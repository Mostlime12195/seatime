// Package api holds client⇄server wire types. JSON over HTTPS, bearer token auth
// (Authorization: Bearer <device token>).
package api

import (
	"strconv"
	"time"

	"seatime/pkg/chain"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// ---- device login (OAuth-style device code flow) ----

// POST /api/v1/device/start {DeviceStartReq} -> DeviceStartResp
type DeviceStartReq struct {
	DeviceName string `json:"device_name"`
	OS         string `json:"os"`
	PublicKey  string `json:"public_key"` // base64 ed25519
	// Relink, when a device the server stopped accepting signs in again:
	// its old device id, the time of signing and a signature with its key
	// over RelinkMessage. The owner can then approve it on the web, and the
	// device keeps its id and history. An older server ignores them.
	PrevDeviceID string `json:"prev_device_id,omitempty"`
	PrevSignedAt int64  `json:"prev_signed_at,omitempty"` // unix seconds
	PrevSig      string `json:"prev_sig,omitempty"`       // base64 ed25519
}

// RelinkMessage is what a device signs to sign in again as itself.
func RelinkMessage(prevDeviceID, publicKey string, signedAt int64) string {
	return "seatime-relink\n" + prevDeviceID + "\n" + publicKey + "\n" + strconv.FormatInt(signedAt, 10)
}

type DeviceStartResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`        // shown to user, e.g. "WXYZ-1234"
	VerificationURL string `json:"verification_url"` // user opens this in a browser
	Interval        int    `json:"interval"`         // poll seconds
	ExpiresIn       int    `json:"expires_in"`
}

// POST /api/v1/device/poll {DevicePollReq} -> DevicePollResp (202 while pending)
type DevicePollReq struct {
	DeviceCode string `json:"device_code"`
}
type DevicePollResp struct {
	Token    string `json:"token"`
	DeviceID string `json:"device_id"`
	Username string `json:"username"`
}

// ---- sync ----

// POST /api/v1/sync {SyncReq} -> SyncResp
// Buckets are the one-page rule's minutes (pkg/ledger). When From and To are
// set, the buckets are this device's complete credit for [From, To): the
// server replaces what the device sent for that window before, so a minute
// that later events moved to another project or removed (an idle record
// written after the fact) does not linger. Without them buckets are upserted
// by (user, project, start).
type SyncReq struct {
	DeviceID      string           `json:"device_id"`
	ClientVersion string           `json:"client_version"`
	ScoreVersion  string           `json:"score_version"`
	Checkpoint    chain.Checkpoint `json:"checkpoint"`
	CheckpointSig string           `json:"checkpoint_sig"` // base64 ed25519 over "seatime-checkpoint\n<device_id>\n<seq>\n<hash>"
	From          time.Time        `json:"from,omitzero"`
	To            time.Time        `json:"to,omitzero"`
	Buckets       []ledger.Bucket  `json:"buckets"`
	Projects      []ProjectMeta    `json:"projects,omitempty"`
	Commits       []CommitRef      `json:"commits,omitempty"`
	// Pending, only while the prompt check is on and only in a sync's last
	// request: per project, the seconds of the last 24 hours that wait for
	// the check. It replaces what this device sent before.
	Pending *PendingTime `json:"pending,omitempty"`
}

// PendingTime is time that waits for the prompt check, by project.
type PendingTime struct {
	Projects []ProjectPending `json:"projects,omitempty"`
}

type ProjectPending struct {
	Project string  `json:"project"`
	Seconds float64 `json:"seconds"`
}

// ProjectMeta describes a project key seen by the client. Remote is only sent
// when it is a public-looking forge URL; otherwise the key is "local:<sha256
// prefix>". Name is the name the user chose for the project, a public
// repo's name, or else the folder's own name (Folder is then true): the
// name of the folder, never its path. The server shows a folder's name
// only for a project nobody has named.
//
// A device keeps a folder's key for good. Label is what the folder goes by
// now, in the form of a key: its git remote, else its name. It is the key
// until a remote is added or changes; then the server lets the project go
// by the label (internal/server/store projectkeys.go). A device older than
// v0.1.10 sends none: its key changes with the remote.
type ProjectMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label,omitempty"`
	Name   string `json:"name,omitempty"`
	Folder bool   `json:"folder,omitempty"`
	Remote string `json:"remote,omitempty"` // e.g. https://github.com/user/repo
}

type CommitRef struct {
	Project string    `json:"project"`
	SHA     string    `json:"sha"`
	TS      time.Time `json:"ts"`
	Ins     int       `json:"ins"`
	Del     int       `json:"del"`
}

type SyncResp struct {
	OK         bool      `json:"ok"`
	ServerTime time.Time `json:"server_time"`
	Message    string    `json:"message,omitempty"` // shown to user by `seatime status`
	// PromptCheck says the server's prompt check is switched on: the device
	// then commits a hash of each prompt that is not a repeat and sends the
	// finished turn for its check (POST /api/v1/prompt-check).
	PromptCheck bool `json:"prompt_check,omitempty"`
	// CheckTimeoutS is how long the server may take over one prompt check
	// (its setting prompt_check_timeout_s); the device waits that long plus
	// a margin. 0: an older server (the device's default).
	CheckTimeoutS int `json:"check_timeout_s,omitempty"`
	// CheckPeriods is the switch's history: the device counts what happened
	// while the check was off with heartbeat/1 and the rest with heartbeat/2,
	// as the server does (pkg/promptcheck Period).
	CheckPeriods []promptcheck.Period `json:"check_periods,omitempty"`
	// QueueDays is how long a device keeps a turn waiting for its check
	// before it drops the text and leaves the prompt pending (setting
	// prompt_check_queue_days). RecountDays is how far back a verdict that
	// came back late may recount synced time (prompt_check_late_days).
	QueueDays   float64 `json:"queue_days,omitempty"`
	RecountDays float64 `json:"recount_days,omitempty"`
	// EarlierPrompts is how many earlier prompts that passed a check request
	// should carry (setting prompt_check_earlier); -1 for none.
	EarlierPrompts int `json:"earlier_prompts,omitempty"`
	PromptChainResp
}

// ---- prompt check (pkg/promptcheck; only while the switch is on) ----

// POST /api/v1/prompt-check {PromptCheckReq} -> PromptCheckResp
// The device sends one when a turn ends (the AI stops, the person sends
// the next prompt, or the session ends). The text in it, and what it says
// about the project (Project), is used for the check and dropped: the
// server keeps only the labels, the prompt hash and the model's name.
type PromptCheckReq struct {
	DeviceID string `json:"device_id"`
	// CheckHash is the prompt hash the log committed to when the prompt was
	// sent (event.Event.CheckHash); Salt is the salt behind it. The server
	// refuses the request unless promptcheck.Hash(Salt, Turn.Prompt) is
	// CheckHash.
	CheckHash string `json:"check_hash"`
	Salt      string `json:"salt"`
	// Turn is the turn judged; Context are up to promptcheck.MaxContextTurns
	// turns before it in the same session, oldest first, as context only.
	Turn    promptcheck.Turn   `json:"turn"`
	Context []promptcheck.Turn `json:"context,omitempty"`
	// Earlier are prompts of the project before the context turns that
	// passed their check, oldest first: text, prompt hash and salt only.
	Earlier []promptcheck.Turn `json:"earlier,omitempty"`
	// Project is what the device read in the project's folder when the
	// prompt was sent: its name, some file names and the top of its README.
	// A device older than v0.1.8 sends none, and is judged without it.
	Project *promptcheck.Project `json:"project,omitempty"`
	// GitIns and GitDel are the line counts git logged at the turn's end
	// (the agent stop's uncommitted changes against HEAD); GitKnown is false
	// when nothing was logged (the person interrupted the AI). The server
	// checks the diff excerpt against them.
	GitIns   int  `json:"git_ins,omitempty"`
	GitDel   int  `json:"git_del,omitempty"`
	GitKnown bool `json:"git_known,omitempty"`
	// FrontMeasured says the activity line's time with the AI tool's window
	// in front was measured. Without it the server does not ask the
	// question that needs it (Rules.Unbacked) and takes it as no.
	FrontMeasured bool `json:"front_measured,omitempty"`
	// PromptTS is when the prompt was sent (its record's time): a verdict
	// that comes back late may then raise the synced minutes it covers.
	PromptTS time.Time `json:"prompt_ts,omitzero"`
}

type PromptCheckResp struct {
	// CheckHash says that the prompt was checked. Whether it counts, and
	// for how long, stays on the server: the server counts a prompt's time
	// itself when the records of that stretch of work arrive (segments).
	CheckHash string `json:"check_hash"`
}

// ---- records: the device's log, sent as it is written ----

// POST /api/v1/records {RecordsReq} -> RecordsResp
// After each sync the device sends the records of its chain that the
// server does not hold yet, up to the checkpoint that sync carried. Every
// record of a project, and every idle-timer record, goes in its private
// form (chain.Hide: text, commands, paths, folder and extras as
// commitments, the project as its server key); anything else is a bare
// leaf hash. The server keeps what replays to a checkpoint it holds, and
// counts each project's time from it on its own clock (internal/server
// records.go), whether or not the computer is on.
type RecordsReq struct {
	DeviceID string `json:"device_id"`
	// StartHash is the chain head before Entries[0].
	StartHash string        `json:"start_hash,omitempty"`
	Entries   []chain.Entry `json:"entries,omitempty"`
	// From is the seq of the first record the device has to send. A request
	// with no entries asks where to go on from.
	From int64 `json:"from,omitempty"`
}

// MaxRecords is the most entries one RecordsReq may carry.
const MaxRecords = 100000

// RecordsResp says how far the server holds the device's records. It is
// also the body of a 409, for records that do not go on from Head.
type RecordsResp struct {
	// Head is the seq of the last record the server holds (0: none).
	Head int64 `json:"head"`
	// Next is the seq to send up to next: a checkpoint the server holds.
	// 0: nothing to send until the next sync.
	Next int64 `json:"next,omitempty"`
}

// ---- segments: a stretch of a project's records, from a device ----

// POST /api/v1/segments {Bundle} -> SegmentResp
// A device older than v0.1.14, which does not send its records as they
// are written, uploads a project's records once a stretch of work on it is
// over: a contiguous seq range of its chain, ending at a checkpoint the
// server holds. Records of the project and the idle-timer records in the
// range are revealed in their private form (chain.Hide: text, commands,
// paths, folder and extras as commitments, the project as its server key);
// every other record is a bare leaf hash. The server checks it against the
// device's checkpoints and counts it with the one-page rule.
type Bundle struct {
	DeviceID     string        `json:"device_id"`
	Project      string        `json:"project"`
	StartHash    string        `json:"start_hash"` // chain head before Entries[0]
	Entries      []chain.Entry `json:"entries"`
	ScoreVersion string        `json:"score_version"`
	Claimed      ledger.Bucket `json:"claimed"` // the device's totals for the project (pkg/audit.Claim)
	// Others are bare times of counted heartbeats in other projects inside
	// the range (no project, no content): each gap goes to the project of
	// the heartbeat that ends it, so the server needs them to split time as
	// the device does. A marker can only shorten the project's gaps.
	Others []time.Time `json:"others,omitempty"`
	// OthersUntil, only under the cap rule (the prompt check on), is for
	// each of Others how far that heartbeat counted forward (zero when it
	// did not): a prompt's minutes, as the device cut them.
	OthersUntil []time.Time `json:"others_until,omitempty"`
	Sig         string      `json:"sig"` // base64 ed25519 over sha256 of canonical JSON of the bundle with Sig=""
}

type SegmentResp struct {
	ID string `json:"id"`
	// Status is verified, pending (prompts wait for their check) or failed
	// (Problems says why).
	Status   string        `json:"status"`
	Problems []string      `json:"problems,omitempty"`
	Scored   ledger.Bucket `json:"scored"` // the server's count, after deductions
	Hours    float64       `json:"hours"`  // credited hours; 0 when failed
	Pending  int           `json:"pending,omitempty"`
}

// ---- what applications read (OAuth bearer token, or public) ----

// Status of a project's hours, as every response that carries hours says.
const (
	StatusVerified = "verified" // every record that counts matched the device's hash chain
	StatusPending  = "pending"  // some time is not checked yet (see status_reason)
	StatusFailed   = "failed"   // some records did not match (see status_reason); they do not count
)

// GET /api/v1/users/{user}/projects/{project}/ysws (JSON) and /ysws.txt
// (the Text field alone): what goes into a YSWS form or the Unified YSWS DB.
type YSWSExport struct {
	User    string `json:"user"`
	Project string `json:"project"`
	// Status and StatusReason are the project's status; AsOf is the time
	// up to which its hours are checked.
	Status       string    `json:"status"`
	StatusReason string    `json:"status_reason,omitempty"`
	AsOf         time.Time `json:"as_of"`

	// Hours is THE number for the hours field: the hours the server counted
	// from records that matched the hash chain.
	Hours         float64 `json:"hours"`
	DarkFlowHours float64 `json:"dark_flow_hours"`
	// PendingHours is synced time that is not checked yet (not in Hours).
	PendingHours float64 `json:"pending_hours"`
	// OverrideHoursJustification is a short generated sentence for an
	// "override hours" justification field.
	OverrideHoursJustification string `json:"override_hours_justification"`

	Commits     int              `json:"commits"`
	CommitRange *YSWSCommitRange `json:"commit_range,omitempty"`
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`

	CodeURL   string `json:"code_url,omitempty"` // GitHub repo, when known
	ShareURL  string `json:"share_url"`          // the project's public page
	ExportURL string `json:"export_url"`

	// Text is the copy-paste block (also served as text/plain at /ysws.txt).
	Text string `json:"text"`
}

type YSWSCommitRange struct {
	First      string    `json:"first"`
	Last       string    `json:"last"`
	Count      int       `json:"count"`
	FirstTime  time.Time `json:"first_time"`
	LastTime   time.Time `json:"last_time"`
	CompareURL string    `json:"compare_url,omitempty"`
}

// ---- public stats (for YSWS programs / Unified DB) ----

// GET /api/v1/users/{user}/projects/{project}/stats?since=RFC3339&until=RFC3339
type ProjectStats struct {
	User    string `json:"user"`
	Project string `json:"project"`
	// Status, StatusReason and AsOf as in YSWSExport.
	Status       string    `json:"status"`
	StatusReason string    `json:"status_reason,omitempty"`
	AsOf         time.Time `json:"as_of"`
	// HumanSeconds and StruggleSeconds are the same counted time (checked
	// against the hash chain); DarkFlowSeconds the AI time not counted.
	HumanSeconds    float64  `json:"human_seconds"`
	StruggleSeconds float64  `json:"struggle_seconds"`
	DarkFlowSeconds float64  `json:"dark_flow_seconds"`
	PendingSeconds  float64  `json:"pending_seconds"`
	Commits         int      `json:"commits"`
	CommitList      []Commit `json:"commit_list,omitempty"`
	// Models splits HumanSeconds, and the range's prompts, by AI model. It
	// is served to the user's own tokens only: an application the user
	// approved, or one of their devices. The rows' seconds add up to
	// HumanSeconds.
	Models []ModelTime `json:"models,omitempty"`
}

// The kinds of a ModelTime row.
const (
	ModelNamed       = "model"        // time and prompts with the model named in Model
	ModelHand        = "hand"         // time from hand edits: no model
	ModelNotRecorded = "not_recorded" // AI time and prompts whose records name no model, and all time checked before models were kept
)

// ModelTime is checked seconds and prompts with one model. Model is the
// name as the AI tool reported it, unchanged.
type ModelTime struct {
	Kind    string  `json:"kind"`
	Model   string  `json:"model,omitempty"`
	Seconds float64 `json:"seconds"`
	Prompts int     `json:"prompts"`
}

// Commit is one commit of a project, as applications see it.
type Commit struct {
	SHA string    `json:"sha"`
	TS  time.Time `json:"ts"`
	Ins int       `json:"ins"`
	Del int       `json:"del"`
}

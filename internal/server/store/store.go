// Package store defines seatime's persistence interface and its implementation
// on SQLite or Postgres (DB).
package store

import (
	"context"
	"errors"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

var (
	ErrNotFound   = errors.New("store: not found")
	ErrConflict   = errors.New("store: conflict")
	ErrRegression = errors.New("store: checkpoint seq regression")
	ErrExpired    = errors.New("store: expired")
)

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type User struct {
	ID          int64
	Username    string
	DisplayName string
	Avatar      string
	SlackID     string
	GitHub      string
	Public      bool
	Role        string
	TZOffsetMin int // display offset from UTC in minutes
	Created     time.Time
	// SessionEpoch is carried in each session cookie; a cookie with an older
	// epoch is not a login (EndSessions, SetRole).
	SessionEpoch int64
	// Deleted: the owner deleted the account (DeleteAccount); zero while it
	// is live. A deleted account has no data, only computers still to
	// remove SeaTime.
	Deleted time.Time
}

// Identity is an external login (provider "hackclub" | "github" | "dev").
type Identity struct {
	Provider, Subject string
}

type Device struct {
	ID        string
	UserID    int64
	Name      string
	OS        string
	PublicKey string    // base64 ed25519
	LastSeen  time.Time // the last accepted sync
	// ClientVersion is the SeaTime version of its last sync ("" before
	// versions were kept).
	ClientVersion string
	Created       time.Time
	Revoked       bool
}

type DeviceCode struct {
	UserCode   string
	DeviceName string
	OS         string
	PublicKey  string
	Expires    time.Time
	UserID     int64 // 0 until approved
	// PrevDevice is the device this sign-in reconnects ("" for a new one):
	// the request was signed with that device's key.
	PrevDevice string
}

type Project struct {
	ID      int64
	UserID  int64
	Key     string
	Slug    string
	Name    string
	Remote  string
	Repo    string // "owner/repo" on GitHub, "" if unlinked
	Hidden  bool
	Decided bool // the owner has chosen hidden or public (SetProjectHidden)
	Named   bool // the owner named it on the web (SetProjectName)
	Created time.Time
}

type Checkpoint struct {
	Seq      int64
	Hash     string
	ServerTS time.Time
}

type Commit struct {
	ProjectID int64
	SHA       string
	TS        time.Time
	Ins, Del  int
}

type DaySum struct {
	Day                           int64 // days since epoch in the user's offset
	Struggle, DarkFlow, Human     float64
	Prompts, Edits, Commits, Runs int
}

type ProjectSum struct {
	Project                   *Project
	Struggle, DarkFlow, Human float64
	LastStart                 time.Time
}

type LeaderRow struct {
	Username, DisplayName, Avatar string
	Struggle, DarkFlow            float64
}

type GlobalStats struct {
	TodayStruggle, WeekStruggle, AllStruggle float64
	Users, ActiveNow                         int
}

// PromptTime is when a prompt that passed the prompt check was sent, its
// kind and the minutes its verdict gave it.
type PromptTime struct {
	TS      time.Time
	Kind    string
	Minutes float64
}

// VersionCount is how many devices sync with one SeaTime version.
type VersionCount struct {
	Version string
	Devices int
}

// PromptPass is a stored decision for one prompt hash.
type PromptPass struct {
	Pass    bool
	Kind    string
	Minutes float64
}

type Store interface {
	Close() error
	// Ping is one trivial read (the health check).
	Ping(ctx context.Context) error

	// users & identities
	LoginIdentity(ctx context.Context, id Identity, wantUsername, display, avatar, slackID, github string) (*User, error)
	LinkIdentity(ctx context.Context, userID int64, id Identity, github string) error
	UserByID(ctx context.Context, id int64) (*User, error)
	UserByName(ctx context.Context, name string) (*User, error)
	// SearchUsers finds live accounts whose username, display name, Slack
	// member ID or GitHub login contains q, whatever its case. An account
	// whose username is q comes first, then the one that tracked most recently.
	SearchUsers(ctx context.Context, q string, limit int) ([]User, error)
	// UserBySlackID matches the Slack member ID exactly (case-sensitive, e.g. "U0123ABCD").
	UserBySlackID(ctx context.Context, slackID string) (*User, error)
	// UserByIdentity finds the user behind an external login (provider "hackclub" = Hack Club Auth, subject = its stable id).
	UserByIdentity(ctx context.Context, provider, subject string) (*User, error)
	// SetSlackID records a Slack ID for a user that has none yet.
	SetSlackID(ctx context.Context, userID int64, slackID string) error
	UpdateUser(ctx context.Context, u *User) error
	SetRole(ctx context.Context, userID int64, role string) error // also ends the user's sessions
	EndSessions(ctx context.Context, userID int64) error
	// UserIdentities lists the external logins linked to a user.
	UserIdentities(ctx context.Context, userID int64) ([]Identity, error)
	// StaffUsers lists every user whose role is not RoleUser.
	StaffUsers(ctx context.Context) ([]User, error)

	// devices
	CreateDeviceCode(ctx context.Context, deviceCodeHash string, dc DeviceCode) error
	DeviceCodeByUserCode(ctx context.Context, userCode string) (*DeviceCode, error)
	ApproveDeviceCode(ctx context.Context, userCode string, userID int64) error
	// ConsumeDeviceCode returns the approved code and deletes it; ErrNotFound if
	// unknown, ErrExpired if expired, and (dc, nil) with dc.UserID==0 if pending.
	ConsumeDeviceCode(ctx context.Context, deviceCodeHash string) (*DeviceCode, error)
	CreateDevice(ctx context.Context, d Device, tokenHash string) error
	DeviceByTokenHash(ctx context.Context, tokenHash string) (*Device, error)
	DeviceByID(ctx context.Context, id string) (*Device, error)
	ListDevices(ctx context.Context, userID int64) ([]Device, error)
	RevokeDevice(ctx context.Context, userID int64, id string) error
	TouchDevice(ctx context.Context, id string, t time.Time) error
	SetClientVersion(ctx context.Context, id, version string) error
	ClientVersions(ctx context.Context, since time.Time) ([]VersionCount, error)

	// projects. DeviceProject is the one place that decides which project
	// a device's key belongs to (projectkeys.go); UpsertProject finds or
	// makes the project that goes by a key now.
	DeviceProject(ctx context.Context, userID int64, deviceID string, m api.ProjectMeta) (*Project, error)
	// ProjectKeys are the keys devices have sent for a project.
	ProjectKeys(ctx context.Context, projectID int64) ([]ProjectKey, error)
	UpsertProject(ctx context.Context, userID int64, m api.ProjectMeta) (*Project, error)
	ProjectByKey(ctx context.Context, userID int64, key string) (*Project, error)
	// ProjectBySlug also finds a project by a key it went by before, by a
	// key a device sent for it and by the slug of a project folded into it.
	ProjectBySlug(ctx context.Context, userID int64, slugOrKey string) (*Project, error)
	ProjectByID(ctx context.Context, id int64) (*Project, error)
	ListProjects(ctx context.Context, userID int64) ([]Project, error)
	// EmptyProjects are the ids of the user's projects with no counted
	// time, no checked time and no commits.
	EmptyProjects(ctx context.Context, userID int64) (map[int64]bool, error)
	// SetProjectHidden also records that the owner has decided (Project.Decided).
	SetProjectHidden(ctx context.Context, userID, projectID int64, hidden bool) error
	SetProjectRepo(ctx context.Context, userID, projectID int64, repo string) error

	// buckets
	UpsertBuckets(ctx context.Context, userID int64, deviceID string, bs []ledger.Bucket) error
	// DeleteBuckets drops what one device sent for minutes in [from, to),
	// except the (project, minute) keys in keep (a sync that resends its
	// whole window replaces it).
	DeleteBuckets(ctx context.Context, userID int64, deviceID string, from, to time.Time, keep []ledger.Bucket) error
	// DeviceBuckets returns one device's rows for minutes in [from, to).
	DeviceBuckets(ctx context.Context, userID int64, deviceID string, from, to time.Time) ([]ledger.Bucket, error)
	DaySums(ctx context.Context, userID int64, projectID int64, fromDay, toDay int64, offsetMin int) ([]DaySum, error)
	Buckets(ctx context.Context, userID, projectID int64, from, to time.Time) ([]ledger.Bucket, error)
	ProjectSums(ctx context.Context, userID int64, from, to time.Time) ([]ProjectSum, error)
	// ModelSums splits ProjectSums' counted seconds, and the prompts, by model.
	ModelSums(ctx context.Context, userID, projectID int64, from, to time.Time) ([]ModelSum, error)
	Leaderboard(ctx context.Context, from time.Time, limit int) ([]LeaderRow, error)
	Global(ctx context.Context, now time.Time) (GlobalStats, error)

	// checkpoints
	AddCheckpoint(ctx context.Context, deviceID string, c chain.Checkpoint, at time.Time) error
	Checkpoints(ctx context.Context, deviceID string, lo, hi int64) ([]Checkpoint, error)
	CheckpointAt(ctx context.Context, deviceID string, seq int64) (*Checkpoint, error)
	// FirstCheckpointFrom returns the first checkpoint with Seq >= seq.
	FirstCheckpointFrom(ctx context.Context, deviceID string, seq int64) (*Checkpoint, error)
	// CheckpointBefore returns the last checkpoint with Seq < seq.
	CheckpointBefore(ctx context.Context, deviceID string, seq int64) (*Checkpoint, error)

	// commits
	UpsertCommits(ctx context.Context, cs []Commit) error
	Commits(ctx context.Context, projectID int64, from, to time.Time) ([]Commit, error)
	CommitCount(ctx context.Context, projectID int64, from, to time.Time) (int, error)

	// chains sent twice (claims.go)
	CheckpointOwners(ctx context.Context, hashes []string, excludeDevice string) ([]DeviceOwner, error)
	UserMinuteHuman(ctx context.Context, userID int64, fromMin, toMin int64) (map[int64]map[string]float64, error)

	// Hackatime's trust colour (trust.go), never read by the count
	HackatimeTrust(ctx context.Context, slackID string) (*HackatimeTrust, error) // ErrNotFound if never asked
	PutHackatimeTrust(ctx context.Context, t HackatimeTrust) error
	// settings an admin changes on /admin (settings.go)
	Settings(ctx context.Context) (map[string]Setting, error)
	PutSetting(ctx context.Context, key string, value *string, by string, at time.Time) error
	SettingsLog(ctx context.Context, limit int) ([]SettingChange, error)

	// prompt check (promptcheck.go): verdicts only, never text
	PromptVerdict(ctx context.Context, deviceID, checkHash string) (*promptcheck.Verdict, error) // ErrNotFound if none
	PutPromptVerdict(ctx context.Context, deviceID, checkHash string, v promptcheck.Verdict, promptTS, at time.Time, ev promptcheck.Evidence) error
	// PassingPrompts are when this device's passing prompts in [from, to)
	// were sent, with their kinds.
	PassingPrompts(ctx context.Context, deviceID string, from, to time.Time) ([]PromptTime, error)
	// SetPending replaces what a device reported waiting for the prompt
	// check, by the device's project keys; Pending sums a user's reports
	// since a time, for one project or all (0).
	SetPending(ctx context.Context, userID int64, deviceID string, byProject map[string]float64, at time.Time) error
	Pending(ctx context.Context, userID, projectID int64, since time.Time) (float64, error)
	PendingAll(ctx context.Context, since time.Time) (seconds float64, users int, err error)
	// the prompt check switch's history (periods.go in pkg/promptcheck)
	RulePeriods(ctx context.Context) ([]promptcheck.Period, error)
	AddRulePeriod(ctx context.Context, p promptcheck.Period, by string) error
	// paid judge calls per UTC day (YYYY-MM-DD) and account
	AddCheckSpend(ctx context.Context, day string, userID int64, cost float64, failed bool) error
	CheckSpend(ctx context.Context, day string, userID int64) (int, error)
	CheckSpendDay(ctx context.Context, day string) (calls, fails int, cost float64, err error)
	// PromptPasses returns the stored decision and kind for each of hashes
	// that has a verdict.
	PromptPasses(ctx context.Context, deviceID string, hashes []string) (map[string]PromptPass, error)

	PromptChain     // promptchain.go
	DeviceRequests  // devicereq.go
	Segments        // segments.go
	OAuth           // oauth.go
	AccountDeletion // deleteaccount.go
}

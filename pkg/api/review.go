package api

import (
	"time"

	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// GET /api/v1/review/users/{user}/projects/{project}?since=RFC3339&until=RFC3339
//
// What a Hack Club review tool reads about one project, with a review key
// (Authorization: Bearer): the counted hours and their status, as
// applications see them, and under them the checked records themselves,
// segment by segment, so that a reviewer can see how the hours happened.
//
// It carries numbers, yes-or-no answers and times. It never carries prompt
// text, a command, a folder, or any hash of them: the server does not have
// the first three, and the hashes stay here.
type ReviewEvidence struct {
	User    string `json:"user"`
	SlackID string `json:"slack_id,omitempty"`
	Project string `json:"project"` // the project's key
	Name    string `json:"name"`
	Repo    string `json:"repo,omitempty"`

	Status          string      `json:"status"`
	StatusReason    string      `json:"status_reason,omitempty"`
	AsOf            time.Time   `json:"as_of"`
	HumanSeconds    float64     `json:"human_seconds"`
	DarkFlowSeconds float64     `json:"dark_flow_seconds"`
	PendingSeconds  float64     `json:"pending_seconds"`
	Models          []ModelTime `json:"models"`
	Commits         []Commit    `json:"commits"`

	Devices  []ReviewDevice  `json:"devices"`
	Segments []ReviewSegment `json:"segments"`
	// More is set when the range holds more segments than one answer
	// carries: ask again with since set to the last segment's last_ts.
	More bool `json:"more,omitempty"`
}

// GET /api/v1/review/users?q=
//
// The accounts whose username, display name, Slack member ID or GitHub
// login contains q (2 characters or more), at most 20, the one that
// tracked most recently first.
type ReviewUsers struct {
	Users []ReviewAccount `json:"users"`
}

type ReviewAccount struct {
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Avatar  string `json:"avatar,omitempty"` // the picture's address, from the sign-in provider
	SlackID string `json:"slack_id,omitempty"`
	GitHub  string `json:"github,omitempty"`
}

// GET /api/v1/review/users/{user}
//
// The projects of one account that a review key may read: every project
// its owner has not hidden, with its hours over all time.
type ReviewUser struct {
	User     string          `json:"user"`
	Name     string          `json:"name,omitempty"` // the display name
	Avatar   string          `json:"avatar,omitempty"`
	SlackID  string          `json:"slack_id,omitempty"`
	GitHub   string          `json:"github,omitempty"`
	Projects []ReviewProject `json:"projects"`
}

type ReviewProject struct {
	Key            string    `json:"key"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	Repo           string    `json:"repo,omitempty"`
	Status         string    `json:"status"`
	HumanSeconds   float64   `json:"human_seconds"`
	PendingSeconds float64   `json:"pending_seconds"`
	FirstTS        time.Time `json:"first_ts"` // of its first checked segment; zero when it has none
	LastTS         time.Time `json:"last_ts"`
}

// ReviewDevice is a computer that sent some of the segments. N is its
// number in this answer; segments name their device by it.
type ReviewDevice struct {
	N       int    `json:"n"`
	OS      string `json:"os,omitempty"`
	Version string `json:"version,omitempty"` // of SeaTime, at its last sync
}

// ReviewSegment is one checked stretch of the project's records from one
// device. Minutes are what it was credited; Records are what was counted.
type ReviewSegment struct {
	ID       string    `json:"id"`
	Device   int       `json:"device"`
	Status   string    `json:"status"` // verified, pending or failed
	Problems []string  `json:"problems,omitempty"`
	FirstTS  time.Time `json:"first_ts"`
	LastTS   time.Time `json:"last_ts"`
	Checked  time.Time `json:"checked"`
	// Minutes: [the minute's start in unix seconds, counted seconds, the
	// agent's seconds that did not count]. Empty for a failed segment.
	Minutes [][3]float64   `json:"minutes"`
	Records []ReviewRecord `json:"records"`
}

// ReviewRecord is one record of a segment, without anything that could
// say what the person typed or where their files are. Most fields are the
// record's own (pkg/event Event) under the same names.
type ReviewRecord struct {
	TS      time.Time  `json:"ts"`
	Kind    event.Kind `json:"kind"`
	Harness string     `json:"harness,omitempty"`
	Session int        `json:"session,omitempty"` // the AI session's number in this answer, from 1
	Model   string     `json:"model,omitempty"`

	// a prompt
	Words          int          `json:"words,omitempty"`
	New            *bool        `json:"new,omitempty"`
	Check          *ReviewCheck `json:"check,omitempty"`
	QueuedMS       int64        `json:"queued_ms,omitempty"`
	InputBeforeMS  *int64       `json:"input_before_ms,omitempty"`
	TerminalIdleS  *int         `json:"terminal_idle_s,omitempty"`
	FrontWindow    string       `json:"front_window,omitempty"`
	SessionPrompts int          `json:"session_prompts,omitempty"`
	OtherStopHook  *bool        `json:"other_stop_hook,omitempty"`

	// a tool call or a hand edit
	Tool          string `json:"tool,omitempty"`
	Write         bool   `json:"write,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	Lines         int    `json:"lines,omitempty"`
	LineNo        int    `json:"lineno,omitempty"`
	CursorPos     int    `json:"cursorpos,omitempty"`
	LineAdditions int    `json:"line_additions,omitempty"`
	LineDeletions int    `json:"line_deletions,omitempty"`
	Category      string `json:"category,omitempty"`
	// File is a hand edit's file by its path inside the project's git
	// repository, when the device sent it beside the record.
	File string `json:"file,omitempty"`

	// a commit
	SHA          string `json:"sha,omitempty"`
	Insertions   int    `json:"insertions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	FilesChanged int    `json:"files_changed,omitempty"`

	RunKind string `json:"run_kind,omitempty"`

	// an agent stop
	Run         *event.RunFacts `json:"run,omitempty"`
	Git         *event.GitFacts `json:"git,omitempty"`
	PromptInLog *bool           `json:"prompt_in_log,omitempty"`

	// presence: on prompts and on idle records
	Presence    string     `json:"presence,omitempty"`
	IdleSrc     string     `json:"idle_src,omitempty"`
	WindowMS    int64      `json:"window_ms,omitempty"`
	NoInput     [][2]int64 `json:"no_input,omitempty"`
	InputSlices [][2]int64 `json:"input_slices,omitempty"`
	FrontSlices [][2]int64 `json:"front_slices,omitempty"`

	Backfilled bool `json:"backfilled,omitempty"`
}

// ReviewCheck is the prompt check's stored answer for one prompt: each
// question's yes or no, never the words that were judged. Waiting is set
// instead when the prompt's check has not come back yet. A prompt with no
// check at all was counted by the plain rule.
type ReviewCheck struct {
	Waiting bool               `json:"waiting,omitempty"`
	Pass    bool               `json:"pass"`
	Kind    string             `json:"kind,omitempty"`
	Minutes float64            `json:"minutes,omitempty"`
	Labels  promptcheck.Labels `json:"labels,omitempty"`
	Skipped []string           `json:"skipped,omitempty"`
	Rules   string             `json:"rules,omitempty"`
	Model   string             `json:"model,omitempty"` // the judge
}

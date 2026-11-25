package api

import (
	"time"

	"seatime/pkg/promptcheck"
)

// ---- the prompt chain (pkg/promptcheck chain.go) ----

// POST /api/v1/prompt-hashes {PromptHashesReq} -> PromptHashesResp
// A device sends the hash of every prompt it records as soon as it can,
// with the check on or off, in chain order. Entries the server already
// holds are skipped. When they do not continue the chain the server holds,
// it answers 409 with its head: the device then relinks the entries the
// server has not taken after that head (promptcheck.Relink) and sends
// them again.
type PromptHashesReq struct {
	DeviceID string                   `json:"device_id"`
	Entries  []promptcheck.ChainEntry `json:"entries"`
}

type PromptHashesResp struct {
	Seq   int64  `json:"seq"`  // the server's last entry for the device
	Link  string `json:"link"` // its link
	Taken int    `json:"taken"`
}

// MaxPromptHashes is how many entries one request may carry.
const MaxPromptHashes = 500

// PromptChainResp is what a sync answer adds for the prompt chain. An
// older server sends none of it (Chain 0): the device then counts with
// CheckPeriods and sends checks only while PromptCheck is on, as before.
type PromptChainResp struct {
	// Chain is 1 when the server takes prompt hashes.
	Chain int `json:"prompt_chain,omitempty"`
	// ServerCheck is the server's own switch, ServerPeriods its history,
	// and UserPeriods the history of the account's own setting (on when
	// empty). PromptCheck is both together, now. The device counts with
	// promptcheck.Effective over them, as the server does.
	ServerCheck   bool                 `json:"server_check,omitempty"`
	ServerPeriods []promptcheck.Period `json:"server_periods,omitempty"`
	UserPeriods   []promptcheck.Period `json:"user_periods,omitempty"`
	// Pulls ask the device to send its saved prompts in these ranges for
	// their check, whatever the account's setting.
	Pulls []PromptPull `json:"prompt_pulls,omitempty"`
	// Closed are prompts the server closed without a verdict (their text
	// did not match, or never came): they count as not passing.
	Closed []ClosedPrompt `json:"closed_prompts,omitempty"`
}

// PromptPull asks for the prompts sent in [From, To), of Project (a server
// key) or of every project ("").
type PromptPull struct {
	Project string    `json:"project,omitempty"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
}

// Covers reports whether the pull asks for a prompt of project (a server
// key) sent at ts.
func (p PromptPull) Covers(project string, ts time.Time) bool {
	return (p.Project == "" || p.Project == project) && !ts.Before(p.From) && ts.Before(p.To)
}

type ClosedPrompt struct {
	Commit string `json:"commit"`
	Status string `json:"status"` // "mismatch" or "missing"
}

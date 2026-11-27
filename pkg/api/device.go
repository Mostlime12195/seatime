package api

import "time"

// ---- check-in: device health, account settings and requests from the web ----

// POST /api/v1/device/check-in {CheckIn} -> CheckInResp
// The daemon checks in about once a minute, with or without new activity.
// It says how this computer is doing (shown to its owner on /settings) and
// gets back the account's settings and anything the owner asked this
// computer to do on the web. An older server answers 404: the device then
// checks again much later.
type CheckIn struct {
	DeviceID      string `json:"device_id"`
	ClientVersion string `json:"client_version"`
	// Can lists the request kinds (Request*) this version carries out. The
	// web offers an action only to devices that can do it.
	Can []string `json:"can"`
	// Tools are the AI tools found on this computer and the state of
	// SeaTime's hooks in each.
	Tools []ToolHealth `json:"tools,omitempty"`
	// Problems are fixed codes (Problem*), never free text or a path.
	Problems []Problem `json:"problems,omitempty"`
	// Settings are the values in effect on this computer.
	Settings DeviceSettings `json:"settings"`
}

// ToolHealth is one AI tool on this computer.
type ToolHealth struct {
	Name  string `json:"name"`  // adapter name, such as claude-code
	State string `json:"state"` // ToolHooked, ToolFailed or ToolOptedOut
}

const (
	ToolHooked   = "hooked"
	ToolFailed   = "failed"
	ToolOptedOut = "opted-out"
)

// Problem is one thing wrong on this computer, as a fixed code and, when it
// is about one AI tool, that tool's name.
type Problem struct {
	Code string `json:"code"`
	Tool string `json:"tool,omitempty"`
}

const (
	ProblemHooksSilent  = "hooks_silent"  // the tool is in use but its hooks are silent
	ProblemHookFailed   = "hook_failed"   // SeaTime could not add its hooks to the tool
	ProblemSyncBehind   = "sync_behind"   // activity older than 20 minutes has not reached the server
	ProblemSyncFailing  = "sync_failing"  // the last sync failed
	ProblemNoService    = "no_service"    // the daemon will not start again after a restart
	ProblemAutoHookOff  = "auto_hook_off" // new AI tools are not hooked automatically
	ProblemNoGit        = "no_git"        // git is missing, so commits are not seen
	ProblemUpdateFailed = "update_failed" // the last automatic update failed
)

// DeviceSettings are settings as they stand on one computer.
type DeviceSettings struct {
	AutoUpdate    bool `json:"auto_update"`
	AlsoHackatime bool `json:"also_hackatime"`
	EditorPlugin  bool `json:"editor_plugin"` // the WakaTime/Hackatime plugin reports to SeaTime
}

type CheckInResp struct {
	// Settings are the account's settings, each with the time it was last
	// changed on the web. A device applies a setting when it changed after
	// the last value it applied, so a change made on the computer itself
	// stays until the setting changes on the web again.
	Settings []AccountSetting `json:"settings,omitempty"`
	// Requests are what the owner asked this computer to do on the web.
	Requests []DeviceRequest `json:"requests,omitempty"`
}

// AccountSetting is one setting the owner chose on the web.
type AccountSetting struct {
	Key     string    `json:"key"` // Setting*
	Value   bool      `json:"value"`
	Updated time.Time `json:"updated"`
}

const (
	SettingAlsoHackatime = "also_hackatime" // relay editor heartbeats to Hackatime unchanged
	SettingAutoUpdate    = "auto_update"    // install signed releases automatically
)

// DeviceRequest is one thing the owner asked a computer to do on the web.
type DeviceRequest struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"` // Request*
	// Project limits RequestForget to one project (a server key); "" is
	// every project.
	Project string `json:"project,omitempty"`
	// Purge makes RequestUninstall delete SeaTime's data on the computer too.
	Purge bool `json:"purge,omitempty"`
}

const (
	// RequestForget deletes the prompt text kept on the computer (as
	// `seatime forget`). Hashes and the hash chain stay.
	RequestForget = "forget"
	// RequestUninstall removes SeaTime from the computer (as `seatime
	// uninstall`); the server then disconnects the device.
	RequestUninstall = "uninstall"
	// RequestSettings is not a request: a device that lists it in Can
	// applies the account's settings.
	RequestSettings = "settings"
)

// POST /api/v1/device/requests/{id} {RequestResult} -> 200
type RequestResult struct {
	DeviceID string `json:"device_id"`
	OK       bool   `json:"ok"`
	// Error is a fixed code when OK is false: "unknown_project",
	// "failed" or "unsupported".
	Error string `json:"error,omitempty"`
	// Forgotten is how many records lost their text (RequestForget).
	Forgotten int `json:"forgotten,omitempty"`
}

// ---- today: what a computer's menu bar icon shows ----

// GET /api/v1/device/today -> Today
// The account's counted time today and what the server has not checked
// yet: the same numbers as the account's own pages on the website. A
// device asks only while its menu bar icon is running. An older server
// answers 404, and the icon then shows neither.
type Today struct {
	// Seconds is the time counted today, in the account's time zone.
	Seconds float64 `json:"seconds"`
	// UncheckedSeconds is counted time that the server has not checked yet,
	// and UncheckedPrompts the prompts sent in it.
	UncheckedSeconds float64 `json:"unchecked_seconds"`
	UncheckedPrompts int     `json:"unchecked_prompts"`
}

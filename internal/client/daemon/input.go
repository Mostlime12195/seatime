package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/idle"
)

// A prompt's hook lasts a moment, so it cannot know how long the keyboard
// and mouse were in use before the prompt. The daemon can: it reads the
// idle timer all along (idle.Recorder). The hook asks it here, on the
// local endpoint, and gets three numbers back.

// inputAnswer is GET /seatime/input?since=<unix ms>.
type inputAnswer struct {
	OK          bool  `json:"ok"` // false: no idle timer on this computer
	InputMS     int64 `json:"input_ms"`
	UnwatchedMS int64 `json:"unwatched_ms"`
	OverMS      int64 `json:"over_ms"`
}

func (d *Daemon) handleInput(w http.ResponseWriter, r *http.Request) {
	var a inputAnswer
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if d.Idle != nil {
		var from time.Time
		if since > 0 {
			from = time.UnixMilli(since)
		}
		if in, ok := d.Idle.InputSince(r.Context(), from); ok {
			a = inputAnswer{true, in.Input.Milliseconds(), in.Unwatched.Milliseconds(), in.Over.Milliseconds()}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

// InputSince asks the daemon on cfg's port how much keyboard or mouse
// input came since a time (zero: as far back as it counts). ok is false
// when no daemon answers in time or it has no idle timer.
func InputSince(cfg config.Config, since time.Time) (in idle.Input, ok bool) {
	c := http.Client{Timeout: 400 * time.Millisecond}
	var ms int64
	if !since.IsZero() {
		ms = since.UnixMilli()
	}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/seatime/input?since=%d", cfg.WakaPortOrDefault(), ms))
	if err != nil {
		return in, false
	}
	defer resp.Body.Close()
	var a inputAnswer
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&a) != nil || !a.OK {
		return in, false
	}
	ms2d := func(v int64) time.Duration { return time.Duration(max(v, 0)) * time.Millisecond }
	return idle.Input{Input: ms2d(a.InputMS), Unwatched: ms2d(a.UnwatchedMS), Over: ms2d(a.OverMS)}, true
}

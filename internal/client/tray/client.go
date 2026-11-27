package tray

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"seatime/internal/client/localauth"
)

// client talks to the daemon's local endpoint with the per-install secret.
type client struct{ port int }

func (c client) do(method, path string, timeout time.Duration, out any) bool {
	secret, err := localauth.Secret()
	if err != nil {
		return false
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", c.port, path), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(out) == nil
}

// status asks the daemon what to show (nil: it did not answer).
func (c client) status() *Status {
	var st Status
	if !c.do(http.MethodGet, "/seatime/tray", 5*time.Second, &st) {
		return nil
	}
	return &st
}

// act passes a click on (path "send" or "update") and waits for the answer.
func (c client) act(path string) (Reply, bool) {
	var r Reply
	ok := c.do(http.MethodPost, "/seatime/tray/"+path, 20*time.Minute, &r)
	return r, ok
}

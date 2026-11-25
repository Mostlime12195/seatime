// Package syncer talks to the SeaTime server: device login, periodic sync of
// score buckets + signed checkpoints, and segments.
package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/config"
	"seatime/pkg/api"
)

var ErrPending = errors.New("pending")

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Version string
	// Offset is the server clock minus ours, measured from the Date header of
	// the last response (1 s resolution); OffsetOK says whether it was set.
	Offset   time.Duration
	OffsetOK bool
	// CheckTimeout bounds a prompt check (0: 3 minutes); see CheckTimeout.
	CheckTimeout time.Duration
}

func NewClient(base, token, version string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), Token: token, Version: version,
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
	// Wait is the server's Retry-After, when it sent one in seconds.
	Wait time.Duration
}

// RetryAfter is how long the server asked to wait (0: it did not say).
func (e *HTTPError) RetryAfter() time.Duration { return e.Wait }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) do(ctx context.Context, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "seatime/"+c.Version+" ("+runtime.GOOS+"/"+runtime.GOARCH+")")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	sent := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		got := time.Now()
		mid := sent.Add(got.Sub(sent) / 2)
		// Date is truncated to the second: add half a second to centre it
		c.Offset, c.OffsetOK = d.Add(500*time.Millisecond).Sub(mid).Round(time.Second), true
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		he := &HTTPError{Status: resp.StatusCode, Body: string(b)}
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			he.Wait = time.Duration(n) * time.Second
		}
		return resp.StatusCode, he
	}
	if resp.StatusCode == http.StatusAccepted && path == "/api/v1/device/poll" {
		return resp.StatusCode, ErrPending
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("bad response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) DeviceStart(ctx context.Context, name, pub string) (api.DeviceStartResp, error) {
	var r api.DeviceStartResp
	_, err := c.do(ctx, "/api/v1/device/start", api.DeviceStartReq{DeviceName: name, OS: runtime.GOOS, PublicKey: pub}, &r)
	return r, err
}

// DeviceRelink starts a sign-in that reconnects an earlier device: the
// request is signed with that device's key, so its owner can approve it
// on the web and the device keeps its id.
func (c *Client) DeviceRelink(ctx context.Context, name string, auth config.Auth, now time.Time) (api.DeviceStartResp, error) {
	req := api.DeviceStartReq{DeviceName: name, OS: runtime.GOOS, PublicKey: auth.PublicKey}
	if auth.DeviceID != "" && auth.PrivateKey != "" {
		if sig, err := signText(auth, api.RelinkMessage(auth.DeviceID, auth.PublicKey, now.Unix())); err == nil {
			req.PrevDeviceID, req.PrevSignedAt, req.PrevSig = auth.DeviceID, now.Unix(), sig
		}
	}
	var r api.DeviceStartResp
	_, err := c.do(ctx, "/api/v1/device/start", req, &r)
	return r, err
}

func (c *Client) DevicePoll(ctx context.Context, code string) (api.DevicePollResp, error) {
	var r api.DevicePollResp
	_, err := c.do(ctx, "/api/v1/device/poll", api.DevicePollReq{DeviceCode: code}, &r)
	return r, err
}

func (c *Client) Sync(ctx context.Context, req api.SyncReq) (api.SyncResp, error) {
	var r api.SyncResp
	_, err := c.do(ctx, "/api/v1/sync", req, &r)
	return r, err
}

// PromptCheck sends one finished turn for the prompt check.
func (c *Client) PromptCheck(ctx context.Context, req api.PromptCheckReq) (api.PromptCheckResp, error) {
	var r api.PromptCheckResp
	cc := *c
	t := c.CheckTimeout // the server says how long the model may take
	if t <= 0 {
		t = 3 * time.Minute
	}
	cc.HTTP = &http.Client{Timeout: t}
	_, err := cc.do(ctx, "/api/v1/prompt-check", req, &r)
	return r, err
}

// PromptHashes sends prompt chain entries. On a 409 the answer still
// carries the server's head.
func (c *Client) PromptHashes(ctx context.Context, req api.PromptHashesReq) (api.PromptHashesResp, error) {
	var r api.PromptHashesResp
	_, err := c.do(ctx, "/api/v1/prompt-hashes", req, &r)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict {
		_ = json.Unmarshal([]byte(he.Body), &r)
	}
	return r, err
}

// Segment uploads one stretch of a project's records for the server to check.
func (c *Client) Segment(ctx context.Context, b api.Bundle) (api.SegmentResp, error) {
	var r api.SegmentResp
	cc := *c
	cc.HTTP = &http.Client{Timeout: 5 * time.Minute}
	_, err := cc.do(ctx, "/api/v1/segments", b, &r)
	return r, err
}

// CheckIn reports this computer's health and gets the account's
// settings and requests from the web.
func (c *Client) CheckIn(ctx context.Context, req api.CheckIn) (api.CheckInResp, error) {
	var r api.CheckInResp
	_, err := c.do(ctx, "/api/v1/device/check-in", req, &r)
	return r, err
}

// RequestResult answers one request from the web.
func (c *Client) RequestResult(ctx context.Context, id int64, res api.RequestResult) error {
	_, err := c.do(ctx, "/api/v1/device/requests/"+strconv.FormatInt(id, 10), res, nil)
	return err
}

// DeviceName is a human label for this machine.
func DeviceName() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "device"
	}
	return h
}

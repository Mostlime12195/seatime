package daemon

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/syncer"
)

// When the server stops accepting this computer (its token is unknown, or
// its owner disconnected it on the web), the daemon starts a new sign-in
// by itself instead of asking for `seatime login`. The request is signed
// with this computer's key, so the server shows it to the owner on the
// Settings page, and approving it reconnects the same device with its
// history. When the token is merely unknown, the daemon also opens the
// sign-in page in the browser, at most once a day. When the owner
// disconnected it on purpose, it waits quietly on the Settings page.

const (
	// MetaSignIn holds the sign-in in progress (JSON pendingSignIn).
	MetaSignIn = "signin_pending"
	// MetaSignInOpened is when the daemon last opened the sign-in page (unix s).
	MetaSignInOpened = "signin_opened"
	// MetaSignInNext is the earliest time to start another sign-in after
	// one expired unapproved (unix s).
	MetaSignInNext = "signin_next"
)

type pendingSignIn struct {
	DeviceCode string    `json:"device_code"`
	UserCode   string    `json:"user_code"`
	URL        string    `json:"url"`
	Expires    time.Time `json:"expires"`
	Interval   int       `json:"interval"`
	Server     string    `json:"server"`
	Revoked    bool      `json:"revoked"`
}

// authRejected reports whether a sync failed because the server no longer
// accepts this device's token, and whether it said the device was revoked.
func authRejected(err error) (rejected, revoked bool) {
	var he *syncer.HTTPError
	if !errors.As(err, &he) || he.Status != 401 {
		return false, false
	}
	return true, strings.Contains(he.Body, "revoked")
}

// signInStep moves the automatic sign-in one step: start one, or ask
// whether it was approved. It returns how long to wait before the next
// step, and done once this computer is signed in again.
func (d *Daemon) signInStep(ctx context.Context, revoked bool, now time.Time) (wait time.Duration, done bool) {
	auth, ok := config.LoadAuth()
	if !ok || auth.PrivateKey == "" {
		return time.Hour, false
	}
	server := strings.TrimRight(auth.Server, "/")
	if server == "" {
		server = config.Load().ServerURL()
	}
	var p pendingSignIn
	if err := d.Store.GetJSON(MetaSignIn, &p); err != nil || p.DeviceCode == "" || now.After(p.Expires) || p.Server != server {
		if p.DeviceCode != "" {
			_ = d.Store.PutJSON(MetaSignIn, nil)
			_ = d.Store.SetMeta(MetaSignInNext, strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
		}
		if next := metaTime(d.Store.GetMeta(MetaSignInNext)); now.Before(next) {
			return next.Sub(now), false
		}
		c := syncer.NewClient(server, "", d.Version)
		r, err := c.DeviceRelink(ctx, syncer.DeviceName(), auth, now)
		if err != nil {
			logx.Printf("sign-in: cannot start: %v", err)
			return 10 * time.Minute, false
		}
		p = pendingSignIn{DeviceCode: r.DeviceCode, UserCode: r.UserCode, URL: r.VerificationURL, Interval: r.Interval, Server: server, Revoked: revoked,
			Expires: now.Add(time.Duration(max(r.ExpiresIn, 60)) * time.Second)}
		_ = d.Store.PutJSON(MetaSignIn, p)
		msg := "This computer is signed out of SeaTime. Approve its sign-in (code " + p.UserCode + ") on the Settings page, or at " + p.URL + ". Your time is still recorded here."
		if revoked {
			msg = "This computer was disconnected on the web. To connect it again, approve it (code " + p.UserCode + ") on the Settings page. Your time is still recorded here."
		}
		_ = d.Store.SetMeta(MetaSyncBlocked, msg)
		opened := metaTime(d.Store.GetMeta(MetaSignInOpened))
		if !revoked && d.OpenBrowser != nil && now.Sub(opened) >= 24*time.Hour {
			_ = d.Store.SetMeta(MetaSignInOpened, strconv.FormatInt(now.Unix(), 10))
			if d.OpenBrowser(p.URL) {
				logx.Printf("sign-in: the server no longer accepts this computer; opened the sign-in page (code %s)", p.UserCode)
			} else {
				logx.Printf("sign-in: the server no longer accepts this computer; approve code %s on the Settings page", p.UserCode)
			}
		} else {
			logx.Printf("sign-in: waiting for approval of code %s on the Settings page", p.UserCode)
		}
	}
	every := time.Duration(max(p.Interval, 5)) * time.Second
	c := syncer.NewClient(p.Server, "", d.Version)
	r, err := c.DevicePoll(ctx, p.DeviceCode)
	if errors.Is(err, syncer.ErrPending) {
		return every, false
	}
	if err != nil {
		var he *syncer.HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 410) {
			_ = d.Store.PutJSON(MetaSignIn, nil) // used or expired: start again later
			_ = d.Store.SetMeta(MetaSignInNext, strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
			return time.Minute, false
		}
		return time.Minute, false
	}
	a := config.Auth{Token: r.Token, DeviceID: r.DeviceID, Username: r.Username, Server: p.Server, PrivateKey: auth.PrivateKey, PublicKey: auth.PublicKey}
	if err := config.SaveAuth(a); err != nil {
		logx.Printf("sign-in: saving: %v", err)
		return time.Minute, false
	}
	_ = d.Store.PutJSON(MetaSignIn, nil)
	_ = d.Store.SetMeta(MetaSyncBlocked, "")
	_ = d.Store.SetMeta(MetaSignInNext, "")
	if r.DeviceID == auth.DeviceID {
		logx.Printf("sign-in: approved; this computer is connected again as %s", r.Username)
	} else {
		logx.Printf("sign-in: approved; this computer is connected as a new device of %s", r.Username)
	}
	return 0, true
}

func metaTime(v string, _ error) time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

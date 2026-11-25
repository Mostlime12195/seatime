package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"seatime/internal/server/store"
)

// Hackatime's trust colour, passed through the Hackatime-shaped API.
//
// Many programs reject an account that Hackatime marks "red". An application that reads SeaTime instead of Hackatime would lose that
// protection if SeaTime answered "blue" for everyone. With
// SEATIME_HACKATIME_TRUST=1 the server asks Hackatime for the account's
// colour by its Slack ID (GET /api/v1/users/{slack_id}/trust_factor, a public
// endpoint) and serves Hackatime's answer. It is off by default because the
// lookup sends the Slack ID to Hackatime.
//
// When Hackatime cannot be asked, the API still answers "blue": it is
// Hackatime's own word for "not scored", and it is what a Hackatime-shaped
// client can parse. An unknown word could break a client that checks the
// colour against Hackatime's four, and it would not stop a client that only
// rejects "red" anyway. What protects against a missed red is that a colour
// Hackatime once served is kept and served for up to trustStale when Hackatime
// is down, and that every answer says where it came from (trust_source), so
// an application can tell "Hackatime says blue" from "not asked".

const (
	trustTTL     = 15 * time.Minute   // a stored answer is fresh this long
	trustStale   = 7 * 24 * time.Hour // a stored answer is served this long when Hackatime fails
	trustTimeout = 3 * time.Second
)

// Where a trust colour came from (TrustFact.Source).
const (
	TrustFromHackatime  = "hackatime"        // Hackatime answered within trustTTL
	TrustStale          = "stale"            // Hackatime failed; its last answer, younger than trustStale
	TrustNotOnHackatime = "not_on_hackatime" // Hackatime has no account with this Slack ID
	TrustUnavailable    = "unavailable"      // Hackatime failed and there is no usable earlier answer
	TrustNoSlackID      = "no_slack_id"      // the account has no Slack ID to ask with
	TrustOff            = "off"              // SEATIME_HACKATIME_TRUST is off
)

// TrustFact is an account's trust colour as the Hackatime-shaped API serves it.
type TrustFact struct {
	Level   string    // blue, red or green, as Hackatime names them; "blue" when not known
	Value   int       // Hackatime's trust_value for Level (blue 0, red 1, green 2)
	Source  string    // one of the Trust* constants above
	Checked time.Time // when Hackatime gave this answer (zero if it never did)
}

// hackatimeLevels are Hackatime's colours and their trust_value. Hackatime
// serves "yellow" as "blue" on its public endpoint; it is kept here so a
// change on their side does not turn into "unavailable".
var hackatimeLevels = map[string]int{"blue": 0, "red": 1, "green": 2, "yellow": 3}

var errTrustNotFound = errors.New("hackatime: user not found")

// trustFor is u's trust colour. It never fails: see the comment at the top.
func (s *Server) trustFor(ctx context.Context, u *store.User) TrustFact {
	if !s.cfg.HackatimeTrust {
		return TrustFact{Level: "blue", Source: TrustOff}
	}
	if u.SlackID == "" || !slackIDRe.MatchString(u.SlackID) {
		return TrustFact{Level: "blue", Source: TrustNoSlackID}
	}
	// one lookup per Slack ID at a time, and at most one a minute even when Hackatime fails
	v, err := s.cached("hackatime-trust:"+u.SlackID, time.Minute, func(ctx context.Context) (any, error) {
		return s.lookupTrust(ctx, u.SlackID), nil
	})
	if err != nil {
		return TrustFact{Level: "blue", Source: TrustUnavailable}
	}
	return v.(TrustFact)
}

func (s *Server) lookupTrust(ctx context.Context, slackID string) TrustFact {
	now := s.now()
	prev, _ := s.store.HackatimeTrust(ctx, slackID)
	if prev != nil && now.Sub(prev.Checked) < trustTTL {
		return storedTrust(prev, TrustFromHackatime)
	}
	level, value, err := s.fetchTrust(ctx, slackID)
	switch {
	case err == nil:
		t := store.HackatimeTrust{SlackID: slackID, Level: level, Value: value, Found: true, Checked: now}
		if err := s.store.PutHackatimeTrust(ctx, t); err != nil {
			s.log.Error("store hackatime trust", "err", err)
		}
		return storedTrust(&t, TrustFromHackatime)
	case errors.Is(err, errTrustNotFound):
		t := store.HackatimeTrust{SlackID: slackID, Level: "blue", Found: false, Checked: now}
		if err := s.store.PutHackatimeTrust(ctx, t); err != nil {
			s.log.Error("store hackatime trust", "err", err)
		}
		return storedTrust(&t, TrustFromHackatime)
	}
	s.log.Warn("hackatime trust lookup failed", "err", err)
	if prev != nil && now.Sub(prev.Checked) < trustStale {
		return storedTrust(prev, TrustStale)
	}
	return TrustFact{Level: "blue", Source: TrustUnavailable}
}

func storedTrust(t *store.HackatimeTrust, source string) TrustFact {
	if !t.Found {
		return TrustFact{Level: "blue", Source: TrustNotOnHackatime, Checked: t.Checked}
	}
	return TrustFact{Level: t.Level, Value: t.Value, Source: source, Checked: t.Checked}
}

// fetchTrust asks Hackatime. It returns errTrustNotFound on a 404 and an
// error on anything else that is not a known colour.
func (s *Server) fetchTrust(ctx context.Context, slackID string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, trustTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.cfg.HackatimeURL+"/api/v1/users/"+url.PathEscape(slackID)+"/trust_factor", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "seatimed")
	resp, err := s.trustHTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 404:
		return "", 0, errTrustNotFound
	default:
		return "", 0, errors.New("hackatime trust_factor: status " + resp.Status)
	}
	var out struct {
		Level string `json:"trust_level"`
		Value *int   `json:"trust_value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", 0, errors.New("hackatime trust_factor: bad json")
	}
	v, ok := hackatimeLevels[out.Level]
	if !ok {
		return "", 0, errors.New("hackatime trust_factor: unknown level")
	}
	if out.Value != nil {
		v = *out.Value
	}
	return out.Level, v, nil
}

// trustJSON is the trust_factor object of the Hackatime-shaped API, with
// SeaTime's trust_source beside Hackatime's two fields.
func trustJSON(t TrustFact) map[string]any {
	return map[string]any{"trust_level": t.Level, "trust_value": t.Value, "trust_source": t.Source}
}

package store

import (
	"context"
	"time"
)

// Hackatime's trust colour for a Slack ID, as the Hackatime-shaped API
// passes it through (trust.go in the server). The count never reads it.

// HackatimeTrust is Hackatime's last answer for a Slack ID.
type HackatimeTrust struct {
	SlackID string
	Level   string // as Hackatime served it: blue, red, green
	Value   int    // Hackatime's trust_value
	Found   bool   // false: Hackatime answered 404 (no such account)
	Checked time.Time
}

func (s *DB) HackatimeTrust(ctx context.Context, slackID string) (*HackatimeTrust, error) {
	t := HackatimeTrust{SlackID: slackID}
	var found int
	var c int64
	err := s.row(ctx, `SELECT level,value,found,checked FROM hackatime_trust WHERE slack_id=?`, slackID).Scan(&t.Level, &t.Value, &found, &c)
	if err != nil {
		return nil, nf(err)
	}
	t.Found, t.Checked = found == 1, ts(c)
	return &t, nil
}

func (s *DB) PutHackatimeTrust(ctx context.Context, t HackatimeTrust) error {
	_, err := s.exec(ctx, `INSERT INTO hackatime_trust(slack_id,level,value,found,checked) VALUES (?,?,?,?,?)
ON CONFLICT(slack_id) DO UPDATE SET level=excluded.level, value=excluded.value, found=excluded.found, checked=excluded.checked`,
		t.SlackID, t.Level, t.Value, b2i(t.Found), t.Checked.Unix())
	return err
}

package store

import (
	"context"
	"strings"
	"time"
)

// Checkpoint owners tell a chain that two accounts sent.

// DeviceOwner is a device that sent a given checkpoint hash.
type DeviceOwner struct {
	DeviceID string
	UserID   int64
}

// orNow is t, or the wall clock when t is zero (callers that don't pass the
// server clock).
func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// CheckpointOwners returns other devices that sent any of the given chain hashes.
func (s *DB) CheckpointOwners(ctx context.Context, hashes []string, excludeDevice string) ([]DeviceOwner, error) {
	seen := map[string]bool{}
	var out []DeviceOwner
	for len(hashes) > 0 {
		n := min(len(hashes), 400)
		chunk := hashes[:n]
		hashes = hashes[n:]
		args := []any{}
		for _, h := range chunk {
			args = append(args, h)
		}
		args = append(args, excludeDevice)
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT c.device_id, d.user_id FROM checkpoints c JOIN devices d ON d.id = c.device_id
WHERE c.hash IN (?`+strings.Repeat(",?", n-1)+`) AND c.device_id<>?`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var o DeviceOwner
			if err := rows.Scan(&o.DeviceID, &o.UserID); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[o.DeviceID] {
				seen[o.DeviceID] = true
				out = append(out, o)
			}
		}
		rows.Close()
	}
	return out, nil
}

// CheckpointBefore returns the device's newest checkpoint with Seq < seq.
func (s *DB) CheckpointBefore(ctx context.Context, dev string, seq int64) (*Checkpoint, error) {
	var c Checkpoint
	var t int64
	err := s.db.QueryRowContext(ctx, `SELECT seq,hash,server_ts FROM checkpoints WHERE device_id=? AND seq<? ORDER BY seq DESC LIMIT 1`, dev, seq).Scan(&c.Seq, &c.Hash, &t)
	c.ServerTS = ts(t)
	return &c, nf(err)
}

// UserMinuteHuman returns, per minute in [fromMin, toMin] and project key,
// the user's stored human seconds (devices merged by MAX, as readers do).
func (s *DB) UserMinuteHuman(ctx context.Context, userID int64, fromMin, toMin int64) (map[int64]map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT b.start_min, p.key, MAX(b.human) FROM buckets b JOIN projects p ON p.id = b.project_id
WHERE b.user_id=? AND b.start_min>=? AND b.start_min<=? GROUP BY b.start_min, p.id`, userID, fromMin, toMin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]float64{}
	for rows.Next() {
		var m int64
		var k string
		var h float64
		if err := rows.Scan(&m, &k, &h); err != nil {
			return nil, err
		}
		if out[m] == nil {
			out[m] = map[string]float64{}
		}
		out[m][k] = h
	}
	return out, rows.Err()
}

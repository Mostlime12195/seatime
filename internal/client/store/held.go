package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// Held is a prompt kept back until the AI tool's own log says who sent it
// (package emit, hold.go): the prompt as the adapter gave it, what was
// read at that moment for the prompt check, and the records of the same
// hook call that come after it. It is in no record and in no count until
// it is released; a prompt the tool made itself never becomes a record.
type Held struct {
	ID       int64
	Session  string
	PromptID string // the prompt's id in the tool's log
	Log      string // the log's path
	At       time.Time
	// Claimed is when a process began to release it (zero: nobody has).
	Claimed time.Time
	Prompt  event.Event
	After   []event.Event
	Snap    *promptcheck.Snapshot
	Info    *promptcheck.Project
}

type heldData struct {
	Prompt event.Event           `json:"prompt"`
	After  []event.Event         `json:"after,omitempty"`
	Snap   *promptcheck.Snapshot `json:"snap,omitempty"`
	Info   *promptcheck.Project  `json:"info,omitempty"`
}

// HeldClaim is how long a release may take before another process takes
// it over.
const HeldClaim = 15 * time.Second

// Hold keeps a prompt back. A prompt that is held already (the same one
// came twice: once from its hook and once from the hook's spool) is not
// held again.
func (s *Store) Hold(h Held) error {
	b, err := json.Marshal(heldData{Prompt: h.Prompt, After: h.After, Snap: h.Snap, Info: h.Info})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO held(session, prompt_id, log, at, data) SELECT ?,?,?,?,?
WHERE NOT EXISTS (SELECT 1 FROM held WHERE session=? AND prompt_id=?)`,
		h.Session, h.PromptID, h.Log, h.At.UnixMilli(), b, h.Session, h.PromptID)
	return err
}

// HeldRows are the prompts kept back, oldest first: a session's, or all
// ("").
func (s *Store) HeldRows(session string) ([]Held, error) {
	q, args := "SELECT id, session, prompt_id, log, at, claimed, data FROM held", []any{}
	if session != "" {
		q += " WHERE session=?"
		args = append(args, session)
	}
	rows, err := s.db.Query(q+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Held
	for rows.Next() {
		var h Held
		var at, claimed int64
		var b []byte
		if err := rows.Scan(&h.ID, &h.Session, &h.PromptID, &h.Log, &at, &claimed, &b); err != nil {
			return nil, err
		}
		var d heldData
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		h.At, h.Prompt, h.After, h.Snap, h.Info = time.UnixMilli(at).UTC(), d.Prompt, d.After, d.Snap, d.Info
		if claimed != 0 {
			h.Claimed = time.UnixMilli(claimed).UTC()
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HeldJoin puts a record behind the prompt a session holds back under
// promptID, so that it is recorded after it. false when no such prompt is
// held (any more).
func (s *Store) HeldJoin(session, promptID string, e event.Event) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	var b []byte
	err = tx.QueryRow("SELECT id, data FROM held WHERE session=? AND prompt_id=? AND claimed=0 ORDER BY id DESC LIMIT 1", session, promptID).Scan(&id, &b)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var d heldData
	if err := json.Unmarshal(b, &d); err != nil {
		return false, err
	}
	for _, a := range d.After {
		if a.Kind == e.Kind && a.TS.Equal(e.TS) {
			return true, nil // it is behind the prompt already
		}
	}
	d.After = append(d.After, e)
	if b, err = json.Marshal(d); err != nil {
		return false, err
	}
	if _, err := tx.Exec("UPDATE held SET data=? WHERE id=?", b, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ClaimHeld takes a held prompt for release: true for one process only,
// until HeldClaim has passed without it finishing (it died).
func (s *Store) ClaimHeld(id int64, now time.Time) (bool, error) {
	res, err := s.db.Exec("UPDATE held SET claimed=? WHERE id=? AND claimed<?", now.UnixMilli(), id, now.Add(-HeldClaim).UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DropHeld forgets a held prompt (it was released).
func (s *Store) DropHeld(id int64) error {
	_, err := s.db.Exec("DELETE FROM held WHERE id=?", id)
	return err
}

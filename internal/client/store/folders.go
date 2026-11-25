package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// A project is a folder. The folders table holds every folder this device
// has seen and the key of the project it belongs to, and that never
// changes: a folder that gets a git remote, or whose remote changes, goes
// on as the same project under the same key. Only its label follows (what
// the folder goes by now: its remote, else "local:<name>"), and the sync
// tells the server.
//
// A folder seen for the first time joins the project that goes by its
// label now: a second clone of a repo, or a folder that was moved. With no
// such project it starts one, under its label as the key. A label that is
// already the key of a project which has since gone on under another label
// is not that project's any more, so the new folder gets a key of its own.

// foldersFill fills folders for a store from before it (schema 9), when a
// folder got a new key each time its remote changed: each folder goes on
// under the newest of its keys, and an older key of the same folder keeps
// no label, so no new folder joins it.
const foldersFill = `
INSERT OR IGNORE INTO folders(root,key,label)
  SELECT root,key,key FROM projects WHERE root<>'' ORDER BY first_seen DESC, last_seen DESC;
UPDATE projects SET label=key WHERE label='' AND (root='' OR key IN (SELECT key FROM folders));
`

// Folder is one folder this device has seen.
type Folder struct {
	Root  string
	Key   string // its project
	Label string // what it goes by now
}

// FolderKey returns the key of the project the folder belongs to, given
// the label the folder goes by now, and keeps both on record.
func (s *Store) FolderKey(folder, label string) (string, error) {
	folder = filepath.Clean(folder)
	var key, was string
	switch err := s.db.QueryRow("SELECT key,label FROM folders WHERE root=?", folder).Scan(&key, &was); {
	case err == nil && was == label:
		return key, nil // the usual case: nothing to write
	case err != nil && err != sql.ErrNoRows:
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	switch err := tx.QueryRow("SELECT key,label FROM folders WHERE root=?", folder).Scan(&key, &was); {
	case err == nil && was == label:
		return key, nil
	case err == nil:
		// the folder's remote was added or changed: the same project
		if _, err := tx.Exec("UPDATE folders SET label=? WHERE root=?", label, folder); err != nil {
			return "", err
		}
		if _, err := tx.Exec("UPDATE projects SET label=? WHERE key=?", label, key); err != nil {
			return "", err
		}
	case err == sql.ErrNoRows:
		// a new folder: the project that goes by its label now, else its own
		switch err := tx.QueryRow("SELECT key FROM projects WHERE label=? ORDER BY last_seen DESC LIMIT 1", label).Scan(&key); {
		case err == sql.ErrNoRows:
			key = label
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM projects WHERE key=?", key).Scan(&n); err != nil {
				return "", err
			}
			if n > 0 {
				key = s.folderID(folder)
			}
		case err != nil:
			return "", err
		}
		if _, err := tx.Exec("INSERT INTO folders(root,key,label) VALUES(?,?,?)", folder, key, label); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	return key, tx.Commit()
}

// folderID is a key for a folder whose label is taken: "local:" and 12 hex
// characters, as a key that is not on a public forge goes by off this
// machine (event.HashedKey), from the folder's path under the device key.
func (s *Store) folderID(folder string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("seatime folder\x00"))
	m.Write([]byte(folder))
	return "local:" + hex.EncodeToString(m.Sum(nil)[:6])
}

// Folders lists every folder this device has seen.
func (s *Store) Folders() ([]Folder, error) {
	rows, err := s.db.Query("SELECT root,key,label FROM folders")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.Root, &f.Key, &f.Label); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FolderOf returns the known folder that holds dir (the deepest one).
func (s *Store) FolderOf(dir string) (Folder, bool) {
	fs, err := s.Folders()
	if err != nil {
		return Folder{}, false
	}
	var best Folder
	for _, f := range fs {
		if len(f.Root) <= len(best.Root) {
			continue
		}
		if rel, err := filepath.Rel(f.Root, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			best = f
		}
	}
	return best, best.Root != ""
}

// Label returns what a project's folder goes by now: the key itself for a
// project this device holds no other label for.
func (s *Store) Label(key string) string {
	var l string
	if err := s.db.QueryRow("SELECT label FROM projects WHERE key=?", key).Scan(&l); err != nil || l == "" {
		return key
	}
	return l
}

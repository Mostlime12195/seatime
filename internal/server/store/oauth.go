package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// OAuth 2.0 for applications that read hours (internal/server oauth.go). The
// secret, codes and tokens are stored as SHA-256 hashes; the server never
// keeps one it could show again.

type OAuthClient struct {
	ID           string
	UserID       int64 // the developer who registered it
	Name         string
	RedirectURIs []string
	SecretHash   string
	Created      time.Time
}

type OAuthGrant struct {
	UserID  int64
	Client  OAuthClient
	Scope   string
	Created time.Time
}

type OAuthCode struct {
	ClientID    string
	UserID      int64
	RedirectURI string
	Challenge   string
	Scope       string
	Expires     time.Time
	Used        bool
	Family      string
}

type OAuthToken struct {
	Kind     string // "access" or "refresh"
	ClientID string
	UserID   int64
	Scope    string
	Family   string
	Expires  time.Time
	Revoked  bool
}

type OAuth interface {
	CreateOAuthClient(ctx context.Context, c OAuthClient) error
	OAuthClient(ctx context.Context, id string) (*OAuthClient, error)
	UserOAuthClients(ctx context.Context, userID int64) ([]OAuthClient, error)
	UpdateOAuthClient(ctx context.Context, c OAuthClient) error
	// DeleteOAuthClient removes an application, its grants and its tokens.
	DeleteOAuthClient(ctx context.Context, userID int64, id string) error

	PutOAuthGrant(ctx context.Context, userID int64, clientID, scope string, at time.Time) error
	OAuthGrantFor(ctx context.Context, userID int64, clientID string) (*OAuthGrant, error)
	UserOAuthGrants(ctx context.Context, userID int64) ([]OAuthGrant, error)
	// RevokeOAuthGrant removes the approval and revokes every token of it.
	RevokeOAuthGrant(ctx context.Context, userID int64, clientID string) error

	PutOAuthCode(ctx context.Context, hash string, c OAuthCode) error
	// UseOAuthCode marks the code used and returns it as it was before:
	// Used true means it had been used already.
	UseOAuthCode(ctx context.Context, hash string) (*OAuthCode, error)

	PutOAuthToken(ctx context.Context, hash string, t OAuthToken, at time.Time) error
	OAuthToken(ctx context.Context, hash string) (*OAuthToken, error)
	// RevokeOAuthToken revokes one token; RevokeOAuthFamily every token
	// issued from one authorization.
	RevokeOAuthToken(ctx context.Context, hash string) error
	RevokeOAuthFamily(ctx context.Context, family string) error
}

const clientCols = `id,user_id,name,redirect_uris,secret_hash,created`

func scanClient(r scanner) (*OAuthClient, error) {
	var c OAuthClient
	var uris string
	var cr int64
	if err := r.Scan(&c.ID, &c.UserID, &c.Name, &uris, &c.SecretHash, &cr); err != nil {
		return nil, nf(err)
	}
	json.Unmarshal([]byte(uris), &c.RedirectURIs)
	c.Created = time.UnixMilli(cr).UTC()
	return &c, nil
}

func (s *DB) CreateOAuthClient(ctx context.Context, c OAuthClient) error {
	uris, _ := json.Marshal(c.RedirectURIs)
	_, err := s.exec(ctx, `INSERT INTO oauth_clients(`+clientCols+`) VALUES (?,?,?,?,?,?)`, c.ID, c.UserID, c.Name, string(uris), c.SecretHash, orNow(c.Created).UnixMilli())
	return err
}

func (s *DB) OAuthClient(ctx context.Context, id string) (*OAuthClient, error) {
	return scanClient(s.row(ctx, `SELECT `+clientCols+` FROM oauth_clients WHERE id=?`, id))
}

func (s *DB) UserOAuthClients(ctx context.Context, uid int64) ([]OAuthClient, error) {
	rows, err := s.rows(ctx, `SELECT `+clientCols+` FROM oauth_clients WHERE user_id=? ORDER BY created`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthClient
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *DB) UpdateOAuthClient(ctx context.Context, c OAuthClient) error {
	uris, _ := json.Marshal(c.RedirectURIs)
	res, err := s.exec(ctx, `UPDATE oauth_clients SET name=?, redirect_uris=?, secret_hash=? WHERE id=? AND user_id=?`, c.Name, string(uris), c.SecretHash, c.ID, c.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *DB) DeleteOAuthClient(ctx context.Context, uid int64, id string) error {
	return s.tx(ctx, func(tx *txn) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM oauth_clients WHERE id=? AND user_id=?`, id, uid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		for _, q := range []string{`DELETE FROM oauth_grants WHERE client_id=?`, `UPDATE oauth_tokens SET revoked=1 WHERE client_id=?`, `DELETE FROM oauth_codes WHERE client_id=?`} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DB) PutOAuthGrant(ctx context.Context, uid int64, clientID, scope string, at time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO oauth_grants(user_id,client_id,scope,created) VALUES (?,?,?,?)
  ON CONFLICT(user_id,client_id) DO UPDATE SET scope=excluded.scope`, uid, clientID, scope, at.UnixMilli())
	return err
}

const grantSel = `SELECT g.user_id,g.scope,g.created,c.id,c.user_id,c.name,c.redirect_uris,c.secret_hash,c.created FROM oauth_grants g JOIN oauth_clients c ON c.id=g.client_id `

func scanGrant(r scanner) (*OAuthGrant, error) {
	var g OAuthGrant
	var gc, cc int64
	var uris string
	if err := r.Scan(&g.UserID, &g.Scope, &gc, &g.Client.ID, &g.Client.UserID, &g.Client.Name, &uris, &g.Client.SecretHash, &cc); err != nil {
		return nil, nf(err)
	}
	json.Unmarshal([]byte(uris), &g.Client.RedirectURIs)
	g.Created, g.Client.Created = time.UnixMilli(gc).UTC(), time.UnixMilli(cc).UTC()
	return &g, nil
}

func (s *DB) OAuthGrantFor(ctx context.Context, uid int64, clientID string) (*OAuthGrant, error) {
	return scanGrant(s.row(ctx, grantSel+`WHERE g.user_id=? AND g.client_id=?`, uid, clientID))
}

func (s *DB) UserOAuthGrants(ctx context.Context, uid int64) ([]OAuthGrant, error) {
	rows, err := s.rows(ctx, grantSel+`WHERE g.user_id=? ORDER BY g.created`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthGrant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

func (s *DB) RevokeOAuthGrant(ctx context.Context, uid int64, clientID string) error {
	return s.tx(ctx, func(tx *txn) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id=? AND client_id=?`, uid, clientID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET revoked=1 WHERE user_id=? AND client_id=?`, uid, clientID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM oauth_codes WHERE user_id=? AND client_id=?`, uid, clientID)
		return err
	})
}

func (s *DB) PutOAuthCode(ctx context.Context, hash string, c OAuthCode) error {
	return s.tx(ctx, func(tx *txn) error {
		tx.ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires < ?`, time.Now().Add(-time.Hour).UnixMilli())
		_, err := tx.ExecContext(ctx, `INSERT INTO oauth_codes(code_hash,client_id,user_id,redirect_uri,challenge,scope,expires,used,family) VALUES (?,?,?,?,?,?,?,0,?)`,
			hash, c.ClientID, c.UserID, c.RedirectURI, c.Challenge, c.Scope, c.Expires.UnixMilli(), c.Family)
		return err
	})
}

func (s *DB) UseOAuthCode(ctx context.Context, hash string) (*OAuthCode, error) {
	var out *OAuthCode
	err := s.tx(ctx, func(tx *txn) error {
		var c OAuthCode
		var exp int64
		var used int
		err := tx.QueryRowContext(ctx, `SELECT client_id,user_id,redirect_uri,challenge,scope,expires,used,family FROM oauth_codes WHERE code_hash=?`, hash).
			Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.Challenge, &c.Scope, &exp, &used, &c.Family)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c.Expires, c.Used = time.UnixMilli(exp).UTC(), used != 0
		out = &c
		_, err = tx.ExecContext(ctx, `UPDATE oauth_codes SET used=1 WHERE code_hash=?`, hash)
		return err
	})
	return out, err
}

func (s *DB) PutOAuthToken(ctx context.Context, hash string, t OAuthToken, at time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO oauth_tokens(token_hash,kind,client_id,user_id,scope,family,expires,revoked,created) VALUES (?,?,?,?,?,?,?,0,?)`,
		hash, t.Kind, t.ClientID, t.UserID, t.Scope, t.Family, t.Expires.UnixMilli(), at.UnixMilli())
	return err
}

func (s *DB) OAuthToken(ctx context.Context, hash string) (*OAuthToken, error) {
	var t OAuthToken
	var exp int64
	var rev int
	err := s.row(ctx, `SELECT kind,client_id,user_id,scope,family,expires,revoked FROM oauth_tokens WHERE token_hash=?`, hash).
		Scan(&t.Kind, &t.ClientID, &t.UserID, &t.Scope, &t.Family, &exp, &rev)
	if err != nil {
		return nil, nf(err)
	}
	t.Expires, t.Revoked = time.UnixMilli(exp).UTC(), rev != 0
	return &t, nil
}

func (s *DB) RevokeOAuthToken(ctx context.Context, hash string) error {
	_, err := s.exec(ctx, `UPDATE oauth_tokens SET revoked=1 WHERE token_hash=?`, hash)
	return err
}

func (s *DB) RevokeOAuthFamily(ctx context.Context, family string) error {
	_, err := s.exec(ctx, `UPDATE oauth_tokens SET revoked=1 WHERE family=?`, family)
	return err
}

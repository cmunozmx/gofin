package jfdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gofin/internal/jfid"
)

const tokenColumns = `access_token, user_id, client, device, device_id, app_version, created, last_activity`

func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var (
		t                 Token
		created, activity sql.NullString
	)
	if err := row.Scan(
		&t.Value, &t.UserID, &t.Client, &t.Device, &t.DeviceID, &t.AppVersion,
		&created, &activity,
	); err != nil {
		return nil, err
	}
	t.Created = timeFromDB(created)
	t.LastActivity = timeFromDB(activity)
	return &t, nil
}

// newTokenValue returns a fresh access token: 16 crypto-random bytes, hex
// encoded. Same construction as the in-memory store it replaces, so tokens
// issued before and after the migration are indistinguishable to a client.
func newTokenValue() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// IssueToken creates and stores a token for a user's client.
//
// Upstream calls these Devices, which is why they live in that table: one row
// per app on per machine, carrying enough to render a session list.
func (s *Store) IssueToken(userID jfid.GUID, client, device, deviceID, appVersion string) (*Token, error) {
	value, err := newTokenValue()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	t := &Token{
		Value:        value,
		UserID:       userID,
		Client:       client,
		Device:       device,
		DeviceID:     deviceID,
		AppVersion:   appVersion,
		Created:      now,
		LastActivity: now,
	}

	if _, err := s.db.Exec(
		`INSERT INTO devices (`+tokenColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Value, t.UserID, t.Client, t.Device, t.DeviceID, t.AppVersion,
		timeToDB(t.Created), timeToDB(t.LastActivity),
	); err != nil {
		return nil, fmt.Errorf("issue token: %w", err)
	}
	return t, nil
}

// TokenByValue looks up a token, returning (nil, nil) when there is none.
//
// This runs on every authenticated request. An unknown token is the ordinary
// case for an expired client session, not an error.
func (s *Store) TokenByValue(value string) (*Token, error) {
	t, err := scanToken(s.db.QueryRow(
		`SELECT `+tokenColumns+` FROM devices WHERE access_token = ?`, value))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("token by value: %w", err)
	}
	return t, nil
}

// RevokeToken deletes a token. Revoking one that is already gone is a no-op:
// two clients logging out of the same session must not produce an error.
func (s *Store) RevokeToken(value string) error {
	if _, err := s.db.Exec(`DELETE FROM devices WHERE access_token = ?`, value); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// RevokeUserTokens deletes every token belonging to a user, logging out all of
// their devices at once.
func (s *Store) RevokeUserTokens(userID jfid.GUID) error {
	if _, err := s.db.Exec(`DELETE FROM devices WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("revoke user tokens: %w", err)
	}
	return nil
}

// TouchActivity records that a token was just used. Deliberately leaves
// created alone -- that is when the session began, not when it was last seen.
func (s *Store) TouchActivity(value string, at time.Time) error {
	if _, err := s.db.Exec(
		`UPDATE devices SET last_activity = ? WHERE access_token = ?`,
		timeToDB(at), value); err != nil {
		return fmt.Errorf("touch activity: %w", err)
	}
	return nil
}

// TokensForUser returns a user's tokens oldest first. An empty result is an
// empty, non-nil slice.
func (s *Store) TokensForUser(userID jfid.GUID) ([]*Token, error) {
	rows, err := s.db.Query(
		`SELECT `+tokenColumns+` FROM devices WHERE user_id = ? ORDER BY created`, userID)
	if err != nil {
		return nil, fmt.Errorf("tokens for user: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("tokens for user: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tokens for user: %w", err)
	}
	return out, nil
}

// Lookup resolves a token to both the token and its user in one query.
//
// This is the authentication hot path -- it runs on every request that carries
// a token -- so it is a join rather than two round trips. An unknown or revoked
// token returns (nil, nil, nil): not an error, just not authenticated.
func (s *Store) Lookup(value string) (*Token, *User, error) {
	var (
		t                 Token
		u                 User
		created, activity sql.NullString
		lastLogin         sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT d.access_token, d.user_id, d.client, d.device, d.device_id,
		        d.app_version, d.created, d.last_activity,
		        u.id, u.name, u.password_hash, u.admin, u.last_login
		 FROM devices d
		 JOIN users u ON u.id = d.user_id
		 WHERE d.access_token = ?`, value,
	).Scan(
		&t.Value, &t.UserID, &t.Client, &t.Device, &t.DeviceID,
		&t.AppVersion, &created, &activity,
		&u.ID, &u.Name, &u.PasswordHash, &u.Admin, &lastLogin,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lookup token: %w", err)
	}

	t.Created = timeFromDB(created)
	t.LastActivity = timeFromDB(activity)
	u.LastLogin = timeFromDB(lastLogin)
	return &t, &u, nil
}

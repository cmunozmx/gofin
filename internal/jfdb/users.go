package jfdb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gofin/internal/jfid"
)

// userColumns is the SELECT list every user read shares, so the Scan order in
// scanUser only has to be right once.
const userColumns = `id, name, password_hash, admin, last_login`

// scanUser reads one row of userColumns. rows is either *sql.Row or *sql.Rows.
func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var (
		u         User
		lastLogin sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Admin, &lastLogin); err != nil {
		return nil, err
	}
	u.LastLogin = timeFromDB(lastLogin)
	return &u, nil
}

// UpsertUser inserts a user or updates the one with the same ID.
//
// It deliberately does NOT write last_login. Config declares users and the
// database caches them, so this runs for every configured user on every boot;
// writing the column here would reset "last seen" to never on every restart.
func (s *Store) UpsertUser(u *User) error {
	if u == nil {
		return errors.New("upsert user: nil user")
	}
	if u.ID.IsZero() {
		return errors.New("upsert user: zero id")
	}

	_, err := s.db.Exec(
		`INSERT INTO users (id, name, name_lower, password_hash, admin)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     name          = excluded.name,
		     name_lower    = excluded.name_lower,
		     password_hash = excluded.password_hash,
		     admin         = excluded.admin`,
		u.ID, u.Name, strings.ToLower(u.Name), u.PasswordHash, u.Admin)
	if err != nil {
		return fmt.Errorf("upsert user %q: %w", u.Name, err)
	}
	return nil
}

// UserByID returns the user with this ID, or (nil, nil) if there is none.
//
// Absence is not an error: an unknown ID arrives whenever a client presents a
// token for a user that has since been removed from the config.
func (s *Store) UserByID(id jfid.GUID) (*User, error) {
	u, err := scanUser(s.db.QueryRow(
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("user by id: %w", err)
	}
	return u, nil
}

// UserByName returns the user with this name, case-insensitively, or
// (nil, nil) if there is none.
//
// Case-insensitive because that is how login behaves: a user who registered as
// "Admin" must still get in typing "admin".
func (s *Store) UserByName(name string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(
		`SELECT `+userColumns+` FROM users WHERE name_lower = ?`, strings.ToLower(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("user by name %q: %w", name, err)
	}
	return u, nil
}

// Users returns every user, ordered by name so the result is stable across
// calls. An empty database gives an empty, non-nil slice.
func (s *Store) Users() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY name_lower`)
	if err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("users: scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	return out, nil
}

// TouchLogin records that a user just authenticated.
//
// Replaces a direct mutation of the shared *User, which raced every reader and
// was lost on restart besides. Touching a user that no longer exists is a
// no-op, not an error -- the row can vanish between authenticating and writing.
func (s *Store) TouchLogin(id jfid.GUID, at time.Time) error {
	if _, err := s.db.Exec(
		`UPDATE users SET last_login = ? WHERE id = ?`, timeToDB(at), id); err != nil {
		return fmt.Errorf("touch login: %w", err)
	}
	return nil
}

package jfdb

import (
	"database/sql"
	"time"

	"gofin/internal/jfid"
)

// Store is the repository surface over a DB. Every method is safe for
// concurrent use: the pool is capped at one connection, so statements
// serialise.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store backed by db.
func NewStore(db *DB) *Store { return &Store{db: db.sql} }

// User is a gofin account.
//
// Field-for-field identical to the struct it replaces in package main, so
// main.go can alias it (`type User = jfdb.User`) and leave every handler and
// test untouched.
type User struct {
	ID           jfid.GUID
	Name         string
	PasswordHash string
	Admin        bool
	LastLogin    time.Time // zero value means never
}

// Token is one client's access token. Upstream calls this a Device.
type Token struct {
	Value        string // the access token itself; the primary key
	UserID       jfid.GUID
	Client       string
	Device       string
	DeviceID     string
	AppVersion   string
	Created      time.Time
	LastActivity time.Time
}

// UserData is one user's state for one item: watched, favourited, how far in.
//
// Rating, AudioStreamIndex and SubtitleStreamIndex are pointers because the
// column is nullable and "unset" is meaningfully different from zero -- audio
// stream 0 is a real stream, and a rating of 0 is a real rating.
type UserData struct {
	ItemID                jfid.GUID
	PlaybackPositionTicks int64
	PlayCount             int
	IsFavorite            bool
	Played                bool
	LastPlayed            time.Time // zero value means never
	Rating                *float64
	AudioStreamIndex      *int
	SubtitleStreamIndex   *int
}

// ---------------------------------------------------------------- time codec

// Timestamps are stored as RFC3339 in UTC: readable in the sqlite3 shell,
// sortable as text, and unambiguous about zone. A zero time.Time becomes SQL
// NULL rather than the year 1, matching how the wire format treats a
// LastLoginDate that has never been set.

// timeToDB renders t for storage. A zero t becomes NULL.
func timeToDB(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// timeFromDB parses a stored timestamp. NULL and unparseable values both come
// back as the zero time, which every caller already treats as "never".
func timeFromDB(s sql.NullString) time.Time {
	if !s.Valid || s.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// ---------------------------------------------------------------- guid codec

// guidToDB renders a GUID for storage: the dashless 32-hex form.
func guidToDB(g jfid.GUID) string { return g.String() }

// guidFromDB parses a stored GUID. An unparseable value comes back as the zero
// GUID; jfid.Parse accepts both the dashed and dashless spellings.
func guidFromDB(s string) jfid.GUID {
	g, err := jfid.Parse(s)
	if err != nil {
		return jfid.Nil
	}
	return g
}

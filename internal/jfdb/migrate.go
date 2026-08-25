package jfdb

import (
	"database/sql"
	"fmt"
)

// migrations are applied in order. Index 0 takes the schema to version 1, index
// 1 to version 2, and so on; the current version is held in SQLite's own
// PRAGMA user_version, so there is no bookkeeping table to keep consistent.
//
// Migrations are append-only. Never edit one that has shipped -- an existing
// database has already run it and will not run it again.
var migrations = []string{
	schemaV1,
}

// schemaV1 is the initial schema: users, their tokens, their per-item state and
// their display preferences.
//
// GUIDs are stored as TEXT in gofin's dashless form (jfid.GUID.String()). Since
// 10.10 upstream also stores them as TEXT rather than a byte-swapped BLOB
// (b.Property<Guid>("Id").HasColumnType("TEXT") in the EF model snapshot),
// which keeps a future import a pure read.
//
// WITHOUT ROWID on every table: each one is keyed by a value that is already
// unique and never auto-incremented, so the implicit rowid would be a second
// index nothing reads.
const schemaV1 = `
CREATE TABLE users (
  id            TEXT PRIMARY KEY,      -- dashless GUID, jfid.GUID.String()
  name          TEXT NOT NULL,
  name_lower    TEXT NOT NULL UNIQUE,  -- lookup by name is case-insensitive
  password_hash TEXT NOT NULL,
  admin         INTEGER NOT NULL DEFAULT 0,
  last_login    TEXT                   -- RFC3339 UTC; NULL means never
) WITHOUT ROWID;

CREATE TABLE devices (
  access_token  TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client        TEXT NOT NULL,
  device        TEXT NOT NULL,
  device_id     TEXT NOT NULL,
  app_version   TEXT NOT NULL DEFAULT '',
  created       TEXT NOT NULL,         -- RFC3339 UTC
  last_activity TEXT NOT NULL          -- RFC3339 UTC
) WITHOUT ROWID;

CREATE INDEX idx_devices_user ON devices(user_id);

-- item_id deliberately has NO foreign key: items are not in this database yet.
-- A row may outlive the item it describes, which is what lets watched state
-- survive a rescan that renamed a file. Upstream behaves the same way. The FK
-- arrives with the item table, if it ever does.
CREATE TABLE user_data (
  user_id                 TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id                 TEXT NOT NULL,
  playback_position_ticks INTEGER NOT NULL DEFAULT 0,
  play_count              INTEGER NOT NULL DEFAULT 0,
  is_favorite             INTEGER NOT NULL DEFAULT 0,
  played                  INTEGER NOT NULL DEFAULT 0,
  last_played_date        TEXT,        -- RFC3339 UTC; NULL means never
  rating                  REAL,
  audio_stream_index      INTEGER,
  subtitle_stream_index   INTEGER,
  PRIMARY KEY (user_id, item_id)
) WITHOUT ROWID;

-- data is the DisplayPreferencesDto serialised as JSON. This is the shape
-- upstream uses for BaseItems.Data (BaseItemMapper.cs): one JSON column that
-- round-trips the struct, plus flat columns for exactly what is filtered on.
-- Nothing filters on preference contents, so there are no flat columns here.
CREATE TABLE display_preferences (
  user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  prefs_id TEXT NOT NULL,
  client   TEXT NOT NULL,
  data     TEXT NOT NULL,
  PRIMARY KEY (user_id, prefs_id, client)
) WITHOUT ROWID;
`

// migrate brings the schema up to len(migrations). It is safe to call on every
// open: a database already at the current version does no work.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	if version > len(migrations) {
		return fmt.Errorf("database is at schema version %d, but this build only knows %d; "+
			"it was written by a newer gofin", version, len(migrations))
	}

	for v := version; v < len(migrations); v++ {
		if err := applyOne(db, v); err != nil {
			return err
		}
	}
	return nil
}

// applyOne runs migrations[i] and bumps user_version to i+1, atomically.
//
// PRAGMA user_version cannot be parameterised, hence the Sprintf; i is a slice
// index, not user input.
func applyOne(db *sql.DB, i int) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration %d: begin: %w", i+1, err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(migrations[i]); err != nil {
		return fmt.Errorf("migration %d: %w", i+1, err)
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return fmt.Errorf("migration %d: set user_version: %w", i+1, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migration %d: commit: %w", i+1, err)
	}
	return nil
}

// schemaVersion reports the schema version of an open database.
func schemaVersion(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

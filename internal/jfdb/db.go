// Package jfdb is the SQLite persistence layer: users, access tokens, per-user
// item data and display preferences.
//
// Nothing in here reaches the wire. Jellyfin's own storage shape
// ($JF/src/Jellyfin.Database/) is a reference, not a constraint -- what has to
// match upstream is the item-ID derivation in package jfid, not the schema.
package jfdb

import (
	"database/sql"
	"fmt"

	// modernc.org/sqlite is the pure-Go SQLite driver. It registers itself as
	// "sqlite" (not "sqlite3", and not its import path). It is chosen over
	// mattn/go-sqlite3 so the tree keeps building with CGO_ENABLED=0, which is
	// what makes `make dist` a single cross-compile and the binary static.
	_ "modernc.org/sqlite"
)

// DB owns the connection pool and the schema.
type DB struct {
	sql *sql.DB
}

// Open opens the database at path, applies the connection pragmas and brings
// the schema up to date. The file is created if it does not exist.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// WAL is what lets a scan write while clients read. Upstream configures
	// synchronous, temp_store, cache_size, locking_mode and journal_size_limit
	// but leaves journal_mode alone
	// ($JF/.../Providers.Sqlite/PragmaConnectionInterceptor.cs); that omission
	// is not worth copying.
	//
	// synchronous=NORMAL is the standard companion to WAL: a crash can lose the
	// last transaction, never the database. For playback positions that is the
	// right trade.
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := sqlDB.Exec(pragma); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	// One connection serialises every statement and removes SQLITE_BUSY
	// entirely. At this workload -- a few writes per playback session, reads
	// only on item listings -- that is the right default. Revisit with a split
	// read/write pool only if a listing shows up in a profile.
	sqlDB.SetMaxOpenConns(1)

	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}

	return &DB{sql: sqlDB}, nil
}

// Close releases the connection pool.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// SQL exposes the underlying handle. Repositories in this package use it; it is
// exported so tests can assert against the schema directly.
func (d *DB) SQL() *sql.DB { return d.sql }

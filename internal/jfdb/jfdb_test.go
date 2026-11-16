package jfdb

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"gofin/internal/jfid"
)

// openTest returns a Store over a fresh database in a temp directory, closed
// when the test ends.
//
// A file rather than :memory: on purpose -- it exercises the real WAL and
// locking behaviour, which is the part most likely to break.
func openTest(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "gofin.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db)
}

// seedUser inserts a user directly, without going through the users repository.
//
// Rows in devices, user_data and display_preferences all carry a foreign key
// onto users, so most tests need one to exist first. This writes the row with
// raw SQL on purpose: a failure in the users repository should not be able to
// fail every other repository's tests as collateral.
func seedUser(t *testing.T, s *Store, name string) jfid.GUID {
	t.Helper()
	id := jfid.HashString("user:"+name, jfid.UTF16LE)
	_, err := s.db.Exec(
		`INSERT INTO users (id, name, name_lower, password_hash, admin) VALUES (?, ?, ?, 'x', 0)`,
		id.String(), name, name)
	if err != nil {
		t.Fatalf("seedUser(%q): %v", name, err)
	}
	return id
}

func TestOpenAppliesSchema(t *testing.T) {
	s := openTest(t)

	v, err := schemaVersion(s.db)
	if err != nil {
		t.Fatalf("schemaVersion: %v", err)
	}
	if want := len(migrations); v != want {
		t.Errorf("user_version = %d, want %d", v, want)
	}

	for _, table := range []string{"users", "devices", "user_data", "display_preferences"} {
		var name string
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofin.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	id := seedUser(t, NewStore(first), "admin")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not re-run migrations over an existing schema, and must
	// still see the row. This is the property the whole package exists for.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var name string
	if err := second.SQL().QueryRow(
		`SELECT name FROM users WHERE id = ?`, id.String()).Scan(&name); err != nil {
		t.Fatalf("row did not survive close and reopen: %v", err)
	}
	if name != "admin" {
		t.Errorf("name = %q, want %q", name, "admin")
	}
}

func TestOpenRejectsANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofin.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Pretend a future gofin wrote this file.
	if _, err := db.SQL().Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	_ = db.Close()

	// Downgrading must refuse rather than silently run migration 1 again over a
	// schema it does not understand.
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a database from a newer schema version")
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s := openTest(t)

	// PRAGMA foreign_keys is off by default in SQLite, so this asserts Open
	// turned it on -- not anything about SQLite itself.
	_, err := s.db.Exec(
		`INSERT INTO devices (access_token, user_id, client, device, device_id, created, last_activity)
		 VALUES ('t', 'nosuchuser', 'c', 'd', 'did', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("insert with a dangling user_id succeeded; foreign keys are not enforced")
	}
}

func TestJournalModeIsWAL(t *testing.T) {
	s := openTest(t)

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestTimeCodec(t *testing.T) {
	// A zero time must store as NULL and read back as zero, so "never logged
	// in" never renders as the year 1.
	if got := timeToDB(time.Time{}); got != nil {
		t.Errorf("timeToDB(zero) = %v, want nil", got)
	}
	if got := timeFromDB(sql.NullString{}); !got.IsZero() {
		t.Errorf("timeFromDB(NULL) = %v, want zero", got)
	}

	want := time.Date(2026, 8, 25, 10, 45, 0, 0, time.UTC)
	stored, ok := timeToDB(want).(string)
	if !ok {
		t.Fatalf("timeToDB(%v) did not produce a string", want)
	}
	if got := timeFromDB(sql.NullString{String: stored, Valid: true}); !got.Equal(want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
}

func TestGUIDCodec(t *testing.T) {
	want := jfid.HashString("user:admin", jfid.UTF16LE)
	if got := guidFromDB(guidToDB(want)); got != want {
		t.Errorf("round trip = %v, want %v", got, want)
	}
	if got := guidFromDB("not a guid"); !got.IsZero() {
		t.Errorf("guidFromDB(garbage) = %v, want zero", got)
	}
}

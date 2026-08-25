package jfdb

import (
	"database/sql"
	"fmt"

	"gofin/internal/jfid"
)

// DisplayPrefs returns the stored display preferences for a user, prefs ID, and client.
// Returns (nil, nil) if no row exists; this is normal and callers should serve defaults.
func (s *Store) DisplayPrefs(userID jfid.GUID, prefsID, client string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(
		`SELECT data FROM display_preferences WHERE user_id = ? AND prefs_id = ? AND client = ?`,
		guidToDB(userID), prefsID, client).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query display_preferences: %w", err)
	}
	return data, nil
}

// SetDisplayPrefs stores or updates display preferences for a user.
// This is an UPSERT on the primary key (user_id, prefs_id, client).
// Empty or nil data returns an error to distinguish "no preferences" from stored bytes.
func (s *Store) SetDisplayPrefs(userID jfid.GUID, prefsID, client string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("data must not be empty")
	}
	_, err := s.db.Exec(
		`INSERT INTO display_preferences (user_id, prefs_id, client, data) VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, prefs_id, client) DO UPDATE SET data = excluded.data`,
		guidToDB(userID), prefsID, client, string(data))
	return err
}

// DeleteDisplayPrefs removes display preferences for a user.
// Returns nil if the row does not exist (rows absent are not an error).
func (s *Store) DeleteDisplayPrefs(userID jfid.GUID, prefsID, client string) error {
	_, err := s.db.Exec(
		`DELETE FROM display_preferences WHERE user_id = ? AND prefs_id = ? AND client = ?`,
		guidToDB(userID), prefsID, client)
	return err
}

// AllDisplayPrefs returns the count of preference rows for a user.
// This is used for tests and future dashboards; never negative.
func (s *Store) AllDisplayPrefs(userID jfid.GUID) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM display_preferences WHERE user_id = ?`, guidToDB(userID)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count display_preferences: %w", err)
	}
	return count, nil
}

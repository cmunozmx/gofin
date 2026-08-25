package jfdb

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gofin/internal/jfid"
)

// userDataColumns omits user_id: every read here is already scoped to one user.
const userDataColumns = `item_id, playback_position_ticks, play_count, is_favorite, played,
	last_played_date, rating, audio_stream_index, subtitle_stream_index`

func scanUserData(row interface{ Scan(...any) error }) (UserData, error) {
	var (
		d          UserData
		lastPlayed sql.NullString
		rating     sql.NullFloat64
		audio, sub sql.NullInt64
	)
	if err := row.Scan(
		&d.ItemID, &d.PlaybackPositionTicks, &d.PlayCount, &d.IsFavorite, &d.Played,
		&lastPlayed, &rating, &audio, &sub,
	); err != nil {
		return UserData{}, err
	}

	d.LastPlayed = timeFromDB(lastPlayed)
	// Nullable columns stay nil rather than becoming zero: audio stream 0 is a
	// real stream and a rating of 0 is a real rating, so "unset" has to be
	// distinguishable from either.
	if rating.Valid {
		v := rating.Float64
		d.Rating = &v
	}
	if audio.Valid {
		v := int(audio.Int64)
		d.AudioStreamIndex = &v
	}
	if sub.Valid {
		v := int(sub.Int64)
		d.SubtitleStreamIndex = &v
	}
	return d, nil
}

// UserDataFor returns everything a user has state for, keyed by item.
//
// One query, called once per request, because the alternative is a lookup per
// row of an item listing. Only items the user has actually touched have a row,
// so this stays small even for a large library.
func (s *Store) UserDataFor(userID jfid.GUID) (map[jfid.GUID]UserData, error) {
	rows, err := s.db.Query(
		`SELECT `+userDataColumns+` FROM user_data WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("user data: %w", err)
	}
	defer rows.Close()

	out := make(map[jfid.GUID]UserData)
	for rows.Next() {
		d, err := scanUserData(rows)
		if err != nil {
			return nil, fmt.Errorf("user data: scan: %w", err)
		}
		out[d.ItemID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user data: %w", err)
	}
	return out, nil
}

// UserDataItem returns one item's state. An item with no row yet comes back as
// the zero value with ItemID set -- nothing watched, nothing favourited --
// which is the truth, not an error.
func (s *Store) UserDataItem(userID, itemID jfid.GUID) (UserData, error) {
	d, err := scanUserData(s.db.QueryRow(
		`SELECT `+userDataColumns+` FROM user_data WHERE user_id = ? AND item_id = ?`,
		userID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return UserData{ItemID: itemID}, nil
	}
	if err != nil {
		return UserData{}, fmt.Errorf("user data item: %w", err)
	}
	return d, nil
}

// Every Set* method below is an upsert that writes ONLY its own columns. On
// insert the rest take their schema defaults; on conflict the rest are left
// alone. Favouriting something must not forget how far in the user was, and
// reporting progress must not clear the favourite flag.

// SetFavorite marks or unmarks an item as a favourite.
func (s *Store) SetFavorite(userID, itemID jfid.GUID, favorite bool) error {
	if _, err := s.db.Exec(
		`INSERT INTO user_data (user_id, item_id, is_favorite) VALUES (?, ?, ?)
		 ON CONFLICT(user_id, item_id) DO UPDATE SET is_favorite = excluded.is_favorite`,
		userID, itemID, favorite); err != nil {
		return fmt.Errorf("set favorite: %w", err)
	}
	return nil
}

// SetPlayed marks an item watched or unwatched.
//
// Marking it watched also counts the play and stamps the date. Un-marking it
// does neither: the user is correcting the flag, not un-watching history, so
// play_count stands.
func (s *Store) SetPlayed(userID, itemID jfid.GUID, played bool, at time.Time) error {
	var err error
	if played {
		_, err = s.db.Exec(
			`INSERT INTO user_data (user_id, item_id, played, play_count, last_played_date)
			 VALUES (?, ?, 1, 1, ?)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET
			     played           = 1,
			     play_count       = user_data.play_count + 1,
			     last_played_date = excluded.last_played_date`,
			userID, itemID, timeToDB(at))
	} else {
		_, err = s.db.Exec(
			`INSERT INTO user_data (user_id, item_id, played) VALUES (?, ?, 0)
			 ON CONFLICT(user_id, item_id) DO UPDATE SET played = 0`,
			userID, itemID)
	}
	if err != nil {
		return fmt.Errorf("set played: %w", err)
	}
	return nil
}

// SetProgress records how far into an item the user has got. This is what
// /Sessions/Playing/Progress feeds, and what makes "Continue Watching" work.
func (s *Store) SetProgress(userID, itemID jfid.GUID, ticks int64, at time.Time) error {
	if _, err := s.db.Exec(
		`INSERT INTO user_data (user_id, item_id, playback_position_ticks, last_played_date)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, item_id) DO UPDATE SET
		     playback_position_ticks = excluded.playback_position_ticks,
		     last_played_date        = excluded.last_played_date`,
		userID, itemID, ticks, timeToDB(at)); err != nil {
		return fmt.Errorf("set progress: %w", err)
	}
	return nil
}

// ResumeItems answers "Continue Watching": started but not finished, most
// recent first. A limit of zero or less means no limit.
//
// The caller resolves each ItemID against the item store and drops the ones
// that no longer exist -- expected after a rescan renames a file, not an error.
func (s *Store) ResumeItems(userID jfid.GUID, limit int) ([]UserData, error) {
	query := `SELECT ` + userDataColumns + ` FROM user_data
	          WHERE user_id = ? AND playback_position_ticks > 0 AND played = 0
	          ORDER BY last_played_date DESC`
	args := []any{userID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("resume items: %w", err)
	}
	defer rows.Close()

	out := []UserData{}
	for rows.Next() {
		d, err := scanUserData(rows)
		if err != nil {
			return nil, fmt.Errorf("resume items: scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resume items: %w", err)
	}
	return out, nil
}

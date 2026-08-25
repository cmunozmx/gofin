package jfdb

import (
	"testing"
	"time"

	"gofin/internal/jfid"
)

func itemID(name string) jfid.GUID { return jfid.HashString("item:"+name, jfid.UTF16LE) }

func TestSetProgressRoundTrip(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	item := itemID("blade-runner")
	when := time.Date(2026, 8, 25, 21, 0, 0, 0, time.UTC)

	if err := s.SetProgress(user, item, 600_000_000, when); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}

	got, err := s.UserDataItem(user, item)
	if err != nil {
		t.Fatalf("UserDataItem: %v", err)
	}
	if got.PlaybackPositionTicks != 600_000_000 {
		t.Errorf("ticks = %d, want 600000000", got.PlaybackPositionTicks)
	}
	if !got.LastPlayed.Equal(when) {
		t.Errorf("LastPlayed = %v, want %v", got.LastPlayed, when)
	}
	if got.ItemID != item {
		t.Errorf("ItemID = %v, want %v", got.ItemID, item)
	}
}

// The two tests that matter most: an upsert must write only its own columns.

func TestSetFavoriteDoesNotClobberProgress(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	item := itemID("blade-runner")

	if err := s.SetProgress(user, item, 600, time.Now().UTC()); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	if err := s.SetFavorite(user, item, true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}

	got, err := s.UserDataItem(user, item)
	if err != nil {
		t.Fatalf("UserDataItem: %v", err)
	}
	if got.PlaybackPositionTicks != 600 {
		t.Errorf("favouriting erased progress: ticks = %d, want 600", got.PlaybackPositionTicks)
	}
	if !got.IsFavorite {
		t.Error("IsFavorite = false, want true")
	}
}

func TestSetProgressDoesNotClobberFavorite(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	item := itemID("blade-runner")

	if err := s.SetFavorite(user, item, true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	if err := s.SetProgress(user, item, 900, time.Now().UTC()); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}

	got, err := s.UserDataItem(user, item)
	if err != nil {
		t.Fatalf("UserDataItem: %v", err)
	}
	if !got.IsFavorite {
		t.Error("reporting progress cleared the favourite flag")
	}
	if got.PlaybackPositionTicks != 900 {
		t.Errorf("ticks = %d, want 900", got.PlaybackPositionTicks)
	}
}

func TestSetPlayedCountsPlays(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	item := itemID("arrival")
	now := time.Now().UTC()

	for i := 1; i <= 2; i++ {
		if err := s.SetPlayed(user, item, true, now); err != nil {
			t.Fatalf("SetPlayed %d: %v", i, err)
		}
		got, err := s.UserDataItem(user, item)
		if err != nil {
			t.Fatalf("UserDataItem: %v", err)
		}
		if got.PlayCount != i {
			t.Errorf("after %d plays, PlayCount = %d, want %d", i, got.PlayCount, i)
		}
		if !got.Played {
			t.Error("Played = false, want true")
		}
	}

	// Un-marking corrects the flag; it does not un-watch history.
	if err := s.SetPlayed(user, item, false, now); err != nil {
		t.Fatalf("SetPlayed(false): %v", err)
	}
	got, err := s.UserDataItem(user, item)
	if err != nil {
		t.Fatalf("UserDataItem: %v", err)
	}
	if got.Played {
		t.Error("Played = true after SetPlayed(false)")
	}
	if got.PlayCount != 2 {
		t.Errorf("PlayCount = %d, want 2 -- un-marking must not reset the count", got.PlayCount)
	}
}

func TestUserDataItemMissingIsZeroNotError(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	item := itemID("never-touched")

	got, err := s.UserDataItem(user, item)
	if err != nil {
		t.Fatalf("UserDataItem on an untouched item returned an error: %v", err)
	}
	if got.ItemID != item {
		t.Errorf("ItemID = %v, want %v", got.ItemID, item)
	}
	if got.PlaybackPositionTicks != 0 || got.PlayCount != 0 || got.IsFavorite || got.Played {
		t.Errorf("got %+v, want the zero value", got)
	}
	if !got.LastPlayed.IsZero() {
		t.Errorf("LastPlayed = %v, want zero", got.LastPlayed)
	}
}

func TestNullableFieldsStayNil(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	item := itemID("dune")

	if err := s.SetProgress(user, item, 10, time.Now().UTC()); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	got, err := s.UserDataItem(user, item)
	if err != nil {
		t.Fatalf("UserDataItem: %v", err)
	}
	if got.Rating != nil {
		t.Errorf("Rating = %v, want nil when never set", *got.Rating)
	}
	if got.AudioStreamIndex != nil {
		t.Errorf("AudioStreamIndex = %v, want nil -- stream 0 is a real stream", *got.AudioStreamIndex)
	}
	if got.SubtitleStreamIndex != nil {
		t.Errorf("SubtitleStreamIndex = %v, want nil", *got.SubtitleStreamIndex)
	}
}

func TestUserDataForIsOneMapPerUser(t *testing.T) {
	s := openTest(t)
	alice := seedUser(t, s, "alice")
	bob := seedUser(t, s, "bob")

	empty, err := s.UserDataFor(alice)
	if err != nil {
		t.Fatalf("UserDataFor: %v", err)
	}
	if empty == nil {
		t.Error("UserDataFor returned nil, want an empty map")
	}
	if len(empty) != 0 {
		t.Errorf("got %d rows, want 0", len(empty))
	}

	now := time.Now().UTC()
	for _, name := range []string{"a", "b", "c"} {
		if err := s.SetProgress(alice, itemID(name), 5, now); err != nil {
			t.Fatalf("SetProgress: %v", err)
		}
	}
	if err := s.SetFavorite(bob, itemID("a"), true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}

	got, err := s.UserDataFor(alice)
	if err != nil {
		t.Fatalf("UserDataFor: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows for alice, want 3", len(got))
	}
	for _, name := range []string{"a", "b", "c"} {
		d, ok := got[itemID(name)]
		if !ok {
			t.Errorf("item %q missing from the map", name)
			continue
		}
		if d.PlaybackPositionTicks != 5 {
			t.Errorf("item %q ticks = %d, want 5", name, d.PlaybackPositionTicks)
		}
	}
	// Bob's row must not leak into alice's map.
	if d := got[itemID("a")]; d.IsFavorite {
		t.Error("bob's favourite appeared in alice's user data")
	}
}

func TestResumeItems(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// Started, not finished -- these should come back, newest first.
	if err := s.SetProgress(user, itemID("older"), 100, base); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	if err := s.SetProgress(user, itemID("newer"), 200, base.Add(time.Hour)); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	// Started but finished -- excluded.
	if err := s.SetProgress(user, itemID("finished"), 300, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	if err := s.SetPlayed(user, itemID("finished"), true, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("SetPlayed: %v", err)
	}
	// Favourited but never started -- excluded, position is still 0.
	if err := s.SetFavorite(user, itemID("unstarted"), true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}

	got, err := s.ResumeItems(user, 0)
	if err != nil {
		t.Fatalf("ResumeItems: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d resume items, want 2 (%+v)", len(got), got)
	}
	if got[0].ItemID != itemID("newer") {
		t.Error("resume list is not most-recent-first")
	}
	if got[1].ItemID != itemID("older") {
		t.Errorf("second item = %v, want %v", got[1].ItemID, itemID("older"))
	}

	limited, err := s.ResumeItems(user, 1)
	if err != nil {
		t.Fatalf("ResumeItems(limit 1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("got %d items with limit 1, want 1", len(limited))
	}
}

func TestResumeItemsEmptyIsNonNil(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	got, err := s.ResumeItems(user, 10)
	if err != nil {
		t.Fatalf("ResumeItems: %v", err)
	}
	if got == nil {
		t.Error("ResumeItems returned nil, want an empty slice")
	}
}

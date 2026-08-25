package jfdb

import (
	"testing"
)

func TestDisplayPrefsRoundTrip(t *testing.T) {
	s := openTest(t)
	userID := seedUser(t, s, "alice")

	data := []byte(`{"sortOrder":"NameDescending","viewMode":"CoverArt","imageSize":"Large","nested":{"level":1,"value":"test"}}`)
	if err := s.SetDisplayPrefs(userID, "library1", "web", data); err != nil {
		t.Fatalf("SetDisplayPrefs: %v", err)
	}

	got, err := s.DisplayPrefs(userID, "library1", "web")
	if err != nil {
		t.Fatalf("DisplayPrefs error = %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("round-trip data mismatch:\ngot  %q\nwant %q", got, data)
	}

	// Also test with unicode character
	dataUnicode := []byte(`{"sortOrder":"NameDescending","item":"café"}`)
	if err := s.SetDisplayPrefs(userID, "library2", "web", dataUnicode); err != nil {
		t.Fatalf("SetDisplayPrefs: %v", err)
	}

	gotUnicode, err := s.DisplayPrefs(userID, "library2", "web")
	if err != nil {
		t.Fatalf("DisplayPrefs error = %v", err)
	}
	if string(gotUnicode) != string(dataUnicode) {
		t.Errorf("unicode round-trip mismatch:\ngot  %q\nwant %q", gotUnicode, dataUnicode)
	}
}

func TestDisplayPrefsMissingRow(t *testing.T) {
	s := openTest(t)
	userID := seedUser(t, s, "alice")

	got, err := s.DisplayPrefs(userID, "nonexistent", "web")
	if err != nil {
		t.Fatalf("DisplayPrefs error = %v", err)
	}
	if got != nil {
		t.Errorf("got data = %q, want nil", string(got))
	}
}

func TestDisplayPrefsSeparateClients(t *testing.T) {
	s := openTest(t)
	userID := seedUser(t, s, "alice")

	data1 := []byte(`{"client":"web"}`)
	if err := s.SetDisplayPrefs(userID, "library1", "web", data1); err != nil {
		t.Fatalf("SetDisplayPrefs web: %v", err)
	}

	data2 := []byte(`{"client":"mobile"}`)
	if err := s.SetDisplayPrefs(userID, "library1", "mobile", data2); err != nil {
		t.Fatalf("SetDisplayPrefs mobile: %v", err)
	}

	gotWeb, _ := s.DisplayPrefs(userID, "library1", "web")
	gotMobile, _ := s.DisplayPrefs(userID, "library1", "mobile")

	if string(gotWeb) != string(data1) {
		t.Errorf("web data mismatch:\ngot  %q\nwant %q", gotWeb, data1)
	}
	if string(gotMobile) != string(data2) {
		t.Errorf("mobile data mismatch:\ngot  %q\nwant %q", gotMobile, data2)
	}

	// Verify writing one does not affect the other
	if err := s.SetDisplayPrefs(userID, "library1", "web", []byte(`{"client":"web","updated":true}`)); err != nil {
		t.Fatalf("SetDisplayPrefs web update: %v", err)
	}

	gotWeb2, _ := s.DisplayPrefs(userID, "library1", "web")
	gotMobile2, _ := s.DisplayPrefs(userID, "library1", "mobile")

	if string(gotWeb2) != `{"client":"web","updated":true}` {
		t.Errorf("web update mismatch:\ngot  %q\nwant `{\"client\":\"web\",\"updated\":true}`", gotWeb2)
	}
	if string(gotMobile2) != string(data2) {
		t.Errorf("mobile still has original data, which is correct: %q", string(gotMobile2))
	}

	// Verify AllDisplayPrefs counts both
	count, err := s.AllDisplayPrefs(userID)
	if err != nil {
		t.Fatalf("AllDisplayPrefs error = %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestSetTwiceOverwrites(t *testing.T) {
	s := openTest(t)
	userID := seedUser(t, s, "alice")

	data1 := []byte(`{"first":true}`)
	if err := s.SetDisplayPrefs(userID, "library1", "web", data1); err != nil {
		t.Fatalf("First SetDisplayPrefs: %v", err)
	}

	countBefore, _ := s.AllDisplayPrefs(userID)
	if countBefore != 1 {
		t.Errorf("count after first set = %d, want 1", countBefore)
	}

	data2 := []byte(`{"second":true}`)
	if err := s.SetDisplayPrefs(userID, "library1", "web", data2); err != nil {
		t.Fatalf("Second SetDisplayPrefs: %v", err)
	}

	got, _ := s.DisplayPrefs(userID, "library1", "web")
	if string(got) != string(data2) {
		t.Errorf("after overwrite:\ngot  %q\nwant %q", got, data2)
	}

	countAfter, _ := s.AllDisplayPrefs(userID)
	if countAfter != 1 {
		t.Errorf("count after second set = %d, want 1", countAfter)
	}
}

func TestSetWithEmptyData(t *testing.T) {
	s := openTest(t)
	userID := seedUser(t, s, "alice")

	tests := []struct {
		name string
		data []byte
	}{
		{"nil data", nil},
		{"empty slice", []byte("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.SetDisplayPrefs(userID, "library1", "web", tt.data)
			if err == nil {
				t.Error("expected error for empty data")
			} else if got, want := err.Error(), "data must not be empty"; got != want {
				t.Errorf("error = %q, want %q", got, want)
			}
		})
	}
}

func TestDeleteDisplayPrefs(t *testing.T) {
	s := openTest(t)
	userID := seedUser(t, s, "alice")

	data := []byte(`{"toDelete":true}`)
	if err := s.SetDisplayPrefs(userID, "library1", "web", data); err != nil {
		t.Fatalf("SetDisplayPrefs: %v", err)
	}

	countBefore, _ := s.AllDisplayPrefs(userID)
	if countBefore != 1 {
		t.Errorf("count before delete = %d, want 1", countBefore)
	}

	if err := s.DeleteDisplayPrefs(userID, "library1", "web"); err != nil {
		t.Fatalf("DeleteDisplayPrefs: %v", err)
	}

	countAfter, _ := s.AllDisplayPrefs(userID)
	if countAfter != 0 {
		t.Errorf("count after delete = %d, want 0", countAfter)
	}

	got, err := s.DisplayPrefs(userID, "library1", "web")
	if err != nil {
		t.Fatalf("DisplayPrefs error = %v", err)
	}
	if got != nil {
		t.Errorf("got data after delete = %q, want nil", string(got))
	}

	// Delete again should return nil (rows absent are not an error)
	err = s.DeleteDisplayPrefs(userID, "library1", "web")
	if err != nil {
		t.Errorf("delete non-existent row returned error: %v", err)
	}
}

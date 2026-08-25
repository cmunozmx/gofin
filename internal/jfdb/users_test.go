package jfdb

import (
	"strings"
	"testing"
	"time"

	"gofin/internal/jfid"
)

func adminUser() *User {
	return &User{
		ID:           jfid.HashString("user:admin", jfid.UTF16LE),
		Name:         "admin",
		PasswordHash: "$PBKDF2-SHA512$iterations=210000$00$aa",
		Admin:        true,
	}
}

func TestUpsertUserRoundTrip(t *testing.T) {
	s := openTest(t)
	want := adminUser()

	if err := s.UpsertUser(want); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	got, err := s.UserByID(want.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got == nil {
		t.Fatal("UserByID returned nil for a user that was just inserted")
	}
	if got.ID != want.ID || got.Name != want.Name ||
		got.PasswordHash != want.PasswordHash || got.Admin != want.Admin {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !got.LastLogin.IsZero() {
		t.Errorf("LastLogin = %v, want zero for a user who has never logged in", got.LastLogin)
	}
}

func TestUpsertUserUpdatesInPlace(t *testing.T) {
	s := openTest(t)
	u := adminUser()
	if err := s.UpsertUser(u); err != nil {
		t.Fatalf("first UpsertUser: %v", err)
	}

	// Same ID, changed password and admin flag: the config file was edited.
	u.PasswordHash = "$PBKDF2-SHA512$iterations=210000$11$bb"
	u.Admin = false
	if err := s.UpsertUser(u); err != nil {
		t.Fatalf("second UpsertUser: %v", err)
	}

	all, err := s.Users()
	if err != nil {
		t.Fatalf("Users: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d users, want 1 -- upsert inserted instead of updating", len(all))
	}
	if all[0].PasswordHash != u.PasswordHash || all[0].Admin {
		t.Errorf("got %+v, want the updated hash and admin=false", all[0])
	}
}

// The one that matters: users are re-seeded from config on every boot, so an
// upsert must not erase when they last logged in.
func TestUpsertUserPreservesLastLogin(t *testing.T) {
	s := openTest(t)
	u := adminUser()
	if err := s.UpsertUser(u); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	when := time.Date(2026, 8, 25, 10, 45, 0, 0, time.UTC)
	if err := s.TouchLogin(u.ID, when); err != nil {
		t.Fatalf("TouchLogin: %v", err)
	}

	got, err := s.UserByID(u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !got.LastLogin.Equal(when) {
		t.Fatalf("after TouchLogin, LastLogin = %v, want %v", got.LastLogin, when)
	}

	// Now re-seed, as a restart would.
	if err := s.UpsertUser(u); err != nil {
		t.Fatalf("re-seed UpsertUser: %v", err)
	}
	got, err = s.UserByID(u.ID)
	if err != nil {
		t.Fatalf("UserByID after re-seed: %v", err)
	}
	if !got.LastLogin.Equal(when) {
		t.Errorf("re-seeding erased last_login: got %v, want %v", got.LastLogin, when)
	}
}

func TestUserByNameIsCaseInsensitive(t *testing.T) {
	s := openTest(t)
	u := adminUser()
	if err := s.UpsertUser(u); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	for _, spelling := range []string{"admin", "ADMIN", "Admin", "aDmIn"} {
		got, err := s.UserByName(spelling)
		if err != nil {
			t.Errorf("UserByName(%q): %v", spelling, err)
			continue
		}
		if got == nil {
			t.Errorf("UserByName(%q) = nil, want the admin user", spelling)
			continue
		}
		if got.ID != u.ID {
			t.Errorf("UserByName(%q) = %v, want %v", spelling, got.ID, u.ID)
		}
	}
}

func TestUserLookupMissIsNotAnError(t *testing.T) {
	s := openTest(t)

	got, err := s.UserByID(jfid.HashString("user:nobody", jfid.UTF16LE))
	if err != nil {
		t.Errorf("UserByID(unknown) returned an error: %v", err)
	}
	if got != nil {
		t.Errorf("UserByID(unknown) = %+v, want nil", got)
	}

	for _, name := range []string{"nobody", ""} {
		got, err := s.UserByName(name)
		if err != nil {
			t.Errorf("UserByName(%q) returned an error: %v", name, err)
		}
		if got != nil {
			t.Errorf("UserByName(%q) = %+v, want nil", name, got)
		}
	}
}

func TestUsersIsOrderedAndNonNil(t *testing.T) {
	s := openTest(t)

	empty, err := s.Users()
	if err != nil {
		t.Fatalf("Users on an empty database: %v", err)
	}
	if empty == nil {
		t.Error("Users returned nil for an empty database, want an empty slice")
	}
	if len(empty) != 0 {
		t.Errorf("got %d users, want 0", len(empty))
	}

	// Inserted out of order; Users must sort them.
	for _, name := range []string{"zoe", "admin", "Mallory"} {
		if err := s.UpsertUser(&User{
			ID:   jfid.HashString("user:"+strings.ToLower(name), jfid.UTF16LE),
			Name: name, PasswordHash: "x",
		}); err != nil {
			t.Fatalf("UpsertUser(%q): %v", name, err)
		}
	}

	all, err := s.Users()
	if err != nil {
		t.Fatalf("Users: %v", err)
	}
	got := make([]string, len(all))
	for i, u := range all {
		got[i] = u.Name
	}
	want := []string{"admin", "Mallory", "zoe"} // by name_lower
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestUpsertUserRejectsBadInput(t *testing.T) {
	s := openTest(t)

	if err := s.UpsertUser(nil); err == nil {
		t.Error("UpsertUser(nil) succeeded, want an error")
	}
	if err := s.UpsertUser(&User{Name: "ghost"}); err == nil {
		t.Error("UpsertUser with a zero ID succeeded, want an error")
	}
}

func TestNamesDifferingOnlyInCaseCollide(t *testing.T) {
	s := openTest(t)

	if err := s.UpsertUser(&User{
		ID: jfid.HashString("user:admin", jfid.UTF16LE), Name: "admin", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	// A different ID but the same name_lower must be refused by the UNIQUE
	// index -- otherwise UserByName would have two candidates and login would
	// depend on row order.
	err := s.UpsertUser(&User{
		ID: jfid.HashString("user:other", jfid.UTF16LE), Name: "ADMIN", PasswordHash: "y",
	})
	if err == nil {
		t.Error("two users differing only in case were both accepted")
	}
}

func TestTouchLoginOnMissingUserIsNoOp(t *testing.T) {
	s := openTest(t)

	if err := s.TouchLogin(jfid.HashString("user:nobody", jfid.UTF16LE), time.Now()); err != nil {
		t.Errorf("TouchLogin on an unknown user returned an error: %v", err)
	}
}

package jfdb

import (
	"testing"
	"time"
)

func TestIssueTokenRoundTrip(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	issued, err := s.IssueToken(user, "Delfin", "iPhone", "dev-1", "2.3.0")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	got, err := s.TokenByValue(issued.Value)
	if err != nil {
		t.Fatalf("TokenByValue: %v", err)
	}
	if got == nil {
		t.Fatal("TokenByValue returned nil for a token that was just issued")
	}
	if got.Value != issued.Value || got.UserID != user ||
		got.Client != "Delfin" || got.Device != "iPhone" ||
		got.DeviceID != "dev-1" || got.AppVersion != "2.3.0" {
		t.Errorf("got %+v, want the issued token", got)
	}
	if got.Created.IsZero() || got.LastActivity.IsZero() {
		t.Errorf("timestamps did not survive: created=%v last=%v", got.Created, got.LastActivity)
	}
}

func TestIssueTokenNeverRepeats(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := s.IssueToken(user, "c", "d", "did", "1.0")
		if err != nil {
			t.Fatalf("IssueToken %d: %v", i, err)
		}
		if seen[tok.Value] {
			t.Fatalf("token value %q issued twice", tok.Value)
		}
		seen[tok.Value] = true

		if len(tok.Value) != 32 {
			t.Fatalf("token %q is %d chars, want 32 hex", tok.Value, len(tok.Value))
		}
	}
}

func TestTokenByValueMissIsNotAnError(t *testing.T) {
	s := openTest(t)

	for _, v := range []string{"nosuchtoken", ""} {
		got, err := s.TokenByValue(v)
		if err != nil {
			t.Errorf("TokenByValue(%q) returned an error: %v", v, err)
		}
		if got != nil {
			t.Errorf("TokenByValue(%q) = %+v, want nil", v, got)
		}
	}
}

func TestRevokeToken(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	tok, err := s.IssueToken(user, "c", "d", "did", "1.0")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := s.RevokeToken(tok.Value); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	got, err := s.TokenByValue(tok.Value)
	if err != nil {
		t.Fatalf("TokenByValue after revoke: %v", err)
	}
	if got != nil {
		t.Error("token still resolves after being revoked")
	}

	// Revoking twice is what two clients logging out of one session looks like.
	if err := s.RevokeToken(tok.Value); err != nil {
		t.Errorf("second RevokeToken returned an error: %v", err)
	}
}

func TestRevokeUserTokensLeavesOtherUsersAlone(t *testing.T) {
	s := openTest(t)
	alice := seedUser(t, s, "alice")
	bob := seedUser(t, s, "bob")

	for i := 0; i < 3; i++ {
		if _, err := s.IssueToken(alice, "c", "d", "did", "1.0"); err != nil {
			t.Fatalf("IssueToken alice: %v", err)
		}
	}
	bobToken, err := s.IssueToken(bob, "c", "d", "did", "1.0")
	if err != nil {
		t.Fatalf("IssueToken bob: %v", err)
	}

	if err := s.RevokeUserTokens(alice); err != nil {
		t.Fatalf("RevokeUserTokens: %v", err)
	}

	aliceLeft, err := s.TokensForUser(alice)
	if err != nil {
		t.Fatalf("TokensForUser(alice): %v", err)
	}
	if len(aliceLeft) != 0 {
		t.Errorf("alice has %d tokens left, want 0", len(aliceLeft))
	}

	stillThere, err := s.TokenByValue(bobToken.Value)
	if err != nil {
		t.Fatalf("TokenByValue(bob): %v", err)
	}
	if stillThere == nil {
		t.Error("revoking alice's tokens also removed bob's")
	}
}

func TestTouchActivityLeavesCreatedAlone(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	tok, err := s.IssueToken(user, "c", "d", "did", "1.0")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	later := tok.Created.Add(3 * time.Hour)
	if err := s.TouchActivity(tok.Value, later); err != nil {
		t.Fatalf("TouchActivity: %v", err)
	}

	got, err := s.TokenByValue(tok.Value)
	if err != nil {
		t.Fatalf("TokenByValue: %v", err)
	}
	if got == nil {
		t.Fatal("token vanished after TouchActivity")
	}
	if !got.LastActivity.Equal(later) {
		t.Errorf("LastActivity = %v, want %v", got.LastActivity, later)
	}
	if !got.Created.Equal(tok.Created) {
		t.Errorf("Created changed to %v, want %v -- that is when the session began",
			got.Created, tok.Created)
	}
}

func TestTokensForUserOrderedAndNonNil(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	empty, err := s.TokensForUser(user)
	if err != nil {
		t.Fatalf("TokensForUser: %v", err)
	}
	if empty == nil {
		t.Error("TokensForUser returned nil, want an empty slice")
	}

	for i := 0; i < 3; i++ {
		if _, err := s.IssueToken(user, "c", "d", "did", "1.0"); err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
	}
	all, err := s.TokensForUser(user)
	if err != nil {
		t.Fatalf("TokensForUser: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d tokens, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Created.Before(all[i-1].Created) {
			t.Errorf("tokens are not oldest-first at index %d", i)
		}
	}
}

func TestTokenRequiresAnExistingUser(t *testing.T) {
	s := openTest(t)

	// The foreign key is what stops a token outliving the user it belongs to.
	ghost := seedUser(t, s, "ghost")
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, ghost); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.IssueToken(ghost, "c", "d", "did", "1.0"); err == nil {
		t.Error("issued a token for a user that does not exist")
	}
}

func TestRevokingAUserCascadesToTokens(t *testing.T) {
	s := openTest(t)
	user := seedUser(t, s, "alice")

	tok, err := s.IssueToken(user, "c", "d", "did", "1.0")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	got, err := s.TokenByValue(tok.Value)
	if err != nil {
		t.Fatalf("TokenByValue: %v", err)
	}
	if got != nil {
		t.Error("token survived deletion of its user; ON DELETE CASCADE is not working")
	}
}

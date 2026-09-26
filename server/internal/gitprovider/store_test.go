package gitprovider

import (
	"testing"
	"time"
)

func TestGitProviderStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Initially no account
	if _, ok := s.GetAccount(); ok {
		t.Fatal("expected no account initially")
	}
	if _, ok := s.GetToken(); ok {
		t.Fatal("expected no token initially")
	}

	acc := &Account{
		Login:     "octocat",
		Name:      "The Octocat",
		AvatarURL: "https://github.com/images/error/octocat_happy.gif",
		HTMLURL:   "https://github.com/octocat",
		Token:     "ghp_test1234567890",
		AuthType:  "token",
		UpdatedAt: time.Now(),
	}

	if err := s.SetAccount(acc); err != nil {
		t.Fatalf("SetAccount: %v", err)
	}

	// Reload from disk to verify persistence
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := s2.GetAccount()
	if !ok || got.Login != "octocat" {
		t.Fatalf("expected login octocat, got %+v", got)
	}
	tok, ok := s2.GetToken()
	if !ok || tok != "ghp_test1234567890" {
		t.Fatalf("expected token ghp_test1234567890, got %s", tok)
	}

	// Delete
	if err := s2.DeleteAccount(); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, ok := s2.GetAccount(); ok {
		t.Fatal("expected account to be deleted")
	}
}

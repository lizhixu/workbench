package groups

import (
	"log/slog"
	"testing"
)

func TestCanAccessGroupsUnion(t *testing.T) {
	s, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	prod, err := s.Create(Group{Name: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	staging, err := s.Create(Group{Name: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant(prod.ID, "alice", GrantOperate, "admin"); err != nil {
		t.Fatal(err)
	}

	// Admin and grant-less users are unrestricted.
	if !s.CanAccessGroups("root", true, []string{"prod"}) {
		t.Fatal("admin must reach any group")
	}
	if !s.CanAccessGroups("bob", false, []string{"prod"}) {
		t.Fatal("user with no grants at all must be unrestricted")
	}

	// Union: one granted group is enough.
	if !s.CanAccessGroups("alice", false, []string{"prod"}) {
		t.Fatal("alice must reach prod")
	}
	if s.CanAccessGroups("alice", false, []string{"staging"}) {
		t.Fatal("alice must not reach staging")
	}
	if !s.CanAccessGroups("alice", false, []string{"staging", "prod"}) {
		t.Fatal("alice must reach a host in [staging prod] via prod")
	}
	// A host in no group is invisible to a restricted user.
	if s.CanAccessGroups("alice", false, nil) {
		t.Fatal("alice must not reach an ungrouped host")
	}
	// Granting the second group opens it too.
	if err := s.SetGrant(staging.ID, "alice", GrantView, "admin"); err != nil {
		t.Fatal(err)
	}
	if !s.CanAccessGroups("alice", false, []string{"staging"}) {
		t.Fatal("alice must reach staging after the grant")
	}
}

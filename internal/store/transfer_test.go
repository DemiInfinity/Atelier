package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMoveRepoAndRedirects(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	me, _ := s.CreateOwner(ctx, "zoe", "", "user")
	co, _ := s.CreateOwner(ctx, "studio", "", "company")
	r, _ := s.CreateRepo(ctx, me.ID, "thing", "", true)
	clash, _ := s.CreateRepo(ctx, co.ID, "taken", "", true)
	_ = clash

	moved, err := s.MoveRepo(ctx, r.ID, co.ID, "thing")
	if err != nil || moved.Owner != "studio" || moved.ID != r.ID {
		t.Fatalf("move: %+v %v", moved, err)
	}
	// The old address still finds it.
	if got, err := s.RepoByName(ctx, "zoe", "thing"); err != nil || got.ID != r.ID || got.Owner != "studio" {
		t.Fatalf("old name lookup: %+v %v", got, err)
	}
	if red, _ := s.RepoRedirects(ctx, r.ID); len(red) != 1 || red[0] != "zoe/thing" {
		t.Errorf("redirects = %v", red)
	}
	// Moving onto a taken name is refused.
	if _, err := s.MoveRepo(ctx, r.ID, co.ID, "taken"); err != ErrNameTaken {
		t.Errorf("clash: %v", err)
	}
	// Rename within the account adds a second redirect; both lead to it.
	if _, err := s.MoveRepo(ctx, r.ID, co.ID, "thing2"); err != nil {
		t.Fatal(err)
	}
	for _, n := range [][2]string{{"zoe", "thing"}, {"studio", "thing"}, {"studio", "thing2"}} {
		if got, err := s.RepoByName(ctx, n[0], n[1]); err != nil || got.ID != r.ID {
			t.Errorf("%s/%s → %+v %v", n[0], n[1], got, err)
		}
	}
	// A new repo that takes an old name wins over the redirect.
	fresh, err := s.CreateRepo(ctx, me.ID, "thing", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RepoByName(ctx, "zoe", "thing"); got.ID != fresh.ID {
		t.Errorf("old name should now be the new repo, got id %d", got.ID)
	}
	// Moving back home onto its own redirect works and clears it.
	if _, err := s.MoveRepo(ctx, r.ID, co.ID, "thing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveRepo(ctx, r.ID, co.ID, "thing"); err != ErrSamePlace {
		t.Errorf("same place: %v", err)
	}
}

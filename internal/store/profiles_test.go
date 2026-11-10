package store

import (
	"context"
	"errors"
	"testing"
)

func TestProfileCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	me, _ := s.CreateOwner(ctx, "demi", "", "user")
	co, _ := s.CreateOwner(ctx, "elyndra", "Elyndra Studios", "company")

	p, err := s.Profile(ctx, me.ID)
	if err != nil || p.OwnerID != me.ID || !p.IsEmpty() || !p.UpdatedAt.IsZero() {
		t.Fatalf("unsaved profile = %+v, %v; want empty", p, err)
	}

	want := Profile{OwnerID: me.ID, Bio: "Makes things.", Location: "Dublin", Website: "https://demi.example",
		Pronouns: "she/her", Avatar: "spark", UpdatedBy: "demi", UpdatedByKind: "owner", UpdatedByPerson: me.ID}
	if err := s.SaveProfile(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Profile(ctx, me.ID)
	if got.Bio != want.Bio || got.Location != want.Location || got.Website != want.Website ||
		got.Pronouns != want.Pronouns || got.Avatar != want.Avatar || got.UpdatedBy != "demi" ||
		got.UpdatedByKind != "owner" || got.UpdatedByPerson != me.ID || got.UpdatedAt.IsZero() {
		t.Errorf("saved profile = %+v", got)
	}

	// Update replaces; companies have their own row.
	want.Bio, want.Pronouns = "Still making things.", ""
	if err := s.SaveProfile(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProfile(ctx, Profile{OwnerID: co.ID, Bio: "A studio."}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Profile(ctx, me.ID)
	if got.Bio != "Still making things." || got.Pronouns != "" {
		t.Errorf("updated profile = %+v", got)
	}
	if c, _ := s.Profile(ctx, co.ID); c.Bio != "A studio." || c.UpdatedByPerson != 0 {
		t.Errorf("company profile = %+v", c)
	}
	if err := s.SaveProfile(ctx, Profile{}); err == nil {
		t.Error("saving a profile without an account should fail")
	}

	if err := s.SetDisplayName(ctx, co.ID, "Elyndra"); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.OwnerByID(ctx, co.ID); o.DisplayName != "Elyndra" {
		t.Errorf("display name = %q", o.DisplayName)
	}
	if err := s.SetDisplayName(ctx, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDisplayName on missing account = %v", err)
	}
}

func TestPinnedRepos(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	me, _ := s.CreateOwner(ctx, "demi", "", "user")
	co, _ := s.CreateOwner(ctx, "elyndra", "", "company")
	var ids []int64
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		r, err := s.CreateRepo(ctx, me.ID, n, "", false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	work, _ := s.CreateRepo(ctx, co.ID, "work", "", true)

	names := func() string {
		rs, err := s.PinnedRepos(ctx, me.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, r := range rs {
			out += r.Name
		}
		return out
	}

	if got := names(); got != "" {
		t.Fatalf("no pins yet, got %q", got)
	}
	// Order is kept and duplicates dropped.
	if err := s.SetPinnedRepos(ctx, me.ID, []int64{ids[2], ids[0], ids[2], ids[4]}); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "cae" {
		t.Errorf("pins = %q, want cae", got)
	}
	// Replacing reorders.
	if err := s.SetPinnedRepos(ctx, me.ID, []int64{ids[4], ids[2]}); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "ec" {
		t.Errorf("pins = %q, want ec", got)
	}
	// At most six; a failed save changes nothing.
	if err := s.SetPinnedRepos(ctx, me.ID, ids); !errors.Is(err, ErrTooManyPins) {
		t.Errorf("seven pins: err = %v, want ErrTooManyPins", err)
	}
	if got := names(); got != "ec" {
		t.Errorf("after refused save pins = %q", got)
	}
	if err := s.SetPinnedRepos(ctx, me.ID, ids[:MaxPins]); err != nil {
		t.Errorf("six pins: %v", err)
	}
	// Only the account's own repositories.
	if err := s.SetPinnedRepos(ctx, me.ID, []int64{work.ID}); !errors.Is(err, ErrPinNotOwned) {
		t.Errorf("pinning another account's repo: err = %v", err)
	}
	if err := s.SetPinnedRepos(ctx, me.ID, []int64{12345}); !errors.Is(err, ErrNotFound) {
		t.Errorf("pinning a missing repo: err = %v", err)
	}
	// Deleting a repository unpins it.
	if err := s.DeleteRepo(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "bcdef" {
		t.Errorf("after delete pins = %q, want bcdef", got)
	}
	// Empty list clears.
	if err := s.SetPinnedRepos(ctx, me.ID, nil); err != nil || names() != "" {
		t.Errorf("clearing pins: %v, %q", err, names())
	}
}

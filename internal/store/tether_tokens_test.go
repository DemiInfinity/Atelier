package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"atelier/internal/secrets"
)

func TestTetherExtraTokensAddListRemove(t *testing.T) {
	s, kr, me, _ := secretsFixture(t)
	ctx := context.Background()
	if err := s.SaveTetherAccount(ctx, kr, TetherAccount{PersonID: me.ID, Login: "DemiInfinity", Method: "pat"}, "github_pat_main"); err != nil {
		t.Fatal(err)
	}
	org, err := s.AddTetherExtraToken(ctx, kr, TetherExtraToken{PersonID: me.ID, Label: "Studio", Login: "DemiInfinity",
		Owners: []string{"Elyndra-Studios, elyndra-studios", " Other "}}, "github_pat_org")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(org.Owners, ","); got != "Elyndra-Studios,Other" {
		t.Errorf("owners = %q", got)
	}
	if _, err := s.AddTetherExtraToken(ctx, kr, TetherExtraToken{PersonID: me.ID}, "x"); err == nil {
		t.Error("a token without owners was saved")
	}
	all, err := s.TetherExtraTokens(ctx, me.ID)
	if err != nil || len(all) != 1 || all[0].Label != "Studio" || all[0].RateRemaining != -1 {
		t.Fatalf("list = %+v, %v", all, err)
	}

	// Sealed, bound to the person and the row.
	var ct []byte
	if err := s.DB().QueryRow(`SELECT token_ct FROM tether_tokens WHERE id=?`, org.ID).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "github_pat_org") {
		t.Fatal("token stored in the clear")
	}
	if _, err := kr.Open(tetherTokenAAD(org.ID, me.ID+1), ct); err == nil {
		t.Error("opened with another person's binding")
	}
	if _, err := kr.Open(tetherAAD(me.ID), ct); err == nil {
		t.Error("opened with the main token's binding")
	}
	if tok, err := s.TetherExtraTokenSecret(ctx, kr, me.ID, org.ID); err != nil || tok != "github_pat_org" {
		t.Fatalf("secret = %q, %v", tok, err)
	}

	// Selection by owner, case-insensitive; no match means the main token.
	if got, err := s.TetherTokenForOwner(ctx, me.ID, "ELYNDRA-studios"); err != nil || got.ID != org.ID {
		t.Errorf("for org = %+v, %v", got, err)
	}
	if _, err := s.TetherTokenForOwner(ctx, me.ID, "DemiInfinity"); !errors.Is(err, ErrNotFound) {
		t.Errorf("personal owner picked an extra token: %v", err)
	}
	if err := s.SetTetherExtraTokenOwners(ctx, me.ID, org.ID, []string{"elyndra-studios"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TetherTokenForOwner(ctx, me.ID, "Other"); !errors.Is(err, ErrNotFound) {
		t.Error("owner list not updated")
	}

	// Another person: nothing of it.
	sam, _ := s.CreateOwner(ctx, "sam", "", "user")
	if _, err := s.TetherTokenForOwner(ctx, sam.ID, "Elyndra-Studios"); !errors.Is(err, ErrNotFound) {
		t.Error("sam got zoe's token")
	}
	if _, err := s.TetherExtraTokenSecret(ctx, kr, sam.ID, org.ID); err == nil {
		t.Error("sam opened zoe's token by id")
	}
	if err := s.DeleteTetherExtraToken(ctx, sam.ID, org.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("sam deleted zoe's token: %v", err)
	}
	if err := s.SetTetherExtraTokenOwners(ctx, sam.ID, org.ID, []string{"x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("sam changed zoe's token: %v", err)
	}

	// Remove; what was cached with it goes too.
	_ = s.TetherCachePut(ctx, me.ID, "GET /repos/a/b"+TetherTokenCacheSuffix(org.ID), "e", []byte("{}"))
	_ = s.TetherCachePut(ctx, me.ID, "GET /repos/a/b", "e", []byte("{}"))
	if err := s.DeleteTetherExtraToken(ctx, me.ID, org.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.TetherExtraTokens(ctx, me.ID); len(all) != 0 {
		t.Error("token not removed")
	}
	if _, _, _, err := s.TetherCacheGet(ctx, me.ID, "GET /repos/a/b"+TetherTokenCacheSuffix(org.ID)); err == nil {
		t.Error("the token's cache survived")
	}
	if _, _, _, err := s.TetherCacheGet(ctx, me.ID, "GET /repos/a/b"); err != nil {
		t.Error("the main token's cache was dropped")
	}

	// Disconnecting the main account removes every token.
	if _, err := s.AddTetherExtraToken(ctx, kr, TetherExtraToken{PersonID: me.ID, Owners: []string{"x"}}, "t2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTetherAccount(ctx, me.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.TetherExtraTokens(ctx, me.ID); len(all) != 0 {
		t.Error("extra tokens survived Disconnect")
	}
}

// A person linked before tether_tokens existed: the single token keeps
// working as the main token, for every owner.
func TestTetherSingleTokenStillWorks(t *testing.T) {
	s, kr, me, _ := secretsFixture(t)
	ctx := context.Background()
	if err := s.SaveTetherAccount(ctx, kr, TetherAccount{PersonID: me.ID, Login: "zoe", Method: "pat"}, "ghp_old"); err != nil {
		t.Fatal(err)
	}
	if all, err := s.TetherExtraTokens(ctx, me.ID); err != nil || len(all) != 0 {
		t.Fatalf("extra = %v, %v", all, err)
	}
	if _, err := s.TetherTokenForOwner(ctx, me.ID, "Elyndra-Studios"); !errors.Is(err, ErrNotFound) {
		t.Errorf("for owner = %v", err)
	}
	if tok, err := s.TetherToken(ctx, kr, me.ID); err != nil || tok != "ghp_old" {
		t.Errorf("main = %q, %v", tok, err)
	}
}

func TestRotateSecretsResealsEveryTetherToken(t *testing.T) {
	s, kr, me, _ := secretsFixture(t)
	ctx := context.Background()
	sam, _ := s.CreateOwner(ctx, "sam", "", "user")
	if err := s.SaveTetherAccount(ctx, kr, TetherAccount{PersonID: me.ID, Login: "zoe", Method: "pat"}, "main"); err != nil {
		t.Fatal(err)
	}
	a, _ := s.AddTetherExtraToken(ctx, kr, TetherExtraToken{PersonID: me.ID, Owners: []string{"org-a"}}, "tok-a")
	b, _ := s.AddTetherExtraToken(ctx, kr, TetherExtraToken{PersonID: me.ID, Owners: []string{"org-b"}}, "tok-b")
	c, _ := s.AddTetherExtraToken(ctx, kr, TetherExtraToken{PersonID: sam.ID, Owners: []string{"org-c"}}, "tok-c")
	to, _ := secrets.NewKeyring(secrets.GenerateKey())
	n, err := s.RotateSecrets(ctx, kr, to)
	if err != nil || n != 4 {
		t.Fatalf("Rotate = %d, %v", n, err)
	}
	for _, x := range []struct {
		person, id int64
		want       string
	}{{me.ID, a.ID, "tok-a"}, {me.ID, b.ID, "tok-b"}, {sam.ID, c.ID, "tok-c"}} {
		if got, err := s.TetherExtraTokenSecret(ctx, to, x.person, x.id); err != nil || got != x.want {
			t.Errorf("token %d after rotation = %q, %v", x.id, got, err)
		}
		if _, err := s.TetherExtraTokenSecret(ctx, kr, x.person, x.id); err == nil {
			t.Errorf("old key still opens token %d", x.id)
		}
	}
	if got, err := s.TetherToken(ctx, to, me.ID); err != nil || got != "main" {
		t.Errorf("main after rotation = %q, %v", got, err)
	}
}

package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "atelier.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOwners(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.PersonalOwner(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PersonalOwner on empty db: got %v, want ErrNotFound", err)
	}

	// Companies created first, and named before the user alphabetically, must still sort after.
	if _, err := s.CreateOwner(ctx, "Acme", "Acme Inc", "company"); err != nil {
		t.Fatal(err)
	}
	me, err := s.CreateOwner(ctx, "zoe", "Zoë", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwner(ctx, "Elyndra", "", "company"); err != nil {
		t.Fatal(err)
	}
	if me.Kind != "user" || me.IsCompany() || me.Label() != "Zoë" || me.Initial() != "Z" {
		t.Errorf("unexpected personal owner %+v", me)
	}

	owners, err := s.Owners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range owners {
		names = append(names, o.Name)
	}
	if got := strings.Join(names, ","); got != "zoe,Acme,Elyndra" {
		t.Errorf("Owners order = %s, want zoe,Acme,Elyndra", got)
	}
	if !owners[1].IsCompany() || owners[2].Label() != "Elyndra" {
		t.Errorf("company owners wrong: %+v", owners[1:])
	}

	p, err := s.PersonalOwner(ctx)
	if err != nil || p.ID != me.ID {
		t.Errorf("PersonalOwner = %+v, %v", p, err)
	}
	o, err := s.OwnerByName(ctx, "ACME")
	if err != nil || o.Name != "Acme" || o.Kind != "company" {
		t.Errorf("OwnerByName case-insensitive = %+v, %v", o, err)
	}
	if _, err := s.OwnerByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OwnerByName missing: %v", err)
	}
	if _, err := s.CreateOwner(ctx, "acme", "", "company"); err == nil {
		t.Error("duplicate owner name (different case) should fail")
	}
	if _, err := s.CreateOwner(ctx, "bad", "", "team"); err == nil {
		t.Error("invalid kind should fail")
	}
}

func TestRepos(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	me, _ := s.CreateOwner(ctx, "zoe", "", "user")
	co, _ := s.CreateOwner(ctx, "Acme", "", "company")

	r, err := s.CreateRepo(ctx, me.ID, "Atelier", "a forge", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.FullName() != "zoe/Atelier" || !r.Private || r.Description != "a forge" || r.OwnerKind != "user" {
		t.Errorf("CreateRepo = %+v", r)
	}
	if _, err := s.CreateRepo(ctx, me.ID, "atelier", "", false); err == nil {
		t.Error("duplicate repo name (different case) should fail")
	}
	if _, err := s.CreateRepo(ctx, co.ID, "atelier", "", false); err != nil {
		t.Errorf("same name under another owner should work: %v", err)
	}

	got, err := s.RepoByName(ctx, "ZOE", "ATELIER")
	if err != nil || got.ID != r.ID {
		t.Fatalf("RepoByName case-insensitive = %+v, %v", got, err)
	}
	if _, err := s.RepoByName(ctx, "zoe", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RepoByName missing: %v", err)
	}

	if err := s.UpdateRepo(ctx, r.ID, "new desc", false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RepoByName(ctx, "zoe", "atelier")
	if got.Description != "new desc" || got.Private {
		t.Errorf("UpdateRepo not applied: %+v", got)
	}

	all, _ := s.Repos(ctx)
	mine, _ := s.ReposByOwner(ctx, me.ID)
	if len(all) != 2 || len(mine) != 1 {
		t.Errorf("Repos=%d ReposByOwner=%d, want 2 and 1", len(all), len(mine))
	}

	if err := s.DeleteRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RepoByName(ctx, "zoe", "atelier"); !errors.Is(err, ErrNotFound) {
		t.Errorf("repo still there after delete: %v", err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.Setting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing setting: %v", err)
	}
	_ = s.SetSetting(ctx, "k", "1")
	_ = s.SetSetting(ctx, "k", "2")
	if v, err := s.Setting(ctx, "k"); err != nil || v != "2" {
		t.Errorf("Setting = %q, %v", v, err)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	tok, err := s.CreateSession(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidSession(ctx, tok) {
		t.Error("fresh session should be valid")
	}
	if s.ValidSession(ctx, tok+"x") || s.ValidSession(ctx, "") {
		t.Error("unknown session should be invalid")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash=?`, tok).Scan(&n)
	if n != 0 {
		t.Error("session token stored in plaintext")
	}

	expired, _ := s.CreateSession(ctx, -time.Minute)
	if s.ValidSession(ctx, expired) {
		t.Error("expired session should be invalid")
	}

	if err := s.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if s.ValidSession(ctx, tok) {
		t.Error("deleted session should be invalid")
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("DeleteSession should also sweep expired sessions, %d left", n)
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	plain, err := s.CreateToken(ctx, "laptop", []string{"repo:read", "repo:write"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) || len(plain) != len(TokenPrefix)+40 {
		t.Fatalf("token %q has the wrong shape", plain)
	}

	tok, err := s.CheckToken(ctx, plain)
	if err != nil {
		t.Fatalf("CheckToken: %v", err)
	}
	if tok.Name != "laptop" || !tok.Has("repo:write") || !tok.Has("repo:read") || tok.Has("admin") {
		t.Errorf("token = %+v", tok)
	}
	if tok.ExpiresAt != nil {
		t.Error("ttl 0 should mean no expiry")
	}
	if tok.Prefix != plain[:len(TokenPrefix)+6] {
		t.Errorf("prefix = %q, want %q", tok.Prefix, plain[:len(TokenPrefix)+6])
	}

	// last_used_at is recorded.
	list, _ := s.Tokens(ctx)
	if len(list) != 1 || list[0].LastUsedAt == nil || list[0].Prefix != tok.Prefix {
		t.Errorf("Tokens = %+v", list)
	}

	// The plaintext never touches the database: dump every text column of every row.
	rows, err := s.db.Query(`SELECT name, token_hash, prefix, scopes FROM tokens`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b, c, d string
		rows.Scan(&a, &b, &c, &d)
		for _, col := range []string{a, b, c, d} {
			if strings.Contains(col, plain) || strings.Contains(col, plain[len(TokenPrefix):]) {
				t.Errorf("plaintext token found in a column: %q", col)
			}
		}
		if b != Hash(plain) {
			t.Errorf("token_hash = %q, want sha256 of the token", b)
		}
	}
	rows.Close()

	for _, bad := range []string{"", plain + "x", plain[:len(plain)-1], "atl_" + strings.Repeat("0", 40), strings.TrimPrefix(plain, TokenPrefix), Hash(plain)} {
		if _, err := s.CheckToken(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("CheckToken(%q) = %v, want ErrNotFound", bad, err)
		}
	}

	read, _ := s.CreateToken(ctx, "ci", []string{"repo:read"}, time.Hour)
	rt, err := s.CheckToken(ctx, read)
	if err != nil || rt.Has("repo:write") || rt.ExpiresAt == nil {
		t.Errorf("read token = %+v, %v", rt, err)
	}

	expired, _ := s.CreateToken(ctx, "old", []string{"repo:read"}, time.Hour)
	if _, err := s.db.Exec(`UPDATE tokens SET expires_at=? WHERE name='old'`, time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckToken(ctx, expired); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token accepted: %v", err)
	}

	if err := s.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckToken(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token accepted: %v", err)
	}
}

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPersonOwnership(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	me, _ := s.CreateOwner(ctx, "zoe", "", "user")
	co, _ := s.CreateOwner(ctx, "studio", "", "company")
	other, _ := s.CreateOwner(ctx, "pat", "", "user")

	// Single-person code paths belong to the personal owner.
	sess, _ := s.CreateSession(ctx, time.Hour)
	if id, ok := s.SessionPerson(ctx, sess); !ok || id != me.ID {
		t.Errorf("session person = %d, want owner %d", id, me.ID)
	}
	tok, _ := s.CreateToken(ctx, "t", []string{"repo:read"}, time.Hour)
	tk, _ := s.CheckToken(ctx, tok)
	if id, _ := s.TokenPerson(ctx, tk.ID); id != me.ID {
		t.Errorf("token person = %d, want %d", id, me.ID)
	}

	// Keys and passkeys follow the person named in added_by.
	k1, _ := s.AddSSHKey(ctx, "a", "ssh-ed25519", "SHA256:a", "ssh-ed25519 A", "zoe", "owner")
	k2, _ := s.AddSSHKey(ctx, "b", "ssh-ed25519", "SHA256:b", "ssh-ed25519 B", "pat", "owner")
	if id, _ := s.SSHKeyPerson(ctx, k1.ID); id != me.ID {
		t.Errorf("zoe's key belongs to %d", id)
	}
	if id, _ := s.SSHKeyPerson(ctx, k2.ID); id != other.ID {
		t.Errorf("pat's key belongs to %d, want %d", id, other.ID)
	}

	// Sessions for a specific person; companies aren't people.
	s2, _ := s.CreateSessionFor(ctx, other.ID, time.Hour)
	if id, ok := s.SessionPerson(ctx, s2); !ok || id != other.ID {
		t.Errorf("pat's session = %d", id)
	}
	if _, err := s.Person(ctx, co.ID); err != ErrNotFound {
		t.Errorf("a company account must not count as a person, got %v", err)
	}
	if _, err := s.CreateSessionFor(ctx, 0, time.Hour); err == nil {
		t.Error("a session without a person should be refused")
	}

	// Reopening runs no migration twice and keeps the data.
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if id, ok := s.SessionPerson(ctx, sess); !ok || id != me.ID {
		t.Errorf("after reopen, session person = %d", id)
	}
	if v, _ := s.Setting(ctx, settingSchemaVersion); v != "1" {
		t.Errorf("schema version = %q, want 1", v)
	}
}

// A database from before person ownership (no person_id columns) is upgraded
// in place and everything is assigned to the owner.
func TestMigrateLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	me, _ := s.CreateOwner(ctx, "zoe", "", "user")
	// Simulate an old database: drop the columns' data and the version marker.
	if _, err := s.db.Exec(`UPDATE sessions SET person_id = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, expires_at) VALUES (?, ?)`, Hash("old"), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, settingSchemaVersion); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var pid int64
	if err := s.db.QueryRow(`SELECT person_id FROM sessions WHERE token_hash=?`, Hash("old")).Scan(&pid); err != nil || pid != me.ID {
		t.Errorf("legacy session person = %d (%v), want %d", pid, err, me.ID)
	}
}

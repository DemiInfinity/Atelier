package store

import (
	"context"
	"path/filepath"
	"strconv"
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
	if v, _ := s.Setting(ctx, settingSchemaVersion); v != strconv.Itoa(len(migrations)) {
		t.Errorf("schema version = %q, want %d", v, len(migrations))
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

// A database from before the "waiting for Atelier" badge (tether_badges has
// no waiting column) gains it in place; cached badges keep what they said.
func TestMigrateTetherBadgeWaiting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "badges.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the earlier release: the table as it was first created, one
	// cached badge, and the schema version from before this migration.
	at := time.Now().Add(-time.Hour).Unix()
	for _, q := range []string{
		`DROP TABLE tether_badges`,
		`CREATE TABLE tether_badges (
  login      TEXT PRIMARY KEY COLLATE NOCASE,
  present    INTEGER NOT NULL DEFAULT 0,
  studio     TEXT NOT NULL DEFAULT '',
  etag       TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL
)`,
		`INSERT INTO tether_badges (login, present, studio, etag, checked_at) VALUES ('dave', 1, 'https://studio.example/dave', 'abc', ` + strconv.FormatInt(at, 10) + `)`,
		`UPDATE settings SET value='3' WHERE key='` + settingSchemaVersion + `'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.TetherBadge(ctx, "dave"); err == nil {
		t.Fatal("the old table already has the new columns: this test proves nothing")
	}
	s.Close()

	for round := 1; round <= 2; round++ { // the second open must be a no-op
		s, err = Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", round, err)
		}
		if v, _ := s.Setting(ctx, settingSchemaVersion); v != strconv.Itoa(len(migrations)) {
			t.Errorf("open %d: schema version = %q, want %d", round, v, len(migrations))
		}
		b, err := s.TetherBadge(ctx, "DAVE")
		if err != nil || !b.Present || b.Waiting || b.Early || b.Studio != "https://studio.example/dave" || b.ETag != "abc" || b.CheckedAt.Unix() != at {
			t.Errorf("open %d: old badge = %+v, %v", round, b, err)
		}
		if round == 2 {
			if w, err := s.TetherBadge(ctx, "wendy"); err != nil || !w.Waiting || w.Present || w.Early {
				t.Errorf("waiting badge after reopen = %+v, %v", w, err)
			}
			if e, err := s.TetherBadge(ctx, "earl"); err != nil || !e.Present || !e.Early || e.Waiting {
				t.Errorf("early badge after reopen = %+v, %v", e, err)
			}
		}
		if err := s.SaveTetherBadge(ctx, TetherBadge{Login: "earl", Present: true, Early: true, CheckedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveTetherBadge(ctx, TetherBadge{Login: "wendy", Waiting: true, CheckedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}

	// Present wins when both are given, and a badge can stop waiting.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveTetherBadge(ctx, TetherBadge{Login: "wendy", Present: true, Waiting: true, CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.TetherBadge(ctx, "wendy"); !w.Present || w.Waiting {
		t.Errorf("present and waiting = %+v", w)
	}
	if err := s.SaveTetherBadge(ctx, TetherBadge{Login: "wendy", CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.TetherBadge(ctx, "wendy"); w.Present || w.Waiting {
		t.Errorf("cleared badge = %+v", w)
	}
	// Early means nothing without Present.
	if err := s.SaveTetherBadge(ctx, TetherBadge{Login: "keen", Waiting: true, Early: true, CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.TetherBadge(ctx, "keen"); k.Present || !k.Waiting || k.Early {
		t.Errorf("early without present = %+v", k)
	}
}

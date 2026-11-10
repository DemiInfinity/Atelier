package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/secrets"
	"atelier/internal/store"
)

func rotateFixture(t *testing.T) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	me, _ := db.CreateOwner(ctx, "zoe", "", "user")
	rp, _ := db.CreateRepo(ctx, me.ID, "demo", "", true)
	kr, _, err := secrets.Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	by := store.SecretsBy{Name: "zoe", Kind: "owner"}
	if err := db.SetSecret(ctx, kr, store.RepoScope(rp.ID), "ONE", "first", by); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSecret(ctx, kr, store.AccountScope(me.ID), "TWO", "second", by); err != nil {
		t.Fatal(err)
	}
	return dir, rp.ID
}

func resolveWith(t *testing.T, dir string, kr *secrets.Keyring, repoID int64) map[string]string {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.ResolveSecrets(context.Background(), kr, repoID, "", []string{"ONE", "TWO"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return got
}

func TestRotateKeyFile(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	dir, repoID := rotateFixture(t)
	before, _ := os.ReadFile(secrets.KeyPath(dir))

	var out bytes.Buffer
	if err := secretsCommand(&out, dir, []string{"rotate-key"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Re-encrypted 2 secret(s)") {
		t.Errorf("output = %q", out.String())
	}
	after, _ := os.ReadFile(secrets.KeyPath(dir))
	old, _ := os.ReadFile(filepath.Join(dir, secrets.OldKeyFile))
	if bytes.Equal(before, after) || !bytes.Equal(before, old) {
		t.Fatal("key file wasn't replaced, or the old key wasn't kept")
	}
	if _, err := os.Stat(filepath.Join(dir, secrets.NextKeyFile)); !os.IsNotExist(err) {
		t.Error("secret.key.next left behind")
	}
	if fi, _ := os.Stat(secrets.KeyPath(dir)); fi.Mode().Perm() != 0o600 {
		t.Errorf("new key mode = %v", fi.Mode().Perm())
	}
	raw, _ := secrets.DecodeKey(string(after))
	only, _ := secrets.NewKeyring(raw)
	if got := resolveWith(t, dir, only, repoID); got["ONE"] != "first" || got["TWO"] != "second" {
		t.Errorf("after rotation = %v", got)
	}
	// The old key alone can no longer open them.
	oldRaw, _ := secrets.DecodeKey(string(before))
	oldKR, _ := secrets.NewKeyring(oldRaw)
	db, _ := store.Open(filepath.Join(dir, "atelier.db"))
	if _, err := db.ResolveSecrets(context.Background(), oldKR, repoID, "", []string{"ONE"}); err == nil {
		t.Error("old key still opens secrets")
	}
	db.Close()

	// Running it again rotates again.
	out.Reset()
	if err := secretsCommand(&out, dir, []string{"rotate-key"}); err != nil {
		t.Fatal(err)
	}
	kr, _, _ := secrets.Load(dir, false)
	if got := resolveWith(t, dir, kr, repoID); got["ONE"] != "first" {
		t.Errorf("after second rotation = %v", got)
	}
}

func TestRotateKeyFinishesInterruptedRotation(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	dir, repoID := rotateFixture(t)
	// A previous run wrote secret.key.next, re-encrypted, then crashed.
	cur, _, _ := secrets.Load(dir, false)
	raw := secrets.GenerateKey()
	if err := secrets.WriteKeyFile(filepath.Join(dir, secrets.NextKeyFile), raw); err != nil {
		t.Fatal(err)
	}
	next, _ := secrets.NewKeyring(raw)
	db, _ := store.Open(filepath.Join(dir, "atelier.db"))
	if _, err := db.RotateSecrets(context.Background(), cur, next); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Meanwhile everything is still readable with what Load finds.
	kr, _, _ := secrets.Load(dir, false)
	if got := resolveWith(t, dir, kr, repoID); got["ONE"] != "first" {
		t.Fatalf("mid-rotation = %v", got)
	}
	var out bytes.Buffer
	if err := secretsCommand(&out, dir, []string{"rotate-key"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Finishing an earlier rotation") {
		t.Errorf("output = %q", out.String())
	}
	installed, _ := secrets.ReadKeyFile(secrets.KeyPath(dir))
	if !bytes.Equal(installed, raw) {
		t.Error("the interrupted rotation's key wasn't installed")
	}
	only, _ := secrets.NewKeyring(raw)
	if got := resolveWith(t, dir, only, repoID); got["TWO"] != "second" {
		t.Errorf("after finishing = %v", got)
	}
}

func TestRotateKeyFromEnv(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	dir, repoID := rotateFixture(t)
	// Switch to an environment key: move the file key into the environment.
	fileKey, _ := secrets.ReadKeyFile(secrets.KeyPath(dir))
	os.Remove(secrets.KeyPath(dir))
	t.Setenv(secrets.EnvKey, secrets.EncodeKey(fileKey))

	var out bytes.Buffer
	if err := secretsCommand(&out, dir, []string{"rotate-key"}); err != nil {
		t.Fatal(err)
	}
	next, err := secrets.ReadKeyFile(filepath.Join(dir, secrets.NextKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ATELIER_SECRET_KEY="+secrets.EncodeKey(next)) {
		t.Errorf("output doesn't give the new key: %q", out.String())
	}
	// Still readable before the environment is updated (Load adds .next)…
	kr, _, _ := secrets.Load(dir, false)
	if got := resolveWith(t, dir, kr, repoID); got["ONE"] != "first" {
		t.Errorf("before env update = %v", got)
	}
	// …and after.
	t.Setenv(secrets.EnvKey, secrets.EncodeKey(next))
	kr, _, _ = secrets.Load(dir, false)
	if got := resolveWith(t, dir, kr, repoID); got["TWO"] != "second" {
		t.Errorf("after env update = %v", got)
	}
}

func TestSecretsCommandUsage(t *testing.T) {
	var out bytes.Buffer
	if err := secretsCommand(&out, t.TempDir(), nil); err == nil {
		t.Error("no subcommand accepted")
	}
	if err := secretsCommand(&out, t.TempDir(), []string{"nope"}); err == nil {
		t.Error("unknown subcommand accepted")
	}
	if _, ok := commands["secrets"]; !ok {
		t.Error("secrets command not registered")
	}
}

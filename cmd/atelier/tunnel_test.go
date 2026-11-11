package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/backup"
	"atelier/internal/config"
	"atelier/internal/secrets"
	"atelier/internal/store"
)

func TestRotateKeyReSealsTunnelToken(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	kr, _, err := secrets.Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	tok := "eyJhIjoidHVubmVsLXRva2VuIn0"
	c, _ := db.TunnelConfig(ctx)
	c.Mode, c.Hostname = "named", "git.example.com"
	if err := db.SaveTunnelConfig(ctx, kr, c, &tok, "zoe"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out bytes.Buffer
	if err := rotateSecretKey(ctx, &out, dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Re-encrypted 1 secret(s)") {
		t.Errorf("rotate output: %s", out.String())
	}
	newKey, err := secrets.ReadKeyFile(secrets.KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	onlyNew, _ := secrets.NewKeyring(newKey)
	db, _ = store.Open(filepath.Join(dir, "atelier.db"))
	defer db.Close()
	if got, err := db.TunnelToken(ctx, onlyNew); err != nil || got != tok {
		t.Fatalf("token with the new key alone = %q, %v", got, err)
	}
	if _, err := db.TunnelToken(ctx, kr); err == nil {
		t.Error("the old key still opens the token: it wasn't re-sealed")
	}
}

func TestRestoreKeepsTunnelOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ATELIER_DATA", dir)
	t.Setenv("ATELIER_BACKUP_DIR", filepath.Join(t.TempDir(), "backups"))
	cfg := config.FromEnv()
	ctx := context.Background()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOwner(ctx, "zoe", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetTunnelEnabled(ctx, true, "zoe"); err != nil {
		t.Fatal(err)
	}
	outc := backup.Run(ctx, backup.PlanFromConfig(cfg, db, "git"))
	db.Close()
	if outc.Err != nil {
		t.Fatal(outc.Err)
	}
	if err := restoreCmd([]string{outc.Result.Path, "--force"}); err != nil {
		t.Fatal(err)
	}
	db, _ = store.Open(cfg.DBPath())
	defer db.Close()
	c, err := db.TunnelConfig(ctx)
	if err != nil || c.Enabled {
		t.Fatalf("after restoring a snapshot taken while on: %+v, %v", c, err)
	}
	if o, err := db.PersonalOwner(ctx); err != nil || o.Name != "zoe" {
		t.Fatalf("restore didn't restore: %v", err)
	}
}

func TestTunnelOffCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir}
	ctx := context.Background()
	db, _ := store.Open(cfg.DBPath())
	me, _ := db.CreateOwner(ctx, "zoe", "", "user")
	_, _ = db.SetTunnelEnabled(ctx, true, "zoe")
	remote, _ := db.CreateSessionFor(ctx, me.ID, time.Hour)
	_ = db.MarkRemoteSession(ctx, remote, me.ID, "x.trycloudflare.com", false)
	lan, _ := db.CreateSessionFor(ctx, me.ID, time.Hour)
	db.Close()

	var out bytes.Buffer
	if err := tunnelCommand(&out, cfg, []string{"off"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Remote access is off. 1 remote session(s) signed out") {
		t.Errorf("output: %q", out.String())
	}
	db, _ = store.Open(cfg.DBPath())
	defer db.Close()
	if c, _ := db.TunnelConfig(ctx); c.Enabled {
		t.Error("still on")
	}
	if _, ok := db.SessionPerson(ctx, remote); ok {
		t.Error("remote session survived")
	}
	if _, ok := db.SessionPerson(ctx, lan); !ok {
		t.Error("LAN session was signed out")
	}
	out.Reset()
	_ = tunnelCommand(&out, cfg, []string{"status"})
	if !strings.Contains(out.String(), "Remote access is off") {
		t.Errorf("status: %q", out.String())
	}
}

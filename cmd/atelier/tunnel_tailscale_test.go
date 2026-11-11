//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"atelier/internal/backup"
	"atelier/internal/config"
	"atelier/internal/secrets"
	"atelier/internal/store"
	"atelier/internal/tunnel"
	"atelier/internal/tunnel/tunneltest"
)

const tsCmdKey = "tskey-auth-kTESTONLY11CNTRL-notARealKeyJustForTests0000"

func TestRotateKeyReSealsTailscaleAuthKey(t *testing.T) {
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
	c, _ := db.TunnelConfig(ctx)
	c.Provider, c.TSMode = store.TunnelTailscale, store.TailscaleFunnel
	if err := db.SaveTunnelConfig(ctx, kr, c, nil, "zoe"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTailscaleAuthKey(ctx, kr, tsCmdKey, "zoe"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out bytes.Buffer
	if err := rotateSecretKey(ctx, &out, dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Re-encrypted 1 secret(s)") || strings.Contains(out.String(), "kTESTONLY") {
		t.Errorf("rotate output: %s", out.String())
	}
	newKey, err := secrets.ReadKeyFile(secrets.KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	onlyNew, _ := secrets.NewKeyring(newKey)
	db, _ = store.Open(filepath.Join(dir, "atelier.db"))
	defer db.Close()
	if got, err := db.TailscaleAuthKey(ctx, onlyNew); err != nil || got != tsCmdKey {
		t.Fatalf("auth key with the new key alone: %v", err)
	}
	if _, err := db.TailscaleAuthKey(ctx, kr); err == nil {
		t.Error("the old key still opens the auth key: it wasn't re-sealed")
	}
	if c, _ := db.TunnelConfig(ctx); !c.HasTSKey || c.APIMode() != "tailscale-funnel" {
		t.Errorf("config after rotate = %+v", c)
	}
}

func alivePID(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestTunnelOffCommandTailscale(t *testing.T) {
	dir := t.TempDir()
	fakes := t.TempDir()
	bin, daemon, err := tunneltest.FakeTailscale(fakes)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: dir, TailscaleBin: bin, TailscaledBin: daemon}
	ctx := context.Background()
	db, _ := store.Open(cfg.DBPath())
	me, _ := db.CreateOwner(ctx, "zoe", "", "user")
	c, _ := db.TunnelConfig(ctx)
	c.Provider = store.TunnelTailscale
	if err := db.SaveTunnelConfig(ctx, nil, c, nil, "zoe"); err != nil {
		t.Fatal(err)
	}
	_ = db.TailscaleSignedIn(ctx, "atelier."+tunneltest.Tailnet)
	_, _ = db.SetTunnelEnabled(ctx, true, "zoe")
	remote, _ := db.CreateSessionFor(ctx, me.ID, time.Hour)
	_ = db.MarkRemoteSession(ctx, remote, me.ID, "atelier."+tunneltest.Tailnet, false)
	lan, _ := db.CreateSessionFor(ctx, me.ID, time.Hour)

	var out bytes.Buffer
	_ = tunnelCommand(&out, cfg, []string{"status"})
	if got := out.String(); !strings.Contains(got, "Remote access is on (Tailscale, private: your tailnet only, atelier."+tunneltest.Tailnet+")") {
		t.Errorf("status: %q", got)
	}

	// The helpers, as the server runs them (already signed in).
	for name, content := range map[string]string{"state": "Running", "hostname": "atelier", "caps": `"https":null`} {
		if err := os.WriteFile(filepath.Join(fakes, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The server's own connection, open for as long as its helpers run.
	serverDB, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverDB.Close() })
	enabled := func() bool { _, on, err := serverDB.TunnelGeneration(ctx); return err == nil && on }
	ts := &tunnel.Tailscale{Bin: bin, DaemonBin: daemon, StateDir: filepath.Join(dir, "tailscale"), PIDDir: dir,
		Poll: 20 * time.Millisecond, Backoff: func(int) time.Duration { return 20 * time.Millisecond }, Allow: enabled}
	t.Cleanup(ts.Stop)
	if err := ts.Start(tunnel.Settings{Mode: tunnel.ModeTailscalePrivate, Origin: "http://127.0.0.1:3001", Hostname: "atelier"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ts.Status().Phase != tunnel.PhaseConnected {
		if time.Now().After(deadline) {
			t.Fatalf("fake tailscale never connected: %+v", ts.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	pidOf := func(name string) int {
		b, _ := os.ReadFile(filepath.Join(fakes, name))
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	pids := []int{pidOf("daemon.pid"), pidOf("daemon.child"), pidOf("serve.child")}
	db.Close()

	out.Reset()
	if err := tunnelCommand(&out, cfg, []string{"off"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Remote access is off. 1 remote session(s) signed out; tailscale and tailscaled stopped.") {
		t.Errorf("output: %q", got)
	}
	deadline = time.Now().Add(5 * time.Second)
	for _, p := range pids {
		for alivePID(p) {
			if time.Now().After(deadline) {
				t.Fatalf("pid %d survived `tunnel off` (%v)", p, pids)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Nothing restarts them: the setting is off.
	time.Sleep(200 * time.Millisecond)
	if alivePID(pidOf("daemon.pid")) {
		t.Error("tailscaled was restarted after `tunnel off`")
	}
	db, _ = store.Open(cfg.DBPath())
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
	if !strings.Contains(out.String(), "Remote access is off (Tailscale, private") {
		t.Errorf("status: %q", out.String())
	}
	// Public mode, and a node that never signed in.
	c, _ = db.TunnelConfig(ctx)
	c.TSMode = store.TailscaleFunnel
	_ = db.SaveTunnelConfig(ctx, nil, c, nil, "zoe")
	_ = db.TailscaleForget(ctx, "zoe")
	db.Close()
	out.Reset()
	_ = tunnelCommand(&out, cfg, []string{"status"})
	if !strings.Contains(out.String(), "Remote access is off (Tailscale, public: Funnel, not signed in to a tailnet yet).") {
		t.Errorf("status: %q", out.String())
	}
}

// A full restore leaves remote access off with Tailscale chosen too, and
// the node's identity (<data>/tailscale) is never in a snapshot.
func TestRestoreKeepsTailscaleOffAndStateOutOfBackups(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ATELIER_DATA", dir)
	t.Setenv("ATELIER_BACKUP_DIR", filepath.Join(t.TempDir(), "backups"))
	cfg := config.FromEnv()
	if cfg.TailscaleBin != "tailscale" || cfg.TailscaledBin != "tailscaled" {
		t.Errorf("default binaries = %q, %q", cfg.TailscaleBin, cfg.TailscaledBin)
	}
	ctx := context.Background()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOwner(ctx, "zoe", "", "user"); err != nil {
		t.Fatal(err)
	}
	c, _ := db.TunnelConfig(ctx)
	c.Provider, c.TSMode = store.TunnelTailscale, store.TailscaleFunnel
	if err := db.SaveTunnelConfig(ctx, nil, c, nil, "zoe"); err != nil {
		t.Fatal(err)
	}
	_ = db.TailscaleSignedIn(ctx, "atelier."+tunneltest.Tailnet)
	if _, err := db.SetTunnelEnabled(ctx, true, "zoe"); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "tailscale")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte(`{"_machinekey":"privkey:NODE-IDENTITY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range backup.DataDirs() {
		if strings.HasPrefix(d, "tailscale") {
			t.Fatalf("%q is registered for backups", d)
		}
	}
	outc := backup.Run(ctx, backup.PlanFromConfig(cfg, db, "git"))
	db.Close()
	if outc.Err != nil {
		t.Fatal(outc.Err)
	}
	for _, f := range outc.Result.Manifest.Files {
		if strings.Contains(f.Path, "tailscale") {
			t.Errorf("snapshot contains %s", f.Path)
		}
	}
	if b, err := os.ReadFile(outc.Result.Path); err == nil && bytes.Contains(b, []byte("NODE-IDENTITY")) {
		t.Error("the node's state is readable in the snapshot")
	}
	if err := restoreCmd([]string{outc.Result.Path, "--force"}); err != nil {
		t.Fatal(err)
	}
	db, _ = store.Open(cfg.DBPath())
	defer db.Close()
	c, err = db.TunnelConfig(ctx)
	if err != nil || c.Enabled {
		t.Fatalf("after restoring a snapshot taken while on: %+v, %v", c, err)
	}
	if !c.Tailscale() || c.APIMode() != "tailscale-funnel" {
		t.Errorf("the provider choice wasn't restored: %+v", c)
	}
	// Restoring in place leaves the node's identity where it was, so the
	// same machine needs no new auth key.
	if b, err := os.ReadFile(filepath.Join(state, "tailscaled.state")); err != nil || !bytes.Contains(b, []byte("NODE-IDENTITY")) {
		t.Errorf("an in-place restore lost the Tailscale state: %v", err)
	}
}

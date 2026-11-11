package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"atelier/internal/config"
	"atelier/internal/store"
	"atelier/internal/tunnel"
)

func init() {
	commands["tunnel"] = func(args []string) error { return tunnelCommand(os.Stdout, config.FromEnv(), args) }
}

const tunnelUsage = `usage: atelier tunnel off|status

  off      turn remote access off now: kill cloudflared, sign out remote sessions
  status   say whether remote access is on

Turning it on is done in Settings → Remote access.`

// tunnelCommand is the shell's kill switch, e.g.
//
//	docker exec atelier atelier tunnel off
//
// It writes the setting (which also revokes every remote session) and kills
// the cloudflared process recorded in <data>/tunnel.pid. The running server
// checks the setting every second and closes its remote listener.
func tunnelCommand(out io.Writer, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New(tunnelUsage)
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "off":
		n, err := db.SetTunnelEnabled(ctx, false, "shell")
		killed, kerr := tunnel.KillFromPIDFile(filepath.Join(cfg.DataDir, "tunnel.pid"), "cloudflared")
		if err != nil {
			return fmt.Errorf("turn off (cloudflared killed: %t): %w", killed, err)
		}
		fmt.Fprintf(out, "Remote access is off. %d remote session(s) signed out", n)
		if killed {
			fmt.Fprint(out, "; cloudflared stopped")
		}
		fmt.Fprintln(out, ".")
		if kerr != nil {
			fmt.Fprintln(out, "Couldn't check for a running cloudflared:", kerr)
		}
		return nil
	case "status":
		c, err := db.TunnelConfig(ctx)
		if err != nil {
			return err
		}
		state := "off"
		if c.Enabled {
			state = "on"
		}
		fmt.Fprintf(out, "Remote access is %s (%s tunnel", state, c.Mode)
		if c.Mode == tunnel.ModeNamed && c.Hostname != "" {
			fmt.Fprintf(out, ", %s", c.Hostname)
		}
		fmt.Fprintln(out, ").")
		return nil
	}
	return fmt.Errorf("unknown tunnel command %q\n%s", args[0], tunnelUsage)
}

// tunnelOffAfterRestore keeps remote access off after a full restore, even
// if the snapshot was taken while it was on.
func tunnelOffAfterRestore(ctx context.Context, db *store.Store) error {
	return db.TunnelForceOff(ctx, "restore")
}

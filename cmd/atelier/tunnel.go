package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"atelier/internal/config"
	"atelier/internal/store"
	"atelier/internal/tunnel"
)

func init() {
	commands["tunnel"] = func(args []string) error { return tunnelCommand(os.Stdout, config.FromEnv(), args) }
}

const tunnelUsage = `usage: atelier tunnel off|status

  off      turn remote access off now: kill cloudflared or Tailscale
           (tailscaled and tailscale serve/funnel), sign out remote sessions
  status   say whether remote access is on, and through which provider and mode

Turning it on is done in Settings → Remote access.`

// tunnelCommand is the shell's kill switch, e.g.
//
//	docker exec atelier atelier tunnel off
//
// It writes the setting (which also revokes every remote session) and kills
// the helpers recorded in the data directory's PID files: cloudflared
// (tunnel.pid), or tailscale serve/funnel and tailscaled. The running server
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
		// Whatever the setting says the provider is, kill every helper there
		// is a PID file for: serve/funnel first, so nothing is forwarded
		// while tailscaled goes down.
		var stopped []string
		var kerrs []error
		for _, h := range []struct{ file, name, bin string }{
			{"tunnel.pid", "cloudflared", cfg.TunnelBin},
			{tunnel.TailscaleServePIDFile, "tailscale", cfg.TailscaleBin},
			{tunnel.TailscaledPIDFile, "tailscaled", cfg.TailscaledBin},
		} {
			want := h.bin
			if want == "" {
				want = h.name
			}
			killed, kerr := tunnel.KillFromPIDFile(filepath.Join(cfg.DataDir, h.file), want)
			if killed {
				stopped = append(stopped, h.name)
			}
			if kerr != nil {
				kerrs = append(kerrs, fmt.Errorf("%s: %w", h.name, kerr))
			}
		}
		if err != nil {
			return fmt.Errorf("turn off (helpers killed: %s): %w", strings.Join(stopped, ", "), err)
		}
		fmt.Fprintf(out, "Remote access is off. %d remote session(s) signed out", n)
		if len(stopped) > 0 {
			fmt.Fprintf(out, "; %s stopped", strings.Join(stopped, " and "))
		}
		fmt.Fprintln(out, ".")
		for _, kerr := range kerrs {
			fmt.Fprintln(out, "Couldn't check for a running helper:", kerr)
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
		switch {
		case c.Tailscale():
			mode := "private: your tailnet only"
			if !c.TailnetOnly() {
				mode = "public: Funnel"
			}
			fmt.Fprintf(out, "Remote access is %s (Tailscale, %s", state, mode)
			if c.TSNode != "" {
				fmt.Fprintf(out, ", %s", c.TSNode)
			} else {
				fmt.Fprint(out, ", not signed in to a tailnet yet")
			}
		default:
			fmt.Fprintf(out, "Remote access is %s (Cloudflare, %s tunnel", state, c.Mode)
			if c.Mode == tunnel.ModeNamed && c.Hostname != "" {
				fmt.Fprintf(out, ", %s", c.Hostname)
			}
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

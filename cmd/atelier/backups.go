package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"atelier/internal/backup"
	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func init() {
	commands["restore"] = restoreCmd
	commands["backup"] = backupCmd
}

// restoreCmd restores a snapshot:
//
//	atelier restore <snapshot.tar.gz>                  everything (server must be stopped)
//	atelier restore <snapshot.tar.gz> --repo owner/name  one repository (server may run)
func restoreCmd(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	repo := fs.String("repo", "", "restore only this repository (owner/name)")
	force := fs.Bool("force", false, "restore everything even if the server seems to be running")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: atelier restore <snapshot.tar.gz> [--repo owner/name] [--force]")
		fs.PrintDefaults()
	}
	// Allow the archive before or after the flags.
	var archive string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		archive, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if archive == "" && fs.NArg() > 0 {
		archive = fs.Arg(0)
	}
	if archive == "" {
		fs.Usage()
		return errors.New("which snapshot? pass the path to an atelier-*.tar.gz file")
	}
	if _, err := os.Stat(archive); err != nil {
		return err
	}

	cfg := config.FromEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	gs, err := gitrepo.New(cfg.RepoRoot())
	if err != nil {
		return err
	}
	o := backup.RestoreOptions{
		Archive: archive, DataDir: cfg.DataDir, DBPath: cfg.DBPath(), RepoRoot: cfg.RepoRoot(),
		Git: gs.Git, Force: *force,
		AfterRepo: func(ctx context.Context, owner, name string) error { return gs.AfterCreate(ctx, owner, name) },
		Log:       func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	}

	if *repo != "" {
		db, err := store.Open(cfg.DBPath())
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer db.Close()
		if err := backup.RestoreRepo(ctx, o, db, *repo); err != nil {
			return err
		}
		fmt.Println("Done.")
		return nil
	}

	m, err := backup.RestoreAll(ctx, o)
	if err != nil {
		return err
	}
	// Run migrations so the restored database matches this version's schema.
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open restored database: %w", err)
	}
	db.Close()
	fmt.Printf("Restored %d repositories and the database from a snapshot taken %s. Start Atelier again.\n",
		len(m.Repos), m.Created.Local().Format("2 Jan 2006 15:04"))
	return nil
}

// backupCmd takes a snapshot from the shell, using the same settings as the
// server (handy before upgrades): atelier backup
func backupCmd(args []string) error {
	cfg := config.FromEnv()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	gs, err := gitrepo.New(cfg.RepoRoot())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	out := backup.Run(ctx, backup.PlanFromConfig(cfg, db, gs.Git))
	_, _ = db.AddBackupRun(context.WithoutCancel(ctx), out.Record("manual", "owner", "shell"))
	fmt.Println(out.Summary())
	if out.Result.Name != "" {
		fmt.Println(out.Result.Path)
	}
	return out.Err
}

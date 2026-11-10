package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"atelier/internal/config"
	"atelier/internal/packages"
	"atelier/internal/store"
	"atelier/internal/web"
)

func init() {
	commands["packages"] = packagesCmd
}

// packagesCmd: atelier packages gc — remove package files nothing refers to.
// Safe while the server runs: files written in the last hour are kept.
func packagesCmd(args []string) error {
	if len(args) == 0 || args[0] != "gc" {
		fmt.Fprintln(os.Stderr, "usage: atelier packages gc")
		return errors.New("unknown packages command")
	}
	cfg := config.FromEnv()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	b, err := packages.New(filepath.Join(cfg.DataDir, "packages"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := web.PackagesGC(ctx, db, b)
	if err != nil {
		return err
	}
	fmt.Println("Packages:", res)
	return nil
}

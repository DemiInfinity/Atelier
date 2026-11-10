// Command atelier runs the Atelier server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
	"atelier/internal/web"
)

// commands are subcommands like `atelier reset-password`. Each lives in its own
// file and registers itself from init(): commands["name"] = func(args []string) error {…}
var commands = map[string]func(args []string) error{}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("atelier", version)
		return
	}
	if name := flag.Arg(0); name != "" {
		cmd, ok := commands[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "atelier: unknown command %q\n", name)
			os.Exit(2)
		}
		if err := cmd(flag.Args()[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "atelier:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg := config.FromEnv()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	repos, err := gitrepo.New(cfg.RepoRoot())
	if err != nil {
		return fmt.Errorf("repositories: %w", err)
	}
	app, err := web.New(cfg, db, repos)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: app,
		// Only the header read is bounded: git pushes and clones stream request
		// and response bodies for as long as they need, so no Read/WriteTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	public := cfg.PublicURL
	if public == "" {
		public = "(derived from each request)"
	}
	log.Printf("atelier %s listening on %s (data %s, public URL %s, GitHub sign-in %s, trust proxy %t)",
		version, cfg.Listen, cfg.DataDir, public, onOff(cfg.GitHubEnabled()), cfg.TrustProxy)

	app.Start(ctx) // background work registered by features (backups, SSH, …)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	stop() // a second signal kills the process immediately

	log.Print("shutting down, waiting up to 30s for requests to finish")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
		_ = srv.Close()
	}
	log.Print("stopped")
	return nil
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

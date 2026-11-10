package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"atelier/internal/config"
	"atelier/internal/secrets"
	"atelier/internal/store"
)

func init() {
	commands["secrets"] = func(args []string) error {
		return secretsCommand(os.Stdout, config.FromEnv().DataDir, args)
	}
}

const secretsUsage = `usage: atelier secrets rotate-key

  rotate-key   generate a new encryption key and re-encrypt every Actions secret with it`

func secretsCommand(out io.Writer, dataDir string, args []string) error {
	if len(args) == 0 {
		return errors.New(secretsUsage)
	}
	switch args[0] {
	case "rotate-key":
		fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
		fs.SetOutput(out)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return rotateSecretKey(context.Background(), out, dataDir)
	}
	return fmt.Errorf("unknown secrets command %q\n%s", args[0], secretsUsage)
}

// rotateSecretKey re-encrypts every secret with a new key.
//
//  1. The new key is written to secret.key.next (or, if that file is left over
//     from an interrupted rotation, reused).
//  2. Every secret is re-encrypted in one database transaction.
//  3. With a key file: secret.key moves to secret.key.old and secret.key.next
//     becomes secret.key. With ATELIER_SECRET_KEY: the new key is printed for
//     you to put in the environment; secret.key.next keeps things readable
//     until then.
//
// Atelier reads secret.key.next and secret.key.old too, so a crash at any
// step leaves every secret readable; run the command again to finish.
func rotateSecretKey(ctx context.Context, out io.Writer, dataDir string) error {
	db, err := store.Open(filepath.Join(dataDir, "atelier.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	n, err := db.SecretCount(ctx)
	if err != nil {
		return err
	}
	current, src, err := secrets.Load(dataDir, n == 0)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("there are secrets in the database but no key: set ATELIER_SECRET_KEY or restore secret.key first")
	}
	if err != nil {
		return err
	}

	nextPath := filepath.Join(dataDir, secrets.NextKeyFile)
	newKey, err := secrets.ReadKeyFile(nextPath)
	switch {
	case err == nil:
		fmt.Fprintln(out, "Finishing an earlier rotation with", secrets.NextKeyFile+".")
	case errors.Is(err, os.ErrNotExist):
		newKey = secrets.GenerateKey()
		if err := secrets.WriteKeyFile(nextPath, newKey); err != nil {
			return fmt.Errorf("write new key: %w", err)
		}
	default:
		return err
	}
	to, err := secrets.NewKeyring(newKey)
	if err != nil {
		return err
	}

	count, err := db.RotateSecrets(ctx, current, to)
	if err != nil {
		return fmt.Errorf("re-encrypt secrets (nothing was changed): %w", err)
	}
	fmt.Fprintf(out, "Re-encrypted %d secret(s) with key %s.\n", count, to.PrimaryID())

	if src == secrets.FromEnv {
		fmt.Fprintf(out, "\nATELIER_SECRET_KEY is set, so update it to the new key and restart Atelier:\n\n  ATELIER_SECRET_KEY=%s\n\n", secrets.EncodeKey(newKey))
		fmt.Fprintf(out, "Until then Atelier reads the new key from %s. Delete that file once the environment is updated.\n", nextPath)
		return nil
	}
	keyPath := secrets.KeyPath(dataDir)
	oldPath := filepath.Join(dataDir, secrets.OldKeyFile)
	if _, err := os.Stat(keyPath); err == nil {
		if err := os.Rename(keyPath, oldPath); err != nil {
			return fmt.Errorf("keep old key: %w", err)
		}
	}
	if err := os.Rename(nextPath, keyPath); err != nil {
		return fmt.Errorf("install new key: %w", err)
	}
	if d, err := os.Open(dataDir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	fmt.Fprintf(out, "The new key is in %s. Back it up, and restart Atelier.\n", keyPath)
	fmt.Fprintf(out, "The previous key is in %s: backups taken before now need it. Delete it once you no longer keep those.\n", oldPath)
	return nil
}

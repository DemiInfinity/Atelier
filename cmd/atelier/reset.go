package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"atelier/internal/config"
	"atelier/internal/store"
)

const minPassword = 10

func init() { commands["reset-password"] = func([]string) error { return resetPassword() } }

// resetPassword sets a new owner password from the server's own shell, e.g.
//
//	docker exec -it atelier atelier reset-password
//
// It needs access to the data directory, so it can't be used over the web.
// Every signed-in browser is signed out afterwards.
func resetPassword() error {
	cfg := config.FromEnv()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	ctx := context.Background()

	owner, err := db.PersonalOwner(ctx)
	if err != nil {
		return errors.New("no owner account yet: open Atelier in a browser to run first-time setup")
	}
	fmt.Printf("Resetting the password for %s.\n", owner.Name)

	pw, err := readPassword("New password: ")
	if err != nil {
		return err
	}
	if len(pw) < minPassword {
		return fmt.Errorf("use at least %d characters", minPassword)
	}
	if len(pw) > 72 {
		return errors.New("use at most 72 characters")
	}
	again, err := readPassword("Repeat it: ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("the two passwords don't match; nothing was changed")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := db.SetSetting(ctx, store.SettingPasswordHash, string(hash)); err != nil {
		return err
	}
	if err := db.DeleteAllSessions(ctx); err != nil {
		return err
	}
	fmt.Println("Password updated. All browsers have been signed out; sign in again with the new password.")
	return nil
}

var stdin = bufio.NewReader(os.Stdin)

// readPassword reads without echo on a terminal, or a plain line otherwise.
func readPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

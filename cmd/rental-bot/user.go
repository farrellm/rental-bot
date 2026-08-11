package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// minPasswordLen is the shortest password this will accept.
//
// Twelve rather than eight because this system is designed to be reachable
// from the internet and TOTP is still deferred (docs/DESIGN.md §7.1): the
// password is the only thing between a stranger and the ledger. argon2id makes
// an offline attack expensive, but it cannot make a short password long.
const minPasswordLen = 12

// passwordEnv supplies the password when stdin is not a terminal, which is how
// this runs from a provisioning script.
const passwordEnv = "RENTAL_BOT_ADMIN_PASSWORD"

// createUser creates the named user or resets an existing one's password.
//
// This is the only way a user is ever created. There is no registration
// endpoint and no first-run setup screen, so an instance that is reachable
// before its operator has finished setting it up cannot be claimed by whoever
// finds it first.
func createUser(ctx context.Context, db *store.DB, username string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("create-user: the username is empty")
	}
	if strings.ContainsAny(username, " \t\r\n") {
		return errors.New("create-user: the username cannot contain whitespace")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(password) < minPasswordLen {
		return fmt.Errorf("create-user: the password is shorter than %d characters", minPasswordLen)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("create-user: %w", err)
	}

	repo := db.Repo()
	now := domain.Stamp(time.Now())

	existing, err := repo.Read().GetUserByUsername(ctx, username)
	reset := err == nil
	if err != nil && !store.NotFound(err) {
		return fmt.Errorf("create-user: %w", err)
	}

	user, err := repo.Write().UpsertUser(ctx, sqlc.UpsertUserParams{
		Username:     username,
		Email:        existing.Email,
		PasswordHash: hash,
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		return fmt.Errorf("create-user: %w", err)
	}

	if reset {
		// A password change ends every session: if the reason for the change
		// is that someone else had the old one, leaving their session alive
		// would make the change pointless.
		if err := auth.NewSessions(repo).RevokeAll(ctx, user.ID); err != nil {
			return fmt.Errorf("create-user: %w", err)
		}
		fmt.Printf("reset the password for %s (id %d); existing sessions ended\n", user.Username, user.ID)
		return nil
	}

	fmt.Printf("created user %s (id %d)\n", user.Username, user.ID)
	return nil
}

// readPassword takes the password from the environment when stdin is not a
// terminal, and otherwise prompts for it twice with the echo off.
func readPassword() (string, error) {
	if v, ok := os.LookupEnv(passwordEnv); ok {
		return v, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Reading a password from a pipe without being asked to would put it
		// in the shell history of whoever piped it.
		return "", fmt.Errorf("create-user: stdin is not a terminal; set %s instead", passwordEnv)
	}

	fmt.Print("Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("create-user: read password: %w", err)
	}

	fmt.Print("Confirm:  ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("create-user: read password: %w", err)
	}

	if string(first) != string(second) {
		return "", errors.New("create-user: the passwords do not match")
	}
	return string(first), nil
}

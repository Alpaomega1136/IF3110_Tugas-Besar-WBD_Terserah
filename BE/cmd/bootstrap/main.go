// Bootstrap creates the first admin without a default password or HTTP bypass.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/config"
	"terserah/backend/internal/database"
)

func main() {
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	username := strings.TrimSpace(os.Getenv("BOOTSTRAP_USERNAME"))
	if username == "" || len(username) > 100 {
		return errors.New("BOOTSTRAP_USERNAME must contain 1 to 100 bytes")
	}
	hash, err := auth.HashPassword(os.Getenv("BOOTSTRAP_PASSWORD"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to metadata database")
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		return errors.New("cannot start bootstrap")
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(3110002)"); err != nil {
		return errors.New("cannot lock bootstrap")
	}
	var exists bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE is_platform_admin=true)").Scan(&exists); err != nil {
		return errors.New("cannot inspect bootstrap state; apply metadata migrations first")
	}
	if exists {
		return errors.New("an admin already exists; bootstrap does not replace accounts")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO users(username,password_hash,is_platform_admin) VALUES($1,$2,true)", username, hash); err != nil {
		return errors.New("cannot create bootstrap admin")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("cannot commit bootstrap admin")
	}
	slog.Info("bootstrap admin created")
	return nil
}

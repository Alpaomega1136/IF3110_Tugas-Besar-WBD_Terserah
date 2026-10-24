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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to metadata database")
	}
	defer db.Close()
	if err = auth.BootstrapAdmin(ctx, db, username, os.Getenv("BOOTSTRAP_PASSWORD")); err != nil {
		return err
	}
	slog.Info("bootstrap admin created")
	return nil
}

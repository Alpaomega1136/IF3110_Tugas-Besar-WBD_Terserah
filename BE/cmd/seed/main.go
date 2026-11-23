// Seed retains the BE-01 CLI variables but only initializes the first admin.
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/config"
	"terserah/backend/internal/database"
	"terserah/backend/migrations"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if !run() {
		os.Exit(1)
	}
}

func run() bool {
	cfg, err := config.Load()
	if err != nil {
		slog.Error(err.Error())
		return false
	}
	username := strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("ADMIN_PASSWORD")
	if len(password) < 12 || len(password) > 1024 {
		slog.Error("ADMIN_PASSWORD must be between 12 and 1024 bytes")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("cannot connect to metadata database")
		return false
	}
	defer pool.Close()

	if err := migrations.Apply(ctx, pool); err != nil {
		slog.Error("metadata migration failed before seed")
		return false
	}

	if err := auth.BootstrapAdmin(ctx, pool, username, password); err != nil {
		slog.Error(err.Error())
		return false
	}
	slog.Info("bootstrap admin created")
	return true
}

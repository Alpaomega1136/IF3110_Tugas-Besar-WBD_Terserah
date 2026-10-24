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
	if err := auth.ValidatePassword(password); err != nil {
		slog.Error("ADMIN_PASSWORD must be between 8 and 1024 bytes")
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

	svc := auth.NewService(auth.NewPostgresStore(pool), auth.Config{})
	user, err := svc.BootstrapAdmin(ctx, username, password)
	if err != nil {
		slog.Error("cannot bootstrap platform admin")
		return false
	}
	slog.Info("platform admin bootstrapped", "id", user.ID, "username", user.Username)
	return true
}

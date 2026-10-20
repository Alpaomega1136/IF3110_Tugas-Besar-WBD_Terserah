package main

import (
	"context"
	"log/slog"
	"os"
	"time"

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("cannot connect to metadata database")
		return false
	}
	defer pool.Close()
	if err = migrations.Apply(ctx, pool); err != nil {
		slog.Error("metadata migration failed; inspect schema and database availability")
		return false
	}
	slog.Info("metadata migrations applied")
	return true
}

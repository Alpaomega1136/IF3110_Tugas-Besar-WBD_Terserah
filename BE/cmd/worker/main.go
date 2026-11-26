package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"terserah/backend/internal/config"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/metrics"
	"terserah/backend/internal/runner"
	"terserah/backend/internal/telemetry"
	"terserah/backend/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown, err := telemetry.Init(ctx, "terserah-worker")
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(cleanup)
	}()
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := database.Open(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to metadata database; check configuration and availability")
	}
	trackedPool := metrics.TrackPool(pool, "metadata")
	defer trackedPool.Close()
	stopMetrics, err := metrics.Listen("127.0.0.1:9465", nil)
	if err != nil {
		return err
	}
	defer stopMetrics()
	targets, err := connections.New(pool, cfg.SecretKey)
	if err != nil {
		return errors.New("cannot initialize target connections")
	}
	background, err := worker.New(jobs.New(pool), runner.New(pool, targets, cfg.SecretKey), worker.Options{})
	if err != nil {
		return err
	}
	slog.Info("Worker started")
	background.Run(ctx)
	return nil
}

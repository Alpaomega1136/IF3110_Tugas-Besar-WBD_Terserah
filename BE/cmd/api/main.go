package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"terserah/backend/internal/admin"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/config"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/database"
	"terserah/backend/internal/httpapi"
	"terserah/backend/internal/runner"
	"terserah/backend/internal/viewer"
)

func main() {
	if err := run(); err != nil {
		// Do not log driver/config errors: they may contain connection credentials.
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := database.Open(connectCtx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to metadata database; check configuration and availability")
	}
	defer pool.Close()
	sessions, err := auth.New(pool, auth.Options{Origin: cfg.AppOrigin, Key: cfg.SecretKey})
	if err != nil {
		return errors.New("cannot initialize sessions")
	}
	targets, err := connections.New(pool, cfg.SecretKey)
	if err != nil {
		return errors.New("cannot initialize target connections")
	}
	handler := httpapi.New(pool, sessions.Register, func(mux *http.ServeMux) {
		targets.Register(mux, sessions)
		viewer.New(targets).Register(mux, sessions)
		runner.New(pool, targets, cfg.SecretKey).Register(mux, sessions)
		admin.NewService(admin.NewPostgresStore(pool), nil).Register(mux, sessions)
	})
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("API started", "address", cfg.HTTPAddr)
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed")
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.New("HTTP server shutdown failed")
		}
	}
	return nil
}

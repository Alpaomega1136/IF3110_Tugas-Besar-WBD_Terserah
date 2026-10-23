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

	"terserah/backend/internal/auth"
	"terserah/backend/internal/config"
	"terserah/backend/internal/database"
	"terserah/backend/internal/httpapi"
)

func main() {
	if err := run(); err != nil {
		// Jangan log detail error driver/config karena dapat memuat kredensial koneksi.
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
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
	handler := httpapi.NewWithOptions(pool, httpapi.Options{
		AuthConfig: auth.Config{CookieSecure: cfg.CookieSecure},
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

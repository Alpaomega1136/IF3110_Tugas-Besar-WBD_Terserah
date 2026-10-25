// Bootstrap creates the first admin without a default password or HTTP bypass.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/config"
	"terserah/backend/internal/database"
)

func main() {
	check := flag.Bool("check", false, "print whether a platform admin already exists")
	flag.Parse()
	if err := run(*check); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(check bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	username := strings.TrimSpace(os.Getenv("BOOTSTRAP_USERNAME"))
	if !check && (username == "" || len(username) > 100) {
		return errors.New("BOOTSTRAP_USERNAME must contain 1 to 100 bytes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to metadata database")
	}
	defer db.Close()
	if check {
		var exists bool
		if err = db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE is_platform_admin=true)").Scan(&exists); err != nil {
			return errors.New("cannot inspect bootstrap state; apply metadata migrations first")
		}
		fmt.Println(exists)
		return nil
	}
	if err = auth.BootstrapAdmin(ctx, db, username, os.Getenv("BOOTSTRAP_PASSWORD")); err != nil {
		return err
	}
	slog.Info("bootstrap admin created")
	return nil
}

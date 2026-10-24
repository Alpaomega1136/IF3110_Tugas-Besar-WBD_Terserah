package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BootstrapAdmin is shared by the current bootstrap and legacy seed CLIs.
// It never resets existing users or creates another admin after initialization.
func BootstrapAdmin(ctx context.Context, db *pgxpool.Pool, username, password string) error {
	username, err := ValidateUsername(username)
	if err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
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
	return nil
}

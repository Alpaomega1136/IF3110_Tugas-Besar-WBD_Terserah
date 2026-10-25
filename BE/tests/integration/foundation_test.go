//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/database"
	"terserah/backend/internal/httpapi"
	"terserah/backend/migrations"
)

func TestFoundation(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL must point to a dedicated test PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal("cannot connect to test PostgreSQL")
	}
	t.Cleanup(admin.Close)
	// Isolated schema: never truncate application tables or drop a database.
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open test pool")
	}
	defer pool.Close()
	var firstCount int
	for attempt := 0; attempt < 2; attempt++ {
		if err := migrations.Apply(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM platform_schema_migrations").Scan(&firstCount); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM platform_schema_migrations").Scan(&count); err != nil || count < 1 || count != firstCount {
		t.Fatalf("migration idempotency: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO groups(name) VALUES ('developers')"); err != nil {
		t.Fatal(err)
	}
	var targetID int64
	if err := pool.QueryRow(ctx, "INSERT INTO database_connections(name,host,database_name,username,password_encrypted) VALUES('fixture','localhost','fixture','test',$1) RETURNING id", []byte("fixture-only")).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES(1,$1,'owner')", targetID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatal("invalid access level was not rejected by its check constraint")
	}
	_, err = pool.Exec(ctx, "INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES(1,999,'view')")
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatal("invalid target was not rejected by its foreign key")
	}
	rr := httptest.NewRecorder()
	httpapi.New(pool).ServeHTTP(rr, httptest.NewRequest("GET", "/health/ready", nil))
	if rr.Code != 200 {
		t.Fatalf("readiness status=%d", rr.Code)
	}
	pool.Close()
	rr = httptest.NewRecorder()
	httpapi.New(pool).ServeHTTP(rr, httptest.NewRequest("GET", "/health/ready", nil))
	if rr.Code != 503 {
		t.Fatalf("closed database readiness status=%d", rr.Code)
	}
}

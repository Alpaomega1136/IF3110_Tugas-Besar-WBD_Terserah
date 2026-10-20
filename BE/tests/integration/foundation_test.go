//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	for range 2 {
		if err := migrations.Apply(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM platform_schema_migrations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration idempotency: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO groups(name) VALUES ('developers')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES (1,999,'owner')"); err == nil {
		t.Fatal("invalid grant accepted")
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

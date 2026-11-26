package metrics

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerMetricsHandler(t *testing.T) {
	h := Handler(nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "version=0.0.4") {
		t.Fatalf("unexpected response: %d", w.Code)
	}
	for _, name := range []string{"terserah_migration_queue_wait_seconds_bucket", "terserah_migration_duration_seconds_count", "terserah_db_pool_connections", "terserah_db_pool_wait_seconds_total"} {
		if !strings.Contains(w.Body.String(), name) {
			t.Errorf("missing %s", name)
		}
	}
	if strings.Contains(w.Body.String(), "terserah_migration_jobs{") {
		t.Fatal("worker must not export shared job counts")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestPoolLifecycleWithoutDatabase(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://test:test@localhost:5432/test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	raw, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := TrackPool(raw, "target")
	t.Cleanup(p.Close)
	var b bytes.Buffer
	writePools(&b)
	if !strings.Contains(b.String(), `terserah_db_pools{role="target"} 1`+"\n") {
		t.Fatal("pool not registered")
	}
	release := TrackHijacked()
	b.Reset()
	writePools(&b)
	if !strings.Contains(b.String(), "terserah_db_target_hijacked_connections 1\n") {
		t.Error("guard not tracked")
	}
	release()
	p.Close()
	p.Close()
	b.Reset()
	writePools(&b)
	if !strings.Contains(b.String(), `terserah_db_pools{role="target"} 0`+"\n") || !strings.Contains(b.String(), "terserah_db_target_hijacked_connections 0\n") {
		t.Fatal("pool or guard leaked")
	}
}

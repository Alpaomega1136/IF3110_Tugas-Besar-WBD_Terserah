package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var statuses = [...]string{"queued", "running", "success", "failed", "unknown", "partial", "recovering", "retry_wait", "resumed", "partially_reverted", "other"}

func writeJobs(ctx context.Context, b *bytes.Buffer, db *pgxpool.Pool) error {
	counts := make(map[string]int64, len(statuses))
	rows, err := db.Query(ctx, `SELECT status, count(*) FROM migration_jobs GROUP BY status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return err
		}
		known := false
		for _, s := range statuses {
			if s == status {
				known = true
				break
			}
		}
		if !known {
			status = "other"
		}
		counts[status] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var retries int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM migration_job_attempts WHERE attempt_no > 1`).Scan(&retries); err != nil {
		return err
	}
	header(b, "terserah_migration_jobs", "gauge", "Persisted migration jobs by current status, including terminal jobs.")
	for _, s := range statuses {
		fmt.Fprintf(b, "terserah_migration_jobs{status=%q} %d\n", s, counts[s])
	}
	header(b, "terserah_migration_retry_attempts", "gauge", "Persisted attempts after the first attempt of the same job.")
	fmt.Fprintf(b, "terserah_migration_retry_attempts %d\n", retries)
	return nil
}

func Handler(db *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		if db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := writeJobs(ctx, &b, db); err != nil {
				http.Error(w, "metrics database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		writeActivity(&b)
		writePools(&b)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b.Bytes())
	})
	return mux
}

func Listen(defaultAddr string, db *pgxpool.Pool) (func(), error) {
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, errors.New("cannot start metrics listener; check METRICS_ADDR and port availability")
	}
	srv := &http.Server{Handler: Handler(db), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("metrics_listener_failed")
		}
	}()
	slog.Info("metrics_listener_started", "address", ln.Addr().String())
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
		<-done
	}, nil
}

//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/database"
	"terserah/backend/internal/runner"
)

// The fixture relays plaintext PostgreSQL frames and cuts exactly one COMMIT
// either before forwarding it or after the server confirms durable success.
func commitProxy(t *testing.T, address string, fault, after bool, commitNumber ...int) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cutAt := int32(1)
	if len(commitNumber) > 0 {
		cutAt = int32(commitNumber[0])
	}
	var observed atomic.Int32
	var once atomic.Bool
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
		if fault && !once.Load() {
			t.Error("COMMIT fault was not triggered")
		}
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			backend, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				_ = client.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, client, backend)
			mu.Unlock()
			wg.Add(2)
			go func() {
				defer wg.Done()
				defer client.Close()
				defer backend.Close()
				reader := bufio.NewReader(client)
				var size [4]byte
				if _, err := io.ReadFull(reader, size[:]); err != nil {
					return
				}
				n := binary.BigEndian.Uint32(size[:])
				if n < 4 || n > 1024*1024 {
					return
				}
				startup := make([]byte, n)
				copy(startup, size[:])
				if _, err := io.ReadFull(reader, startup[4:]); err != nil {
					return
				}
				if _, err := backend.Write(startup); err != nil {
					return
				}
				for {
					kind, payload, err := pgFrame(reader)
					if err != nil {
						return
					}
					if fault && !after && kind == 'Q' && strings.EqualFold(string(payload), "COMMIT\x00") && observed.Add(1) == cutAt && once.CompareAndSwap(false, true) {
						return
					}
					if err = writeFrame(backend, kind, payload); err != nil {
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				defer client.Close()
				defer backend.Close()
				reader := bufio.NewReader(backend)
				for {
					kind, payload, err := pgFrame(reader)
					if err != nil {
						return
					}
					if fault && after && kind == 'C' && string(payload) == "COMMIT\x00" && observed.Add(1) == cutAt && once.CompareAndSwap(false, true) {
						return
					}
					if err = writeFrame(client, kind, payload); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func pgFrame(r *bufio.Reader) (byte, []byte, error) {
	kind, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var size [4]byte
	if _, err = io.ReadFull(r, size[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n < 4 || n > 1024*1024 {
		return 0, nil, fmt.Errorf("invalid test protocol frame")
	}
	payload := make([]byte, n-4)
	_, err = io.ReadFull(r, payload)
	return kind, payload, err
}
func writeFrame(w io.Writer, kind byte, payload []byte) error {
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(4+len(payload)))
	copy(frame[5:], payload)
	_, err := w.Write(frame)
	return err
}

func TestCommitResponseRecovery(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_commit_%v", after), func(t *testing.T) {
			a := newM1App(t)
			admin := a.browser()
			admin.login(t, "admin")
			input := targetInput(t, "TEST_TARGET_DATABASE_URL", "commit-fault")
			input.Port = commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, after)
			target := createTarget(t, admin, input)
			grant(t, a, "editor", target.ID, "edit")
			editor := a.browser()
			editor.login(t, "editor")
			base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
			db, err := database.Open(context.Background(), os.Getenv("TEST_TARGET_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			schema := fmt.Sprintf("fault_%d", time.Now().UnixNano())
			defer func() {
				if _, err := db.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
					t.Error(err)
				}
			}()
			w := editor.request("POST", base+"/drafts", map[string]string{"title": "commit fault", "sql": "CREATE SCHEMA " + schema + "; CREATE TABLE " + schema + ".proof(id int); INSERT INTO " + schema + ".proof VALUES(1)", "sql_down": "DROP SCHEMA " + schema + " CASCADE"})
			status(t, w, 201)
			var d runner.Draft
			if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("%s/drafts/%d/execute", base, d.ID)
			approveDraft(t, a, editor, target.ID, d.ID, 1)
			w = editor.executeAndWait(t, path, map[string]string{"revision": "1"})
			status(t, w, 202)
			var attempt runner.Result
			if err := json.Unmarshal(w.Body.Bytes(), &attempt); err != nil || attempt.Status != "unknown" {
				t.Fatalf("outcome=%s", w.Body.String())
			}
			w = editor.request("POST", base+"/migrations/reconcile", nil)
			status(t, w, 200)
			var result struct {
				Items []runner.Result `json:"reconciled"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Items) != 1 {
				t.Fatalf("recovery=%s", w.Body.String())
			}
			want := "failed"
			if after {
				want = "success"
			}
			if result.Items[0].Status != want || result.Items[0].ExecutionID != attempt.ExecutionID {
				t.Fatalf("recovery=%+v want %s", result.Items, want)
			}
			var exists bool
			if err = db.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", schema+".proof").Scan(&exists); err != nil || exists != after {
				t.Fatalf("target changes exist=%v after=%v err=%v", exists, after, err)
			}
			w = editor.executeAndWait(t, path, map[string]string{"revision": "1"})
			status(t, w, 200)
			var replay runner.Result
			if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.Status != want || replay.ExecutionID != attempt.ExecutionID {
				t.Fatal("replay changed attempt")
			}
		})
	}
}

func TestRecoveryAfterWaitingForCommit(t *testing.T) {
	ctx := context.Background()
	owner, err := database.Open(ctx, os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatal("cannot open target fixture admin")
	}
	defer owner.Close()
	cfg, err := pgx.ParseConfig(os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid target fixture URL")
	}
	var settings []string
	if err = owner.QueryRow(ctx, `SELECT COALESCE((SELECT setconfig FROM pg_catalog.pg_db_role_setting
 WHERE setrole=0 AND setdatabase=(SELECT oid FROM pg_catalog.pg_database WHERE datname=$1)),ARRAY[]::text[])`, cfg.Database).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	// Use a database setting so the CI database owner need not administer roles.
	prefix := "ALTER DATABASE " + pgx.Identifier{cfg.Database}.Sanitize()
	defer func() {
		restore := prefix + " RESET default_transaction_isolation"
		for _, setting := range settings {
			if strings.HasPrefix(setting, "default_transaction_isolation=") {
				value := strings.TrimPrefix(setting, "default_transaction_isolation=")
				restore = prefix + " SET default_transaction_isolation = '" + strings.ReplaceAll(value, "'", "''") + "'"
			}
		}
		if _, err := owner.Exec(ctx, restore); err != nil {
			t.Error("cannot restore target fixture isolation", err)
		}
	}()
	for _, isolation := range []string{"repeatable read", "serializable"} {
		t.Run(isolation, func(t *testing.T) {
			if _, err := owner.Exec(ctx, prefix+" SET default_transaction_isolation = '"+isolation+"'"); err != nil {
				t.Fatal(err)
			}
			probe, err := database.Open(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
			if err != nil {
				t.Fatal("cannot check target fixture isolation")
			}
			var actual string
			err = probe.QueryRow(ctx, "SHOW default_transaction_isolation").Scan(&actual)
			probe.Close()
			if err != nil || actual != isolation {
				t.Fatal("target fixture isolation was not applied", err)
			}
			a := newM1App(t)
			admin := a.browser()
			admin.login(t, "admin")
			input := targetInput(t, "TEST_TARGET_DATABASE_URL", "recovery-snapshot")
			input.Port = commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, true)
			target := createTarget(t, admin, input)
			grant(t, a, "editor", target.ID, "edit")
			editor, checker := a.browser(), a.browser()
			editor.login(t, "editor")
			checker.login(t, "editor")
			base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
			w := editor.request("POST", base+"/drafts", map[string]string{"title": "recovery snapshot", "sql": "SELECT pg_catalog.pg_advisory_xact_lock(3110099)", "sql_down": "SELECT 1"})
			status(t, w, 201)
			var draft runner.Draft
			if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
				t.Fatal(err)
			}
			approveDraft(t, a, editor, target.ID, draft.ID, 1)
			barrier, err := owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var executeDone, recoveryDone chan *httptest.ResponseRecorder
			defer func() {
				_ = barrier.Rollback(ctx)
				for _, done := range []chan *httptest.ResponseRecorder{executeDone, recoveryDone} {
					if done != nil {
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("recovery fixture request did not finish")
						}
					}
				}
			}()
			if _, err = barrier.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(3110099)"); err != nil {
				t.Fatal(err)
			}
			waitForLock := func(key int64) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for {
					var waiting bool
					if err := owner.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks
 WHERE locktype='advisory' AND database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
 AND classid=0 AND objid=$1::oid AND objsubid=1 AND NOT granted)`, key).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("request did not wait for target lock %d", key)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			executeDone = make(chan *httptest.ResponseRecorder, 1)
			go func() {
				defer close(executeDone)
				executeDone <- editor.executeAndWait(t, fmt.Sprintf("%s/drafts/%d/execute", base, draft.ID), map[string]string{"revision": "1"})
			}()
			waitForLock(3110099)
			recoveryDone = make(chan *httptest.ResponseRecorder, 1)
			go func() {
				defer close(recoveryDone)
				recoveryDone <- checker.request("POST", base+"/migrations/reconcile", nil)
			}()
			waitForLock(3110003)
			if err = barrier.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			status(t, <-executeDone, 202)
			w = <-recoveryDone
			status(t, w, 200)
			var out struct {
				Items []runner.Result `json:"reconciled"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Items) != 1 || out.Items[0].Status != "success" || out.Items[0].ErrorCode != nil {
				t.Fatalf("committed migration not recovered after lock wait: %s", w.Body.String())
			}
			var count int
			if err := owner.QueryRow(ctx, "SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1", out.Items[0].ExecutionID).Scan(&count); err != nil || count != 1 {
				t.Fatal("expected one durable commit receipt", err)
			}
			var state string
			if err := a.db.QueryRow(ctx, "SELECT status FROM migration_history WHERE execution_id=$1", out.Items[0].ExecutionID).Scan(&state); err != nil || state != "success" {
				t.Fatal("recovered status not persisted", err)
			}
		})
	}
}

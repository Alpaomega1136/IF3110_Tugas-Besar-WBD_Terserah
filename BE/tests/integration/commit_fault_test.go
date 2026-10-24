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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"terserah/backend/internal/database"
	"terserah/backend/internal/runner"
)

// The fixture relays plaintext PostgreSQL frames and cuts exactly one COMMIT
// either before forwarding it or after the server confirms durable success.
func commitProxy(t *testing.T, address string, fault, after bool) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
					if fault && !after && kind == 'Q' && strings.EqualFold(string(payload), "COMMIT\x00") && once.CompareAndSwap(false, true) {
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
					if fault && after && kind == 'C' && string(payload) == "COMMIT\x00" && once.CompareAndSwap(false, true) {
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
			w := editor.request("POST", base+"/drafts", map[string]string{"title": "commit fault", "sql": "CREATE SCHEMA " + schema + "; CREATE TABLE " + schema + ".proof(id int); INSERT INTO " + schema + ".proof VALUES(1)"})
			status(t, w, 201)
			var d runner.Draft
			if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("%s/drafts/%d/execute", base, d.ID)
			w = editor.request("POST", path, map[string]string{"revision": "1"})
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
			w = editor.request("POST", path, map[string]string{"revision": "1"})
			status(t, w, 200)
			var replay runner.Result
			if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.Status != want || replay.ExecutionID != attempt.ExecutionID {
				t.Fatal("replay changed attempt")
			}
		})
	}
}

//go:build integration

package smoke_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/database"
)

type browser struct {
	client                 *http.Client
	origin, csrf, password string
}

func newBrowser(t *testing.T, origin, password string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	return &browser{client: &http.Client{Jar: jar, Transport: transport, Timeout: 12 * time.Second}, origin: origin, password: password}
}

func (b *browser) call(t *testing.T, method, path string, input any, want int, out any) {
	t.Helper()
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal("cannot encode smoke request")
		}
	}
	req, err := http.NewRequest(method, b.origin+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal("invalid smoke request")
	}
	req.Header.Set("Origin", b.origin)
	req.Header.Set("Content-Type", "application/json")
	if b.csrf != "" {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: HTTP transport failed", method, path)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 {
		t.Fatal("cannot read bounded smoke response")
	}
	// Never print HTTP bodies: even a failed assertion can contain CSRF or SQL.
	if res.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d", method, path, res.StatusCode, want)
	}
	if bytes.Contains(data, []byte(b.password)) {
		t.Fatal("response leaked fixture password")
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: invalid JSON response", method, path)
		}
	}
	if path == "/api/v1/auth/login" && want == 200 {
		protected := false
		for _, cookie := range res.Cookies() {
			if cookie.Name == "terserah_session" {
				protected = cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode && cookie.Path == "/"
			}
		}
		if !protected {
			t.Fatal("session cookie protections missing")
		}
	}
}

func (b *browser) login(t *testing.T, name string) {
	t.Helper()
	var csrf struct {
		Token string `json:"csrf_token"`
	}
	b.call(t, "GET", "/api/v1/auth/csrf", nil, 200, &csrf)
	if csrf.Token == "" {
		t.Fatal("missing prelogin CSRF")
	}
	b.csrf = csrf.Token
	b.call(t, "POST", "/api/v1/auth/login", map[string]string{"username": name, "password": b.password}, 200, nil)
	b.call(t, "GET", "/api/v1/auth/csrf", nil, 200, &csrf)
	if csrf.Token == "" {
		t.Fatal("missing session CSRF")
	}
	b.csrf = csrf.Token
}

func (b *browser) executeAndWait(t *testing.T, path, revision string, replayed bool) runner.Result {
	t.Helper()
	var response struct {
		Job      jobs.Job       `json:"job"`
		Attempts []jobs.Attempt `json:"attempts"`
	}
	want := 202
	if replayed {
		want = 200
	}
	b.call(t, "POST", path, map[string]string{"revision": revision}, want, &response)
	jobPath := fmt.Sprintf("/api/v1/databases/%d/jobs/%d", response.Job.DatabaseID, response.Job.ID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b.call(t, "GET", jobPath, nil, 200, &response)
		if response.Job.Status == jobs.Success || response.Job.Status == jobs.Failed || response.Job.Status == jobs.Unknown {
			if len(response.Attempts) != 1 || response.Attempts[0].Version == nil {
				t.Fatal("completed job has no execution history")
			}
			a := response.Attempts[0]
			return runner.Result{ExecutionID: a.ExecutionID, Version: *a.Version, Status: string(response.Job.Status), ErrorCode: response.Job.ErrorCode}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker did not finish production HTTP job")
	return runner.Result{}
}

// Launch all three production entry points against an isolated metadata schema.
func startAPI(t *testing.T, firstAdminCommand string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	metadataURL := os.Getenv("TEST_DATABASE_URL")
	if metadataURL == "" {
		t.Fatal("TEST_DATABASE_URL must name a dedicated fixture database")
	}
	db, err := database.Open(ctx, metadataURL)
	if err != nil {
		t.Fatal("cannot open smoke metadata database")
	}
	t.Cleanup(db.Close)
	schema := fmt.Sprintf("smoke_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = db.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("cannot create isolated smoke schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("cannot clean smoke metadata schema")
		}
	})
	u, err := url.Parse(metadataURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("smoke TEST_DATABASE_URL must use postgres URL form")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	origin := "http://" + address
	var key [32]byte
	if _, err = rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	var passwordBytes [24]byte
	if _, err = rand.Read(passwordBytes[:]); err != nil {
		t.Fatal(err)
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes[:])
	env := append(os.Environ(), "GOTOOLCHAIN=local", "DATABASE_URL="+u.String(), "HTTP_ADDR="+address, "APP_ORIGIN="+origin, "TARGET_CREDENTIAL_KEY="+base64.StdEncoding.EncodeToString(key[:]), "BOOTSTRAP_USERNAME=demo-admin", "BOOTSTRAP_PASSWORD="+password, "ADMIN_USERNAME=demo-admin", "ADMIN_PASSWORD="+password)
	env = append(env, "WORKER_EMBEDDED=true", "OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=")
	if firstAdminCommand == "bootstrap" {
		env = append(env, "WORKER_EMBEDDED=false")
	}
	bin := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for _, name := range []string{"migrate", "bootstrap", "seed", "api", "worker"} {
		buildCtx, stop := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(buildCtx, "go", "build", "-o", filepath.Join(bin, name+suffix), "./cmd/"+name)
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		err = cmd.Run()
		stop()
		if err != nil {
			t.Fatalf("cannot build production %s with installed Go", name)
		}
	}
	cliCtx, stopCLI := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCLI()
	setupCommands := []string{"migrate", "migrate", firstAdminCommand}
	if firstAdminCommand == "seed" {
		setupCommands = []string{"seed", "migrate", "migrate"}
	}
	for _, name := range setupCommands {
		if name == "bootstrap" {
			check := exec.CommandContext(cliCtx, filepath.Join(bin, "bootstrap"+suffix), "-check")
			check.Env = env
			output, e := check.Output()
			if e != nil || strings.TrimSpace(string(output)) != "false" {
				t.Fatal("bootstrap check must report missing admin before first creation")
			}
			invalid := exec.CommandContext(cliCtx, filepath.Join(bin, "bootstrap"+suffix))
			invalid.Env = append(append([]string(nil), env...), "BOOTSTRAP_PASSWORD=too-short")
			if invalid.Run() == nil {
				t.Fatal("short bootstrap password accepted")
			}
			check = exec.CommandContext(cliCtx, filepath.Join(bin, "bootstrap"+suffix), "-check")
			check.Env = env
			output, e = check.Output()
			if e != nil || strings.TrimSpace(string(output)) != "false" {
				t.Fatal("failed bootstrap must remain recoverable")
			}
		}
		cmd := exec.CommandContext(cliCtx, filepath.Join(bin, name+suffix))
		cmd.Env = env
		if err = cmd.Run(); err != nil {
			t.Fatalf("production %s CLI failed", name)
		}
	}
	check := exec.CommandContext(cliCtx, filepath.Join(bin, "bootstrap"+suffix), "-check")
	check.Env = env
	var checkLogs bytes.Buffer
	check.Stderr = &checkLogs
	if output, e := check.Output(); e != nil || strings.TrimSpace(string(output)) != "true" {
		t.Fatal("bootstrap check must recognize the existing admin")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(checkLogs.Bytes()), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatal("bootstrap SQL logs must remain JSON on stderr, separate from its boolean stdout")
		}
	}
	for _, name := range []string{"bootstrap", "seed"} {
		cmd := exec.CommandContext(cliCtx, filepath.Join(bin, name+suffix))
		cmd.Env = append(append([]string(nil), env...), "BOOTSTRAP_PASSWORD=different-"+password, "ADMIN_PASSWORD=different-"+password)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "an admin already exists") {
			t.Fatalf("%s must refuse to replace existing admin", name)
		}
	}
	server := exec.Command(filepath.Join(bin, "api"+suffix))
	if firstAdminCommand == "bootstrap" {
		background := exec.Command(filepath.Join(bin, "worker"+suffix))
		background.Env = env
		background.Stdout = io.Discard
		background.Stderr = io.Discard
		if err = background.Start(); err != nil {
			t.Fatal("cannot start standalone worker")
		}
		workerDone := make(chan struct{})
		go func() { _ = background.Wait(); close(workerDone) }()
		t.Cleanup(func() {
			_ = background.Process.Kill()
			select {
			case <-workerDone:
			case <-time.After(5 * time.Second):
				t.Error("worker cleanup timed out")
			}
		})
	}
	server.Env = env
	server.Stdout = io.Discard
	server.Stderr = io.Discard
	if err = server.Start(); err != nil {
		t.Fatal("cannot start production API")
	}
	done := make(chan struct{})
	go func() { _ = server.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = server.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("API process cleanup timed out")
		}
	})
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(origin + "/health/ready")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 {
				return origin, password
			}
		}
		select {
		case <-done:
			t.Fatal("API exited before readiness")
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("API readiness timed out")
	return "", ""
}

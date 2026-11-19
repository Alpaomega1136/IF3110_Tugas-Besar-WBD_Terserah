package config

import (
	"encoding/base64"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing database URL accepted")
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/platform")
	t.Setenv("HTTP_ADDR", "")
	cfg, err := Load()
	if err != nil || cfg.HTTPAddr != ":8080" {
		t.Fatalf("default config: %v, %v", cfg, err)
	}
	t.Setenv("HTTP_ADDR", ":9090")
	cfg, err = Load()
	if err != nil || cfg.HTTPAddr != ":9090" {
		t.Fatal("address override ignored")
	}
}

func TestLoadAPI(t *testing.T) {
	t.Setenv("WORKER_EMBEDDED", "")
	t.Setenv("DATABASE_URL", "postgres://localhost/platform")
	t.Setenv("APP_ORIGIN", "http://localhost:8080")
	t.Setenv("TARGET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if _, err := LoadAPI(); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"", "localhost:8080", "http://localhost:8080/", "https://user:password@example.com", "http://localhost:8080?x=1"} {
		t.Setenv("APP_ORIGIN", origin)
		if _, err := LoadAPI(); err == nil {
			t.Fatalf("invalid origin accepted: %q", origin)
		}
	}
	t.Setenv("APP_ORIGIN", "http://localhost:8080")
	t.Setenv("TARGET_CREDENTIAL_KEY", "not a key")
	if _, err := LoadAPI(); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestWorkerConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/platform")
	t.Setenv("TARGET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("APP_ORIGIN", "")
	if _, err := LoadWorker(); err != nil {
		t.Fatal("standalone worker should not require APP_ORIGIN", err)
	}
	t.Setenv("APP_ORIGIN", "http://localhost:3000")
	for _, entry := range []struct {
		value   string
		enabled bool
	}{{"", true}, {"true", true}, {"false", false}} {
		t.Setenv("WORKER_EMBEDDED", entry.value)
		cfg, err := LoadAPI()
		if err != nil || cfg.WorkerEmbedded != entry.enabled {
			t.Fatal("worker configuration ignored")
		}
	}
	t.Setenv("WORKER_EMBEDDED", "typo")
	if _, err := LoadAPI(); err == nil {
		t.Fatal("invalid worker configuration accepted")
	}
}

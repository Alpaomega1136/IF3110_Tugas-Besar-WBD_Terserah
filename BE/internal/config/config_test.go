package config

import "testing"

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

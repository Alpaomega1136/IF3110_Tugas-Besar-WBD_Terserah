package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("FE_HTTP_ADDR", "")
	t.Setenv("FE_BACKEND_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":3000" || cfg.BackendURL.String() != "http://localhost:8080" {
		t.Fatalf("unexpected default config: %#v", cfg)
	}
}

func TestLoadRejectsUnsafeBackendURLs(t *testing.T) {
	for _, badURL := range []string{"file:///tmp/secret", "http://user:pass@localhost:8080", "not-an-http-url"} {
		t.Run(badURL, func(t *testing.T) {
			t.Setenv("FE_BACKEND_URL", badURL)
			if _, err := Load(); err == nil {
				t.Fatal("invalid backend URL accepted")
			}
		})
	}
}

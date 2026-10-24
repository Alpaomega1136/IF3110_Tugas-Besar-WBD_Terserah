package config

import (
	"errors"
	"net/url"
	"os"
)

type Config struct {
	HTTPAddr   string
	BackendURL *url.URL
}

func Load() (Config, error) {
	addr := os.Getenv("FE_HTTP_ADDR")
	if addr == "" {
		addr = ":3000"
	}

	backend := os.Getenv("FE_BACKEND_URL")
	if backend == "" {
		backend = "http://localhost:8080"
	}

	u, err := url.Parse(backend)
	if err != nil {
		return Config{}, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, errors.New("FE_BACKEND_URL must be an http(s) URL with host and without credentials/query/fragment")
	}

	return Config{HTTPAddr: addr, BackendURL: u}, nil
}

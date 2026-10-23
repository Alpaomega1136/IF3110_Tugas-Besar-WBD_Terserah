package config

import (
	"errors"
	"os"
)

type Config struct {
	HTTPAddr     string
	DatabaseURL  string
	CookieSecure bool
}

// Load reads process environment; it does not load .env files automatically.
func Load() (Config, error) {
	secure := os.Getenv("COOKIE_SECURE")
	c := Config{
		HTTPAddr:     os.Getenv("HTTP_ADDR"),
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		CookieSecure: secure == "true" || secure == "1",
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return c, nil
}

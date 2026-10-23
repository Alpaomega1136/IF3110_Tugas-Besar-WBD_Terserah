package config

import (
	"encoding/base64"
	"errors"
	"net/url"
	"os"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	AppOrigin   string
	SecretKey   []byte
}

// LoadAPI requires secrets only for the API, not the metadata migration CLI.
func LoadAPI() (Config, error) {
	c, err := Load()
	if err != nil {
		return Config{}, err
	}
	c.AppOrigin = os.Getenv("APP_ORIGIN")
	u, err := url.Parse(c.AppOrigin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, errors.New("APP_ORIGIN must be an http(s) origin without a path")
	}
	c.SecretKey, err = base64.StdEncoding.DecodeString(os.Getenv("TARGET_CREDENTIAL_KEY"))
	if err != nil || len(c.SecretKey) != 32 {
		return Config{}, errors.New("TARGET_CREDENTIAL_KEY must be base64-encoded 32 random bytes")
	}
	return c, nil
}

// Load reads process environment; it does not load .env files automatically.
func Load() (Config, error) {
	c := Config{HTTPAddr: os.Getenv("HTTP_ADDR"), DatabaseURL: os.Getenv("DATABASE_URL")}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return c, nil
}

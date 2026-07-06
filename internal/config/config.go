// Package config parses server configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all server configuration. Everything comes from env vars;
// see .env.example for the full list.
type Config struct {
	APIKey          string
	ListenAddr      string
	DatabaseURL     string
	RedisAddr       string
	StoragePath     string
	MaxUploadBytes  int64
	MaxPixels       int64
	RateLimitPerMin int
}

// Load reads configuration from the environment. API_KEY and DATABASE_URL
// are required; everything else has a default.
func Load() (Config, error) {
	cfg := Config{
		APIKey:      os.Getenv("API_KEY"),
		ListenAddr:  getenvDefault("LISTEN_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		RedisAddr:   getenvDefault("REDIS_ADDR", "localhost:6379"),
		StoragePath: getenvDefault("STORAGE_PATH", "./data/images"),
	}

	if cfg.APIKey == "" {
		return Config{}, fmt.Errorf("API_KEY is required")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	var err error
	if cfg.MaxUploadBytes, err = getenvInt64("MAX_UPLOAD_BYTES", 10*1024*1024); err != nil {
		return Config{}, err
	}
	if cfg.MaxPixels, err = getenvInt64("MAX_PIXELS", 50_000_000); err != nil {
		return Config{}, err
	}
	rate, err := getenvInt64("RATE_LIMIT_PER_MIN", 120)
	if err != nil {
		return Config{}, err
	}
	cfg.RateLimitPerMin = int(rate)

	if cfg.MaxUploadBytes <= 0 {
		return Config{}, fmt.Errorf("MAX_UPLOAD_BYTES must be positive")
	}
	if cfg.MaxPixels <= 0 {
		return Config{}, fmt.Errorf("MAX_PIXELS must be positive")
	}
	if cfg.RateLimitPerMin <= 0 {
		return Config{}, fmt.Errorf("RATE_LIMIT_PER_MIN must be positive")
	}

	return cfg, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt64(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not a valid integer: %q", key, v)
	}
	return n, nil
}

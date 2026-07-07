package config

import (
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every config variable so tests are hermetic against
// whatever the invoking shell has exported (Load treats "" as unset).
func clearEnv(t *testing.T) {
	for _, k := range []string{
		"API_KEY", "LISTEN_ADDR", "DATABASE_URL", "REDIS_ADDR",
		"STORAGE_PATH", "MAX_UPLOAD_BYTES", "MAX_PIXELS", "RATE_LIMIT_PER_MIN",
		"CACHE_CONTROL_MAX_AGE", "DERIVATIVE_CACHE_TTL",
	} {
		t.Setenv(k, "")
	}
}

func setRequired(t *testing.T) {
	clearEnv(t)
	t.Setenv("API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := Config{
		APIKey:             "test-key",
		ListenAddr:         ":8080",
		DatabaseURL:        "postgres://localhost/test",
		RedisAddr:          "localhost:6379",
		StoragePath:        "./data/images",
		MaxUploadBytes:     10 * 1024 * 1024,
		MaxPixels:          50_000_000,
		RateLimitPerMin:    120,
		CacheControlMaxAge: 31_536_000,
		DerivativeCacheTTL: 720 * time.Hour,
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("LISTEN_ADDR", ":9090")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("STORAGE_PATH", "/data/images")
	t.Setenv("MAX_UPLOAD_BYTES", "1048576")
	t.Setenv("MAX_PIXELS", "1000000")
	t.Setenv("RATE_LIMIT_PER_MIN", "10")
	t.Setenv("CACHE_CONTROL_MAX_AGE", "3600")
	t.Setenv("DERIVATIVE_CACHE_TTL", "24h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := Config{
		APIKey:             "test-key",
		ListenAddr:         ":9090",
		DatabaseURL:        "postgres://localhost/test",
		RedisAddr:          "redis:6379",
		StoragePath:        "/data/images",
		MaxUploadBytes:     1048576,
		MaxPixels:          1000000,
		RateLimitPerMin:    10,
		CacheControlMaxAge: 3600,
		DerivativeCacheTTL: 24 * time.Hour,
	}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "missing API_KEY",
			env:     map[string]string{"DATABASE_URL": "postgres://localhost/test"},
			wantErr: "API_KEY is required",
		},
		{
			name:    "missing DATABASE_URL",
			env:     map[string]string{"API_KEY": "k"},
			wantErr: "DATABASE_URL is required",
		},
		{
			name: "non-integer MAX_UPLOAD_BYTES",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "MAX_UPLOAD_BYTES": "ten",
			},
			wantErr: "MAX_UPLOAD_BYTES",
		},
		{
			name: "non-integer MAX_PIXELS",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "MAX_PIXELS": "lots",
			},
			wantErr: "MAX_PIXELS",
		},
		{
			name: "non-integer RATE_LIMIT_PER_MIN",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "RATE_LIMIT_PER_MIN": "fast",
			},
			wantErr: "RATE_LIMIT_PER_MIN",
		},
		{
			name: "zero MAX_UPLOAD_BYTES",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "MAX_UPLOAD_BYTES": "0",
			},
			wantErr: "MAX_UPLOAD_BYTES must be positive",
		},
		{
			name: "negative MAX_PIXELS",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "MAX_PIXELS": "-1",
			},
			wantErr: "MAX_PIXELS must be positive",
		},
		{
			name: "zero RATE_LIMIT_PER_MIN",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "RATE_LIMIT_PER_MIN": "0",
			},
			wantErr: "RATE_LIMIT_PER_MIN must be positive",
		},
		{
			name: "non-integer CACHE_CONTROL_MAX_AGE",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "CACHE_CONTROL_MAX_AGE": "forever",
			},
			wantErr: "CACHE_CONTROL_MAX_AGE",
		},
		{
			name: "negative CACHE_CONTROL_MAX_AGE",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "CACHE_CONTROL_MAX_AGE": "-1",
			},
			wantErr: "CACHE_CONTROL_MAX_AGE must not be negative",
		},
		{
			name: "non-duration DERIVATIVE_CACHE_TTL",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "DERIVATIVE_CACHE_TTL": "monthly",
			},
			wantErr: "DERIVATIVE_CACHE_TTL",
		},
		{
			name: "zero DERIVATIVE_CACHE_TTL",
			env: map[string]string{
				"API_KEY": "k", "DATABASE_URL": "d", "DERIVATIVE_CACHE_TTL": "0s",
			},
			wantErr: "DERIVATIVE_CACHE_TTL must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

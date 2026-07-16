package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/pyaethu-aung/image-server/internal/api"
	"github.com/pyaethu-aung/image-server/internal/config"
	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/fetch"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

const fetchTimeout = 30 * time.Second

func main() {
	// `server healthcheck` is invoked by the Dockerfile's HEALTHCHECK
	// instruction. It probes this same binary's own /healthz over loopback
	// and exits 0/1, so the runtime image never needs curl (smaller image,
	// no extra CVE surface to patch).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Pool size is not configured here; pgxpool defaults to max(4, NumCPU())
	// conns. Override via pool_max_conns/pool_min_conns query params on
	// DATABASE_URL if that default is ever wrong for a deployment.
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("postgres connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(pingCtx); err != nil {
		slog.Error("postgres ping", "err", err)
		os.Exit(1)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		slog.Error("redis ping", "err", err)
		os.Exit(1)
	}

	store, err := storage.NewLocal(cfg.StoragePath)
	if err != nil {
		slog.Error("storage", "err", err)
		os.Exit(1)
	}

	server := api.NewServer(cfg, store, db.New(pool), rdb, fetch.New(fetchTimeout, cfg.MaxUploadBytes))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewRouter(server),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "err", err)
			os.Exit(1)
		}
	}
}

// runHealthcheck implements `server healthcheck`. It reuses config.Load() so
// it always probes the port the main process is actually listening on
// (LISTEN_ADDR), rather than a hardcoded value that could silently drift.
func runHealthcheck() {
	cfg, err := config.Load()
	if err != nil {
		os.Exit(1)
	}
	addr := cfg.ListenAddr
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		os.Exit(1)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

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

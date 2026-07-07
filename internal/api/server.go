package api

import (
	"net/http"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/config"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// Server implements the generated ServerInterface. Every endpoint is
// implemented, so the gen.Unimplemented embed is gone; the assertion below
// keeps the compiler honest about that.
type Server struct {
	cfg     config.Config
	store   storage.Storage
	images  imageStore
	fetcher imageFetcher
	rdb     *redis.Client
	limiter *redis_rate.Limiter
}

var _ gen.ServerInterface = (*Server)(nil)

// NewServer wires the handler dependencies. images is satisfied by
// *db.Queries and fetcher by *fetch.Client; unit tests inject fakes.
func NewServer(cfg config.Config, store storage.Storage, images imageStore, rdb *redis.Client, fetcher imageFetcher) *Server {
	return &Server{
		cfg:     cfg,
		store:   store,
		images:  images,
		fetcher: fetcher,
		rdb:     rdb,
		limiter: redis_rate.NewLimiter(rdb),
	}
}

// Healthz implements GET /healthz (public, no auth).
func (s *Server) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

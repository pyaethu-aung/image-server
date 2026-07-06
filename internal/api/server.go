package api

import (
	"net/http"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/config"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// Server implements the generated ServerInterface. Endpoints not built yet
// fall through to the embedded gen.Unimplemented (501).
type Server struct {
	gen.Unimplemented

	cfg     config.Config
	store   storage.Storage
	images  imageStore
	fetcher imageFetcher
	limiter *redis_rate.Limiter
}

// NewServer wires the handler dependencies. images is satisfied by
// *db.Queries and fetcher by *fetch.Client; unit tests inject fakes.
func NewServer(cfg config.Config, store storage.Storage, images imageStore, rdb *redis.Client, fetcher imageFetcher) *Server {
	return &Server{
		cfg:     cfg,
		store:   store,
		images:  images,
		fetcher: fetcher,
		limiter: redis_rate.NewLimiter(rdb),
	}
}

// Healthz implements GET /healthz (public, no auth).
func (s *Server) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

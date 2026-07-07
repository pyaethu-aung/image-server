package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Redis layout for the derivative cache. Storage is authoritative; Redis is
// a best-effort accelerator, so every helper here fails open (logs and
// degrades to a storage check) rather than failing the request.
//
//	imgcache:<id>:<hash>  marker: this derivative exists in storage
//	imgderivs:<id>        set of <hash> values, so delete can purge markers
func markerKey(id uuid.UUID, ck string) string {
	return "imgcache:" + id.String() + ":" + ck
}

func derivSetKey(id uuid.UUID) string {
	return "imgderivs:" + id.String()
}

// cacheHas reports whether Redis remembers the derivative. A Redis error
// counts as a miss.
func (s *Server) cacheHas(ctx context.Context, id uuid.UUID, ck string) bool {
	n, err := s.rdb.Exists(ctx, markerKey(id, ck)).Result()
	if err != nil {
		slog.Error("derivative cache lookup failed", "err", err, "id", id)
		return false
	}
	return n > 0
}

// cacheRemember records that the derivative exists in storage: the marker and
// the per-image set entry, both bounded by DerivativeCacheTTL.
func (s *Server) cacheRemember(ctx context.Context, id uuid.UUID, ck string) {
	ttl := s.cfg.DerivativeCacheTTL
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, markerKey(id, ck), "1", ttl)
		p.SAdd(ctx, derivSetKey(id), ck)
		p.Expire(ctx, derivSetKey(id), ttl)
		return nil
	})
	if err != nil {
		slog.Error("derivative cache store failed", "err", err, "id", id)
	}
}

// cachePurge forgets every derivative marker for the image plus the set
// itself. Storage remains the source of truth, so a partial purge only costs
// stale markers that expire with the TTL.
func (s *Server) cachePurge(ctx context.Context, id uuid.UUID) {
	cks, err := s.rdb.SMembers(ctx, derivSetKey(id)).Result()
	if err != nil {
		slog.Error("derivative cache purge enumerate failed", "err", err, "id", id)
		cks = nil
	}
	keys := make([]string, 0, len(cks)+1)
	for _, ck := range cks {
		keys = append(keys, markerKey(id, ck))
	}
	keys = append(keys, derivSetKey(id))
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		slog.Error("derivative cache purge failed", "err", err, "id", id)
	}
}

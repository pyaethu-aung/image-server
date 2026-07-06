package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-redis/redis_rate/v10"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
)

// secured reports whether the generated wrapper marked the route as
// requiring the ApiKeyAuth scheme. /healthz never carries the value, so it
// stays public without any route list here.
func secured(r *http.Request) bool {
	return r.Context().Value(gen.ApiKeyAuthScopes) != nil
}

// authMiddleware enforces X-API-Key on secured routes. The comparison is
// constant-time so the key cannot be recovered byte-by-byte from timing.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !secured(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid X-API-Key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware enforces a per-key GCRA limit backed by Redis. The
// Redis key holds a hash of the API key, never the key itself. If Redis is
// unavailable the request is allowed through (fail open): rate limiting is
// protection, not a hard dependency of uploads.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !secured(r) {
			next.ServeHTTP(w, r)
			return
		}
		sum := sha256.Sum256([]byte(r.Header.Get("X-API-Key")))
		key := "ratelimit:" + hex.EncodeToString(sum[:8])
		res, err := s.limiter.Allow(r.Context(), key, redis_rate.PerMinute(s.cfg.RateLimitPerMin))
		if err != nil {
			slog.Error("rate limit check failed, failing open", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if res.Allowed == 0 {
			secs := int(math.Ceil(res.RetryAfter.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded for this API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

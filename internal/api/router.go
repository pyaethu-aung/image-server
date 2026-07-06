// Package api wires the HTTP router, handlers, and middleware around the
// interfaces generated from the OpenAPI spec (internal/api/gen).
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
)

// NewRouter mounts the generated routes on a chi router.
//
// HandlerWithOptions applies Middlewares as handler = mw(handler) in slice
// order, so the last element is outermost and runs first: auth before rate
// limit, so unauthenticated requests never spend rate budget.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.Recoverer, requestLogger)
	return gen.HandlerWithOptions(s, gen.ChiServerOptions{
		BaseRouter:  r,
		Middlewares: []gen.MiddlewareFunc{s.rateLimitMiddleware, s.authMiddleware},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// Parameter binding failures (e.g. a non-UUID id) land here.
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		},
	})
}

// requestLogger emits one structured log line per request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration", time.Since(start),
			"request_id", chimw.GetReqID(r.Context()),
		)
	})
}

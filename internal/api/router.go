// Package api wires the HTTP router, handlers, and middleware.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter builds the HTTP router. The /v1 handlers land here as they are
// implemented (via the generated ServerInterface in internal/api/gen).
func NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", handleHealthz)
	return r
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

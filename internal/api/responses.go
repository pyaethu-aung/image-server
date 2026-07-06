package api

import (
	"encoding/json"
	"net/http"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/db"
)

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the spec's Error shape.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, gen.Error{Code: code, Message: message})
}

// toAPIImage converts a database row to the spec's Image model.
func toAPIImage(i db.Image) gen.Image {
	return gen.Image{
		Id:               i.ID,
		OriginalFilename: i.OriginalFilename,
		ContentHash:      i.ContentHash,
		MimeType:         i.MimeType,
		Width:            int(i.Width),
		Height:           int(i.Height),
		SizeBytes:        i.SizeBytes,
		CreatedAt:        i.CreatedAt.Time,
	}
}

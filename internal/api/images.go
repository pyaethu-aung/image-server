package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/imageproc"
)

// GetImage implements GET /v1/images/{id}. Without transform params it serves
// the stored original; with params it serves a derivative rendered on the fly.
// The generated params are ignored in favour of re-parsing the raw query, so
// imageproc.ParseTransform stays the single source of validation truth.
func (s *Server) GetImage(w http.ResponseWriter, r *http.Request, id gen.ImageID, _ gen.GetImageParams) {
	img, err := s.images.GetImage(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "image not found")
			return
		}
		internalError("get image", err).write(w)
		return
	}

	t, err := imageproc.ParseTransform(r.URL.Query())
	if err != nil {
		var pe *imageproc.ParamError
		if errors.As(err, &pe) {
			writeError(w, http.StatusBadRequest, "bad_request", pe.Error())
			return
		}
		internalError("parse transform", err).write(w)
		return
	}

	if t.IsIdentity() {
		s.serveOriginal(w, r, img)
		return
	}
	s.serveTransformed(w, r, img, t)
}

// GetImageMeta implements GET /v1/images/{id}/meta.
func (s *Server) GetImageMeta(w http.ResponseWriter, r *http.Request, id gen.ImageID) {
	img, err := s.images.GetImage(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "image not found")
			return
		}
		internalError("get image meta", err).write(w)
		return
	}
	// Metadata may change (a future re-tag), so it is not cached like bytes.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, toAPIImage(img))
}

// serveOriginal streams the stored original bytes back unchanged.
func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request, img db.Image) {
	rc, err := s.store.Get(r.Context(), img.StorageKey)
	if err != nil {
		// The DB row exists but the object does not: a storage/DB inconsistency,
		// which is a server-side fault rather than client input.
		internalError("get original", err).write(w)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", img.MimeType)
	s.setImageCacheControl(w)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// The status and headers are already committed, so this can only be
		// logged, not turned into an error response.
		slog.Error("serve original", "err", err, "id", img.ID)
	}
}

// serveTransformed renders img through t and streams the derivative back.
func (s *Server) serveTransformed(w http.ResponseWriter, r *http.Request, img db.Image, t imageproc.Transform) {
	rc, err := s.store.Get(r.Context(), img.StorageKey)
	if err != nil {
		internalError("get original", err).write(w)
		return
	}
	src, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		internalError("read original", err).write(w)
		return
	}

	out, contentType, err := imageproc.Apply(src, t, s.cfg.MaxPixels)
	if err != nil {
		internalError("apply transform", err).write(w)
		return
	}

	w.Header().Set("Content-Type", contentType)
	s.setImageCacheControl(w)
	w.WriteHeader(http.StatusOK)
	// out is libvips-encoded image bytes served with an explicit image/*
	// Content-Type and X-Content-Type-Options: nosniff, so it cannot be
	// interpreted as an active document.
	if _, err := w.Write(out); err != nil { //nolint:gosec // G705: image bytes, not HTML; served nosniff with an image/* type
		slog.Error("serve transformed", "err", err, "id", img.ID)
	}
}

// setImageCacheControl sets the caching policy for served image bytes and
// prevents content-type sniffing. Each URL (id + exact params) maps to
// immutable content, so it can be cached hard.
func (s *Server) setImageCacheControl(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable", s.cfg.CacheControlMaxAge))
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

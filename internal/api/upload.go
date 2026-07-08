package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/fetch"
	"github.com/pyaethu-aung/image-server/internal/imageproc"
)

// maxFromURLBodyBytes bounds the JSON request body for from-url uploads.
const maxFromURLBodyBytes = 1 << 20

// multipartMemoryBytes is the in-memory threshold for multipart parsing;
// larger parts spill to temp files. The security bound on total size is
// http.MaxBytesReader, not this.
const multipartMemoryBytes = 4 << 20

// uniqueViolation is the Postgres error code for a unique-constraint hit.
const uniqueViolation = "23505"

// httpError carries an error response through the ingest pipeline.
type httpError struct {
	status int
	code   string
	msg    string
}

func (e *httpError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.msg)
}

// UploadImage implements POST /v1/images (multipart).
func (s *Server) UploadImage(w http.ResponseWriter, r *http.Request) {
	// The size cap wraps the body before any parsing so it is enforced on
	// the wire, not on a header the client controls.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil { //nolint:gosec // G120: total size is capped by http.MaxBytesReader above
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds the configured size limit")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart form")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", `multipart field "file" is required`)
		return
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read uploaded file")
		return
	}

	img, herr := s.ingest(r.Context(), data, header.Filename)
	if herr != nil {
		herr.write(w)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIImage(img))
}

// UploadImageFromURL implements POST /v1/images/from-url.
func (s *Server) UploadImageFromURL(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFromURLBodyBytes)
	var body gen.UploadImageFromURLJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if body.Url == "" {
		writeError(w, http.StatusBadRequest, "bad_request", `"url" is required`)
		return
	}

	data, err := s.fetcher.Fetch(r.Context(), body.Url)
	if err != nil {
		if errors.Is(err, fetch.ErrTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "remote file exceeds the upload size limit")
			return
		}
		// Blocked destinations, bad schemes, unreachable hosts, and non-200
		// responses are all problems with the submitted URL.
		writeError(w, http.StatusBadRequest, "bad_request", "URL is not allowed or could not be fetched")
		return
	}

	img, herr := s.ingest(r.Context(), data, filenameFromURL(body.Url))
	if herr != nil {
		herr.write(w)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIImage(img))
}

// ingest validates, dedups, stores, and records one image. Both upload
// paths share it, so the security checks cannot drift apart.
func (s *Server) ingest(ctx context.Context, data []byte, filename string) (db.Image, *httpError) {
	info, err := imageproc.DetectImage(data)
	if err != nil {
		return db.Image{}, &httpError{http.StatusUnsupportedMediaType, "unsupported_media_type", "not a supported image type (jpeg, png, gif, webp, tiff, heic, heif, avif)"}
	}
	if err := imageproc.CheckPixelLimit(info.Width, info.Height, s.cfg.MaxPixels); err != nil {
		return db.Image{}, &httpError{http.StatusBadRequest, "bad_request", "image dimensions exceed the configured pixel limit"}
	}
	// The width/height DB columns are int32.
	if info.Width > math.MaxInt32 || info.Height > math.MaxInt32 {
		return db.Image{}, &httpError{http.StatusBadRequest, "bad_request", "image dimensions exceed the configured pixel limit"}
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	// Dedup: byte-identical uploads return the existing record.
	if img, err := s.images.GetImageByContentHash(ctx, hash); err == nil {
		return img, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Image{}, internalError("dedup lookup", err)
	}

	key := "originals/" + hash[:2] + "/" + hash[2:4] + "/" + hash
	if err := s.store.Put(ctx, key, bytes.NewReader(data), info.MimeType); err != nil {
		return db.Image{}, internalError("store original", err)
	}

	img, err := s.images.CreateImage(ctx, db.CreateImageParams{
		OriginalFilename: filename,
		ContentHash:      hash,
		MimeType:         info.MimeType,
		Width:            int32(info.Width),  //nolint:gosec // G115: bounded by the MaxInt32 check above
		Height:           int32(info.Height), //nolint:gosec // G115: bounded by the MaxInt32 check above
		SizeBytes:        int64(len(data)),
		StorageKey:       key,
	})
	if err != nil {
		// A concurrent identical upload may win the unique-constraint race;
		// its record is the dedup answer.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			if existing, lookupErr := s.images.GetImageByContentHash(ctx, hash); lookupErr == nil {
				return existing, nil
			}
		}
		return db.Image{}, internalError("create image record", err)
	}
	return img, nil
}

// internalError logs the cause and returns an opaque 500; internals never
// reach the response body.
func internalError(op string, err error) *httpError {
	slog.Error("upload failed", "op", op, "err", err)
	return &httpError{http.StatusInternalServerError, "internal", "internal error"}
}

// filenameFromURL extracts a best-effort display filename from the URL path.
func filenameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		return ""
	}
	return base
}

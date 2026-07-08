package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pyaethu-aung/image-server/internal/config"
	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// get sends a GET and returns the status, headers, and fully read body.
func (h *uploadHarness) get(t *testing.T, path string, withKey bool) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if withKey {
		req.Header.Set("X-API-Key", testAPIKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// storeOriginal writes PNG data to a fresh local storage at a fixed key and
// returns the storage plus the key, mirroring how ingest stores originals.
func storeOriginal(t *testing.T, data []byte) (storage.Storage, string) {
	t.Helper()
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}
	key := "originals/aa/bb/deadbeef"
	if err := st.Put(context.Background(), key, bytes.NewReader(data), "image/png"); err != nil {
		t.Fatalf("seed storage: %v", err)
	}
	return st, key
}

// imageRow builds a db.Image describing a stored PNG original.
func imageRow(id uuid.UUID, key string, w, h, size int) db.Image {
	return db.Image{
		ID:               id,
		OriginalFilename: "photo.png",
		ContentHash:      "deadbeef",
		MimeType:         "image/png",
		Width:            int32(w),
		Height:           int32(h),
		SizeBytes:        int64(size),
		StorageKey:       key,
		CreatedAt:        pgtype.Timestamptz{Valid: true},
	}
}

func imageConfig() config.Config {
	cfg := testConfig()
	cfg.CacheControlMaxAge = 3600
	return cfg
}

func TestGetImageOriginal(t *testing.T) {
	data := pngBytes(t, 8, 6)
	st, key := storeOriginal(t, data)
	id := uuid.New()
	row := imageRow(id, key, 8, 6, len(data))
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}

	h := newUploadHarness(t, imageConfig(), images, nil, st)
	status, header, body := h.get(t, "/v1/images/"+id.String(), true)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if ct := header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if !bytes.Equal(body, data) {
		t.Errorf("body length = %d, want the stored %d bytes verbatim", len(body), len(data))
	}
	if cc := header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=3600") {
		t.Errorf("Cache-Control = %q, want public/immutable with max-age=3600", cc)
	}
}

func TestGetImageTransformed(t *testing.T) {
	data := pngBytes(t, 40, 20)
	st, key := storeOriginal(t, data)
	id := uuid.New()
	row := imageRow(id, key, 40, 20, len(data))
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}

	h := newUploadHarness(t, imageConfig(), images, nil, st)

	// A format conversion produces a derivative, not the original bytes.
	status, header, body := h.get(t, "/v1/images/"+id.String()+"?w=20&fmt=jpeg", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if ct := header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if bytes.Equal(body, data) {
		t.Error("transformed body equals the original; expected a re-encoded derivative")
	}
	if len(body) == 0 {
		t.Error("transformed body is empty")
	}
	if cc := header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}

func TestGetImageErrors(t *testing.T) {
	data := pngBytes(t, 8, 6)

	tests := []struct {
		name       string
		path       string
		images     func(st storage.Storage, key string, id uuid.UUID) *fakeImageStore
		seedStore  bool // when false, the storage has no object at the key
		corruptSrc bool // when true, the stored "original" is not a valid image
		withKey    bool
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown id",
			path:       "/v1/images/" + uuid.New().String(),
			images:     func(storage.Storage, string, uuid.UUID) *fakeImageStore { return &fakeImageStore{} },
			seedStore:  true,
			withKey:    true,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name: "db lookup failure",
			path: "/v1/images/" + uuid.New().String(),
			images: func(storage.Storage, string, uuid.UUID) *fakeImageStore {
				return &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) {
					return db.Image{}, errors.New("db down")
				}}
			},
			seedStore:  true,
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name: "invalid transform param",
			path: "/v1/images/PLACEHOLDER?w=0",
			images: func(_ storage.Storage, key string, id uuid.UUID) *fakeImageStore {
				row := imageRow(id, key, 8, 6, 10)
				return &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
			},
			seedStore:  true,
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name: "invalid format param",
			path: "/v1/images/PLACEHOLDER?fmt=gif",
			images: func(_ storage.Storage, key string, id uuid.UUID) *fakeImageStore {
				row := imageRow(id, key, 8, 6, 10)
				return &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
			},
			seedStore:  true,
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name: "original bytes missing from storage",
			path: "/v1/images/PLACEHOLDER",
			images: func(_ storage.Storage, key string, id uuid.UUID) *fakeImageStore {
				row := imageRow(id, key, 8, 6, 10)
				return &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
			},
			seedStore:  false,
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name: "original bytes missing on transform",
			path: "/v1/images/PLACEHOLDER?w=4",
			images: func(_ storage.Storage, key string, id uuid.UUID) *fakeImageStore {
				row := imageRow(id, key, 8, 6, 10)
				return &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
			},
			seedStore:  false,
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name: "stored original is not a valid image on transform",
			path: "/v1/images/PLACEHOLDER?w=4",
			images: func(_ storage.Storage, key string, id uuid.UUID) *fakeImageStore {
				row := imageRow(id, key, 8, 6, 10)
				return &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
			},
			seedStore:  true,
			corruptSrc: true,
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name:       "missing api key",
			path:       "/v1/images/" + uuid.New().String(),
			images:     func(storage.Storage, string, uuid.UUID) *fakeImageStore { return &fakeImageStore{} },
			seedStore:  true,
			withKey:    false,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := uuid.New()
			srcData := data
			if tt.corruptSrc {
				srcData = []byte("not an image")
			}

			st, err := storage.NewLocal(t.TempDir())
			if err != nil {
				t.Fatalf("storage.NewLocal: %v", err)
			}
			key := "originals/aa/bb/deadbeef"
			if tt.seedStore {
				if err := st.Put(context.Background(), key, bytes.NewReader(srcData), "image/png"); err != nil {
					t.Fatalf("seed storage: %v", err)
				}
			}

			images := tt.images(st, key, id)
			h := newUploadHarness(t, imageConfig(), images, nil, st)

			path := strings.Replace(tt.path, "PLACEHOLDER", id.String(), 1)
			status, _, body := h.get(t, path, tt.withKey)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, body)
			}
			if e := decodeErrorResp(t, body); e.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

// TestGetImageTransformNonWebSourceRequiresFmt covers the guard that a source
// in a non-web format (heic/heif/avif/tiff) cannot be transformed without an
// explicit fmt, since the server never re-encodes to those formats. The guard
// fires on the stored MIME type before storage or libvips is touched.
func TestGetImageTransformNonWebSourceRequiresFmt(t *testing.T) {
	for _, mime := range []string{"image/heic", "image/heif", "image/avif", "image/tiff"} {
		t.Run(mime, func(t *testing.T) {
			id := uuid.New()
			row := imageRow(id, "originals/aa/bb/deadbeef", 128, 64, 100)
			row.MimeType = mime
			images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}

			h := newUploadHarness(t, imageConfig(), images, nil, nil)
			status, _, body := h.get(t, "/v1/images/"+id.String()+"?w=16", true)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body: %s)", status, http.StatusBadRequest, body)
			}
			if e := decodeErrorResp(t, body); e.Code != "bad_request" {
				t.Errorf("error code = %q, want bad_request", e.Code)
			}
		})
	}
}

func TestGetImageInvalidUUID(t *testing.T) {
	h := newUploadHarness(t, imageConfig(), &fakeImageStore{}, nil, nil)
	status, _, body := h.get(t, "/v1/images/not-a-uuid", true)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if e := decodeErrorResp(t, body); e.Code != "bad_request" {
		t.Errorf("error code = %q, want bad_request", e.Code)
	}
}

func TestGetImageMeta(t *testing.T) {
	id := uuid.New()
	row := imageRow(id, "originals/aa/bb/deadbeef", 8, 6, 42)
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}

	h := newUploadHarness(t, imageConfig(), images, nil, nil)
	status, header, body := h.get(t, "/v1/images/"+id.String()+"/meta", true)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if cc := header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	img := decodeImage(t, body)
	if img.Id != id {
		t.Errorf("id = %v, want %v", img.Id, id)
	}
	if img.Width != 8 || img.Height != 6 || img.SizeBytes != 42 {
		t.Errorf("meta = %dx%d %dB, want 8x6 42B", img.Width, img.Height, img.SizeBytes)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime_type = %q, want image/png", img.MimeType)
	}
}

func TestGetImageMetaErrors(t *testing.T) {
	tests := []struct {
		name       string
		images     *fakeImageStore
		withKey    bool
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown id",
			images:     &fakeImageStore{},
			withKey:    true,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name: "db lookup failure",
			images: &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) {
				return db.Image{}, errors.New("db down")
			}},
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name:       "missing api key",
			images:     &fakeImageStore{},
			withKey:    false,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newUploadHarness(t, imageConfig(), tt.images, nil, nil)
			status, _, body := h.get(t, "/v1/images/"+uuid.New().String()+"/meta", tt.withKey)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if e := decodeErrorResp(t, body); e.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

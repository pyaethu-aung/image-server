package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/config"
	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/fetch"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// fakeImageStore satisfies imageStore with injectable behavior. The zero
// value behaves like an empty database.
type fakeImageStore struct {
	getByHash   func(hash string) (db.Image, error)
	getByID     func(id uuid.UUID) (db.Image, error)
	create      func(arg db.CreateImageParams) (db.Image, error)
	createCalls int
	lastCreate  db.CreateImageParams
}

func (f *fakeImageStore) GetImageByContentHash(_ context.Context, hash string) (db.Image, error) {
	if f.getByHash != nil {
		return f.getByHash(hash)
	}
	return db.Image{}, pgx.ErrNoRows
}

func (f *fakeImageStore) CreateImage(_ context.Context, arg db.CreateImageParams) (db.Image, error) {
	f.createCalls++
	f.lastCreate = arg
	if f.create != nil {
		return f.create(arg)
	}
	return imageFromParams(arg), nil
}

func (f *fakeImageStore) GetImage(_ context.Context, id uuid.UUID) (db.Image, error) {
	if f.getByID != nil {
		return f.getByID(id)
	}
	return db.Image{}, pgx.ErrNoRows
}

func (f *fakeImageStore) DeleteImage(_ context.Context, _ uuid.UUID) (db.Image, error) {
	return db.Image{}, pgx.ErrNoRows
}

func imageFromParams(arg db.CreateImageParams) db.Image {
	return db.Image{
		ID:               uuid.New(),
		OriginalFilename: arg.OriginalFilename,
		ContentHash:      arg.ContentHash,
		MimeType:         arg.MimeType,
		Width:            arg.Width,
		Height:           arg.Height,
		SizeBytes:        arg.SizeBytes,
		StorageKey:       arg.StorageKey,
		CreatedAt:        pgtype.Timestamptz{Valid: true},
	}
}

// fakeFetcher satisfies imageFetcher.
type fakeFetcher struct {
	fetchFn func(url string) ([]byte, error)
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	return f.fetchFn(url)
}

// failingStorage errors on writes to drive the 500 path.
type failingStorage struct{ storage.Storage }

func (failingStorage) Put(context.Context, string, io.Reader, string) error {
	return errors.New("disk on fire")
}

// uploadHarness bundles the pieces a handler test needs to assert on.
type uploadHarness struct {
	srv    *httptest.Server
	store  storage.Storage
	images *fakeImageStore
}

func newUploadHarness(t *testing.T, cfg config.Config, images *fakeImageStore, fetcher imageFetcher, st storage.Storage) *uploadHarness {
	t.Helper()
	if st == nil {
		local, err := storage.NewLocal(t.TempDir())
		if err != nil {
			t.Fatalf("storage.NewLocal: %v", err)
		}
		st = local
	}
	rdb, _ := newTestRedis(t)
	srv := httptest.NewServer(NewRouter(NewServer(cfg, st, images, rdb, fetcher)))
	t.Cleanup(srv.Close)
	return &uploadHarness{srv: srv, store: st, images: images}
}

// pngBytes returns an encoded w x h PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// multipartBody builds a multipart body with one file field.
func multipartBody(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// post sends an authenticated (or not) POST and returns the status code and
// the fully read, closed response body.
func (h *uploadHarness) post(t *testing.T, path, contentType string, body io.Reader, withKey bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if withKey {
		req.Header.Set("X-API-Key", testAPIKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, respBody
}

func decodeImage(t *testing.T, body []byte) gen.Image {
	t.Helper()
	var img gen.Image
	if err := json.Unmarshal(body, &img); err != nil {
		t.Fatalf("decode image body: %v", err)
	}
	return img
}

func decodeErrorResp(t *testing.T, body []byte) gen.Error {
	t.Helper()
	var e gen.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e
}

func TestUploadImageHappyPath(t *testing.T) {
	data := pngBytes(t, 4, 5)
	sum := sha256.Sum256(data)
	wantHash := hex.EncodeToString(sum[:])

	h := newUploadHarness(t, testConfig(), &fakeImageStore{}, nil, nil)
	body, ct := multipartBody(t, "file", "test.png", data)
	status, respBody := h.post(t, "/v1/images", ct, body, true)

	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	img := decodeImage(t, respBody)
	if img.ContentHash != wantHash {
		t.Errorf("content_hash = %q, want %q", img.ContentHash, wantHash)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime_type = %q, want image/png", img.MimeType)
	}
	if img.Width != 4 || img.Height != 5 {
		t.Errorf("dims = %dx%d, want 4x5", img.Width, img.Height)
	}
	if img.SizeBytes != int64(len(data)) {
		t.Errorf("size_bytes = %d, want %d", img.SizeBytes, len(data))
	}
	if img.OriginalFilename != "test.png" {
		t.Errorf("original_filename = %q, want test.png", img.OriginalFilename)
	}

	wantKey := "originals/" + wantHash[:2] + "/" + wantHash[2:4] + "/" + wantHash
	exists, err := h.store.Exists(t.Context(), wantKey)
	if err != nil || !exists {
		t.Errorf("stored object at %q: exists=%v err=%v", wantKey, exists, err)
	}
	if h.images.lastCreate.StorageKey != wantKey {
		t.Errorf("recorded storage_key = %q, want %q", h.images.lastCreate.StorageKey, wantKey)
	}
}

func TestUploadImageDedupReturnsExisting(t *testing.T) {
	data := pngBytes(t, 2, 2)
	existing := db.Image{ID: uuid.New(), ContentHash: "whatever", MimeType: "image/png",
		Width: 2, Height: 2, SizeBytes: 9, CreatedAt: pgtype.Timestamptz{Valid: true}}
	images := &fakeImageStore{getByHash: func(string) (db.Image, error) { return existing, nil }}

	h := newUploadHarness(t, testConfig(), images, nil, nil)
	body, ct := multipartBody(t, "file", "dup.png", data)
	status, respBody := h.post(t, "/v1/images", ct, body, true)

	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if img := decodeImage(t, respBody); img.Id != existing.ID {
		t.Errorf("id = %v, want existing %v", img.Id, existing.ID)
	}
	if images.createCalls != 0 {
		t.Errorf("CreateImage called %d times on dedup hit, want 0", images.createCalls)
	}
}

func TestUploadImageUniqueViolationRace(t *testing.T) {
	data := pngBytes(t, 2, 2)
	existing := db.Image{ID: uuid.New(), MimeType: "image/png", CreatedAt: pgtype.Timestamptz{Valid: true}}
	calls := 0
	images := &fakeImageStore{
		getByHash: func(string) (db.Image, error) {
			calls++
			if calls == 1 {
				return db.Image{}, pgx.ErrNoRows // pre-insert dedup check misses
			}
			return existing, nil // post-conflict re-fetch finds the winner
		},
		create: func(db.CreateImageParams) (db.Image, error) {
			return db.Image{}, &pgconn.PgError{Code: uniqueViolation}
		},
	}

	h := newUploadHarness(t, testConfig(), images, nil, nil)
	body, ct := multipartBody(t, "file", "race.png", data)
	status, respBody := h.post(t, "/v1/images", ct, body, true)

	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if img := decodeImage(t, respBody); img.Id != existing.ID {
		t.Errorf("id = %v, want winner's %v", img.Id, existing.ID)
	}
}

func TestUploadImageErrors(t *testing.T) {
	smallCfg := testConfig()
	smallCfg.MaxUploadBytes = 64

	bombCfg := testConfig()
	bombCfg.MaxPixels = 5 // a 4x5 image (20 px) exceeds this

	tests := []struct {
		name       string
		cfg        config.Config
		images     *fakeImageStore
		st         storage.Storage
		body       func(t *testing.T) (io.Reader, string)
		withKey    bool
		wantStatus int
		wantCode   string
	}{
		{
			name:   "missing api key",
			images: &fakeImageStore{},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
				return b, ct
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:   "missing file field",
			images: &fakeImageStore{},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "not-file", "a.png", pngBytes(t, 1, 1))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:   "malformed multipart body",
			images: &fakeImageStore{},
			body: func(*testing.T) (io.Reader, string) {
				return strings.NewReader("not multipart at all"), "multipart/form-data; boundary=xyz"
			},
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:   "oversized upload",
			cfg:    smallCfg,
			images: &fakeImageStore{},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "big.png", pngBytes(t, 50, 50))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "payload_too_large",
		},
		{
			name:   "not an image",
			images: &fakeImageStore{},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "a.txt", []byte("plain text"))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   "unsupported_media_type",
		},
		{
			name:   "decompression bomb",
			cfg:    bombCfg,
			images: &fakeImageStore{},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "bomb.png", pngBytes(t, 4, 5))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name: "dedup lookup failure",
			images: &fakeImageStore{getByHash: func(string) (db.Image, error) {
				return db.Image{}, errors.New("db down")
			}},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name:   "storage write failure",
			images: &fakeImageStore{},
			st:     failingStorage{},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name: "create failure that is not a unique violation",
			images: &fakeImageStore{create: func(db.CreateImageParams) (db.Image, error) {
				return db.Image{}, errors.New("insert failed")
			}},
			body: func(t *testing.T) (io.Reader, string) {
				b, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
				return b, ct
			},
			withKey:    true,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if cfg.APIKey == "" {
				cfg = testConfig()
			}
			h := newUploadHarness(t, cfg, tt.images, nil, tt.st)
			body, ct := tt.body(t)
			status, respBody := h.post(t, "/v1/images", ct, body, tt.withKey)

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if e := decodeErrorResp(t, respBody); e.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

func TestUploadImageFromURLHappyPath(t *testing.T) {
	data := pngBytes(t, 3, 3)
	fetcher := &fakeFetcher{fetchFn: func(url string) ([]byte, error) {
		if url != "https://example.com/photos/pic.png" {
			return nil, fmt.Errorf("unexpected url %q", url)
		}
		return data, nil
	}}
	images := &fakeImageStore{}

	h := newUploadHarness(t, testConfig(), images, fetcher, nil)
	status, respBody := h.post(t, "/v1/images/from-url", "application/json",
		strings.NewReader(`{"url":"https://example.com/photos/pic.png"}`), true)

	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	img := decodeImage(t, respBody)
	if img.MimeType != "image/png" {
		t.Errorf("mime_type = %q, want image/png", img.MimeType)
	}
	if img.OriginalFilename != "pic.png" {
		t.Errorf("original_filename = %q, want pic.png (from URL path)", img.OriginalFilename)
	}
}

func TestUploadImageFromURLErrors(t *testing.T) {
	tests := []struct {
		name       string
		fetcher    imageFetcher
		body       string
		withKey    bool
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing api key",
			fetcher:    &fakeFetcher{fetchFn: func(string) ([]byte, error) { return nil, nil }},
			body:       `{"url":"https://example.com/a.png"}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "invalid JSON",
			fetcher:    &fakeFetcher{fetchFn: func(string) ([]byte, error) { return nil, nil }},
			body:       `{"url":`,
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:       "empty url",
			fetcher:    &fakeFetcher{fetchFn: func(string) ([]byte, error) { return nil, nil }},
			body:       `{"url":""}`,
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name: "blocked destination",
			fetcher: &fakeFetcher{fetchFn: func(string) ([]byte, error) {
				return nil, fmt.Errorf("dial: %w", fetch.ErrBlockedAddress)
			}},
			body:       `{"url":"http://169.254.169.254/latest/meta-data"}`,
			withKey:    true,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name: "remote file too large",
			fetcher: &fakeFetcher{fetchFn: func(string) ([]byte, error) {
				return nil, fmt.Errorf("read: %w", fetch.ErrTooLarge)
			}},
			body:       `{"url":"https://example.com/huge.png"}`,
			withKey:    true,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "payload_too_large",
		},
		{
			name: "remote content is not an image",
			fetcher: &fakeFetcher{fetchFn: func(string) ([]byte, error) {
				return []byte("<html>hello</html>"), nil
			}},
			body:       `{"url":"https://example.com/page.html"}`,
			withKey:    true,
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   "unsupported_media_type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newUploadHarness(t, testConfig(), &fakeImageStore{}, tt.fetcher, nil)
			status, respBody := h.post(t, "/v1/images/from-url", "application/json",
				strings.NewReader(tt.body), tt.withKey)

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if e := decodeErrorResp(t, respBody); e.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

func TestFilenameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://example.com/photos/pic.png", "pic.png"},
		{"https://example.com/", ""},
		{"https://example.com", ""},
		{"https://example.com/a/b/c.jpg?w=100", "c.jpg"},
		{"://bad", ""},
	}
	for _, tt := range tests {
		if got := filenameFromURL(tt.url); got != tt.want {
			t.Errorf("filenameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

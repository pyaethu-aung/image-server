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

	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// spyStorage counts Get calls per key so tests can prove a cache hit never
// re-reads the original.
type spyStorage struct {
	storage.Storage
	gets []string
}

func (s *spyStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.gets = append(s.gets, key)
	return s.Storage.Get(ctx, key)
}

func (s *spyStorage) countGets(key string) int {
	n := 0
	for _, k := range s.gets {
		if k == key {
			n++
		}
	}
	return n
}

// brokenCleanupStorage errors on the calls DeleteImage makes after the DB
// delete, to prove cleanup is best-effort.
type brokenCleanupStorage struct{ storage.Storage }

func (brokenCleanupStorage) Delete(context.Context, string) error {
	return errors.New("delete broken")
}

func (brokenCleanupStorage) List(context.Context, string) ([]string, error) {
	return nil, errors.New("list broken")
}

// del sends an authenticated (or not) DELETE and returns the status and body.
func (h *uploadHarness) del(t *testing.T, path string, withKey bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if withKey {
		req.Header.Set("X-API-Key", testAPIKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, body
}

// newCacheHarness seeds one stored PNG original behind a spy and returns the
// harness, the spy, the image id, and the original's storage key.
func newCacheHarness(t *testing.T) (*uploadHarness, *spyStorage, uuid.UUID, string) {
	t.Helper()
	data := pngBytes(t, 40, 20)
	st, key := storeOriginal(t, data)
	spy := &spyStorage{Storage: st}
	id := uuid.New()
	row := imageRow(id, key, 40, 20, len(data))
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
	h := newUploadHarness(t, imageConfig(), images, nil, spy)
	return h, spy, id, key
}

func TestGetImageCacheMissGeneratesAndRemembers(t *testing.T) {
	h, _, id, _ := newCacheHarness(t)

	// No fmt param: the derivative keeps the source type, so the key ends .png.
	status, header, _ := h.get(t, "/v1/images/"+id.String()+"?w=20", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if ct := header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}

	keys, err := h.store.List(t.Context(), "derivatives/"+id.String()+"/")
	if err != nil {
		t.Fatalf("List derivatives: %v", err)
	}
	if len(keys) != 1 || !strings.HasSuffix(keys[0], ".png") {
		t.Errorf("stored derivatives = %v, want one .png key", keys)
	}

	markers := h.mr.Keys()
	var haveMarker, haveSet bool
	for _, k := range markers {
		if strings.HasPrefix(k, "imgcache:"+id.String()+":") {
			haveMarker = true
		}
		if k == "imgderivs:"+id.String() {
			haveSet = true
		}
	}
	if !haveMarker || !haveSet {
		t.Errorf("redis keys = %v, want an imgcache marker and the imgderivs set", markers)
	}
}

func TestGetImageCacheHitSkipsRegeneration(t *testing.T) {
	h, spy, id, origKey := newCacheHarness(t)
	path := "/v1/images/" + id.String() + "?w=20&fmt=webp"

	_, _, first := h.get(t, path, true)
	status, header, second := h.get(t, path, true)
	if status != http.StatusOK {
		t.Fatalf("second status = %d, want %d", status, http.StatusOK)
	}
	if ct := header.Get("Content-Type"); ct != "image/webp" {
		t.Errorf("cache-hit Content-Type = %q, want image/webp", ct)
	}
	if !bytes.Equal(first, second) {
		t.Error("cache hit returned different bytes than the generating request")
	}
	if n := spy.countGets(origKey); n != 1 {
		t.Errorf("original fetched %d times across two requests, want 1 (hit must not regenerate)", n)
	}
}

func TestGetImageCacheRepairAfterRedisLoss(t *testing.T) {
	h, spy, id, origKey := newCacheHarness(t)
	path := "/v1/images/" + id.String() + "?w=20"

	h.get(t, path, true)
	h.mr.FlushAll() // Redis evicted everything; storage still has the object.

	status, _, _ := h.get(t, path, true)
	if status != http.StatusOK {
		t.Fatalf("status after flush = %d, want %d", status, http.StatusOK)
	}
	if n := spy.countGets(origKey); n != 1 {
		t.Errorf("original fetched %d times, want 1 (repair must serve from storage)", n)
	}
	// The repair must also repopulate the marker.
	var haveMarker bool
	for _, k := range h.mr.Keys() {
		if strings.HasPrefix(k, "imgcache:"+id.String()+":") {
			haveMarker = true
		}
	}
	if !haveMarker {
		t.Error("marker not repaired after storage hit")
	}
}

func TestGetImageStaleMarkerRegenerates(t *testing.T) {
	h, spy, id, origKey := newCacheHarness(t)
	path := "/v1/images/" + id.String() + "?w=20"

	h.get(t, path, true)

	// Storage loses the object but Redis still remembers it: the marker is a
	// lie and the request must fall through to regeneration, not 500.
	keys, err := h.store.List(t.Context(), "derivatives/"+id.String()+"/")
	if err != nil || len(keys) != 1 {
		t.Fatalf("List derivatives = %v, %v; want one key", keys, err)
	}
	if err := h.store.Delete(t.Context(), keys[0]); err != nil {
		t.Fatalf("delete derivative: %v", err)
	}

	status, _, _ := h.get(t, path, true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if n := spy.countGets(origKey); n != 2 {
		t.Errorf("original fetched %d times, want 2 (stale marker must regenerate)", n)
	}
}

func TestGetImageRedisDownFailsOpen(t *testing.T) {
	h, spy, id, origKey := newCacheHarness(t)
	path := "/v1/images/" + id.String() + "?w=20"
	h.mr.Close() // every Redis call now errors; rate limit and cache fail open

	status, _, _ := h.get(t, path, true)
	if status != http.StatusOK {
		t.Fatalf("first status with redis down = %d, want %d", status, http.StatusOK)
	}
	// Second request: the marker is unreachable, but storage.Exists finds the
	// derivative, so it is served without regeneration.
	status, _, _ = h.get(t, path, true)
	if status != http.StatusOK {
		t.Fatalf("second status with redis down = %d, want %d", status, http.StatusOK)
	}
	if n := spy.countGets(origKey); n != 1 {
		t.Errorf("original fetched %d times, want 1 (storage.Exists must cover for redis)", n)
	}
}

func TestDeleteImagePurges(t *testing.T) {
	data := pngBytes(t, 40, 20)
	st, origKey := storeOriginal(t, data)
	id := uuid.New()
	row := imageRow(id, origKey, 40, 20, len(data))
	images := &fakeImageStore{
		getByID:    func(uuid.UUID) (db.Image, error) { return row, nil },
		deleteByID: func(uuid.UUID) (db.Image, error) { return row, nil },
	}
	h := newUploadHarness(t, imageConfig(), images, nil, st)

	// Materialize two derivatives (and their Redis state) before deleting.
	h.get(t, "/v1/images/"+id.String()+"?w=20", true)
	h.get(t, "/v1/images/"+id.String()+"?w=10&fmt=webp", true)

	status, _ := h.del(t, "/v1/images/"+id.String(), true)
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", status, http.StatusNoContent)
	}

	if exists, err := h.store.Exists(t.Context(), origKey); err != nil || exists {
		t.Errorf("original after delete: exists=%v err=%v, want gone", exists, err)
	}
	keys, err := h.store.List(t.Context(), "derivatives/"+id.String()+"/")
	if err != nil || len(keys) != 0 {
		t.Errorf("derivatives after delete = %v (err %v), want none", keys, err)
	}
	for _, k := range h.mr.Keys() {
		if strings.Contains(k, id.String()) {
			t.Errorf("redis key %q survived the purge", k)
		}
	}
}

func TestDeleteImageErrors(t *testing.T) {
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
			name: "db failure",
			images: &fakeImageStore{deleteByID: func(uuid.UUID) (db.Image, error) {
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
			status, body := h.del(t, "/v1/images/"+uuid.New().String(), tt.withKey)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if e := decodeErrorResp(t, body); e.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

func TestDeleteImageCleanupIsBestEffort(t *testing.T) {
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}
	id := uuid.New()
	row := imageRow(id, "originals/aa/bb/deadbeef", 8, 6, 10)
	images := &fakeImageStore{deleteByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
	h := newUploadHarness(t, imageConfig(), images, nil, brokenCleanupStorage{st})

	// Storage cleanup fails across the board; the DB row is gone, so the
	// client still gets 204.
	status, _ := h.del(t, "/v1/images/"+id.String(), true)
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want %d despite cleanup failures", status, http.StatusNoContent)
	}
}

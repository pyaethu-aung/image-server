package api

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// gpsMarker is a recognisable payload embedded in EXIF so tests can assert a
// strip removed it.
const gpsMarker = "GPS-51.5-0.12-secret"

// jpegWithExif builds a w x h JPEG with an APP1 EXIF segment carrying
// gpsMarker, so a strip can be observed removing it.
func jpegWithExif(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	j := buf.Bytes()
	exif := append([]byte("Exif\x00\x00"), []byte(gpsMarker)...)
	seg := []byte{0xFF, 0xE1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}
	seg = append(seg, exif...)
	out := append([]byte{}, j[:2]...)
	out = append(out, seg...)
	return append(out, j[2:]...)
}

// seedImage stores data as an original and returns the storage, a matching
// db.Image row, and its id.
func seedImage(t *testing.T, data []byte, mime string, w, h int) (storage.Storage, uuid.UUID, db.Image) {
	t.Helper()
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}
	key := "originals/aa/bb/deadbeef"
	if err := st.Put(context.Background(), key, bytes.NewReader(data), mime); err != nil {
		t.Fatalf("seed storage: %v", err)
	}
	id := uuid.New()
	row := db.Image{
		ID:               id,
		OriginalFilename: "photo",
		ContentHash:      "deadbeef",
		MimeType:         mime,
		Width:            int32(w),
		Height:           int32(h),
		SizeBytes:        int64(len(data)),
		StorageKey:       key,
		CreatedAt:        pgtype.Timestamptz{Valid: true},
	}
	return st, id, row
}

func TestGetImageStripOnlyLossless(t *testing.T) {
	data := jpegWithExif(t, 24, 18)
	if !bytes.Contains(data, []byte(gpsMarker)) {
		t.Fatal("test setup: original JPEG missing EXIF marker")
	}
	st, id, row := seedImage(t, data, "image/jpeg", 24, 18)
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
	h := newUploadHarness(t, imageConfig(), images, nil, st)

	status, header, body := h.get(t, "/v1/images/"+id.String()+"?strip=true", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if ct := header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if bytes.Contains(body, []byte(gpsMarker)) || bytes.Contains(body, []byte("Exif")) {
		t.Error("stripped response still contains EXIF metadata")
	}
	if len(body) >= len(data) {
		t.Errorf("stripped body (%d) should be smaller than the metadata-laden original (%d)", len(body), len(data))
	}
	if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
		t.Errorf("stripped body does not decode as JPEG: %v", err)
	}
	if !strings.Contains(header.Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", header.Get("Cache-Control"))
	}

	// The derivative is cached: a second identical request is byte-for-byte
	// equal, and a derivative object exists in storage.
	_, _, body2 := h.get(t, "/v1/images/"+id.String()+"?strip=true", true)
	if !bytes.Equal(body, body2) {
		t.Error("second strip request is not byte-identical (cache miss?)")
	}
	keys, err := st.List(context.Background(), "derivatives/"+id.String()+"/")
	if err != nil {
		t.Fatalf("list derivatives: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("derivative count = %d, want 1", len(keys))
	}
}

func TestGetImageStripWithResize(t *testing.T) {
	data := jpegWithExif(t, 40, 30)
	st, id, row := seedImage(t, data, "image/jpeg", 40, 30)
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
	h := newUploadHarness(t, imageConfig(), images, nil, st)

	// Resize + strip: libvips re-encodes and drops metadata in one pass.
	status, header, body := h.get(t, "/v1/images/"+id.String()+"?w=20&strip=true", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if ct := header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if bytes.Contains(body, []byte(gpsMarker)) {
		t.Error("resize+strip response still contains EXIF marker")
	}
	if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
		t.Errorf("body does not decode as JPEG: %v", err)
	}
}

func TestGetImageStripOnlyUnsupportedFormat(t *testing.T) {
	for _, mime := range []string{"image/webp", "image/gif"} {
		t.Run(mime, func(t *testing.T) {
			// Storage bytes are irrelevant: the 415 fires before any read.
			st, id, row := seedImage(t, []byte("dummy"), mime, 10, 10)
			images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
			h := newUploadHarness(t, imageConfig(), images, nil, st)

			status, _, body := h.get(t, "/v1/images/"+id.String()+"?strip=true", true)
			if status != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415 (body: %s)", status, body)
			}
			if !bytes.Contains(body, []byte("unsupported_media_type")) {
				t.Errorf("body = %s, want an unsupported_media_type error", body)
			}
		})
	}
}

func TestGetImageStripFalseServesOriginal(t *testing.T) {
	// strip=false is unset: the request is identity and serves the stored
	// original verbatim, metadata intact.
	data := jpegWithExif(t, 12, 12)
	st, id, row := seedImage(t, data, "image/jpeg", 12, 12)
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
	h := newUploadHarness(t, imageConfig(), images, nil, st)

	status, _, body := h.get(t, "/v1/images/"+id.String()+"?strip=false", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !bytes.Equal(body, data) {
		t.Error("strip=false should serve the original bytes verbatim")
	}
}

func TestGetImageStripBadValue(t *testing.T) {
	st, id, row := seedImage(t, jpegWithExif(t, 10, 10), "image/jpeg", 10, 10)
	images := &fakeImageStore{getByID: func(uuid.UUID) (db.Image, error) { return row, nil }}
	h := newUploadHarness(t, imageConfig(), images, nil, st)

	status, _, _ := h.get(t, "/v1/images/"+id.String()+"?strip=maybe", true)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid strip value", status)
	}
}

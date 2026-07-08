//go:build apitest

package api

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pyaethu-aung/image-server/internal/config"
)

// readImageprocFixture loads a real heic/avif/tiff fixture from the imageproc
// package's testdata (the stdlib encoders cannot produce these formats).
func readImageprocFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "imageproc", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// gifBytes builds a small GIF, a format with no lossless strip support.
func gifBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, m, nil); err != nil {
		t.Fatalf("gif.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestAPIHealthz(t *testing.T) {
	h := newAPIHarness(t, nil)
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, "/healthz", "", nil, false))
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
}

func TestAPIUploadImageMultipart(t *testing.T) {
	h := newAPIHarness(t, nil)
	data := pngBytes(t, 3, 2)

	body, ct := multipartBody(t, "file", "photo.png", data)
	status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
	}
	first := decodeImage(t, respBody)
	if first.MimeType != "image/png" || first.Width != 3 || first.Height != 2 {
		t.Errorf("image = %+v, want 3x2 image/png", first)
	}

	// Byte-identical re-upload dedups to the same record.
	body2, ct2 := multipartBody(t, "file", "photo-again.png", data)
	status2, _, respBody2 := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct2, body2, true))
	if status2 != http.StatusCreated {
		t.Fatalf("dedup status = %d, want %d", status2, http.StatusCreated)
	}
	if second := decodeImage(t, respBody2); second.Id != first.Id {
		t.Errorf("dedup id = %v, want %v", second.Id, first.Id)
	}
}

func TestAPIUploadImageUnauthorized(t *testing.T) {
	h := newAPIHarness(t, nil)
	body, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestAPIUploadImageUnsupportedType(t *testing.T) {
	h := newAPIHarness(t, nil)
	body, ct := multipartBody(t, "file", "a.txt", []byte("not an image"))
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", status, http.StatusUnsupportedMediaType)
	}
}

func TestAPIUploadImageTooLarge(t *testing.T) {
	h := newAPIHarness(t, func(cfg *config.Config) { cfg.MaxUploadBytes = 64 })
	body, ct := multipartBody(t, "file", "big.png", pngBytes(t, 50, 50))
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}
}

func TestAPIUploadImageRateLimited(t *testing.T) {
	h := newAPIHarness(t, func(cfg *config.Config) { cfg.RateLimitPerMin = 1 })

	body, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", status, http.StatusCreated)
	}

	body2, ct2 := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
	status2, header, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct2, body2, true))
	if status2 != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", status2, http.StatusTooManyRequests)
	}
	if header.Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
}

// TestAPIUploadFromURLBlocked exercises the REAL SSRF guard end-to-end:
// metadata and loopback destinations must be rejected before any dial. The
// from-url happy path is covered by the unit tests with a fake fetcher; the
// real guard correctly refuses to fetch from this test's own loopback
// server, so a live 201 here would require real egress (intentional split).
func TestAPIUploadFromURLBlocked(t *testing.T) {
	h := newAPIHarness(t, nil)

	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost/image.png",
		"ftp://example.com/image.png",
	} {
		req := h.newReq(t, http.MethodPost, "/v1/images/from-url", "application/json",
			strings.NewReader(`{"url":"`+target+`"}`), true)
		status, _, _ := h.doValidated(t, req)
		if status != http.StatusBadRequest {
			t.Errorf("POST from-url %q status = %d, want %d", target, status, http.StatusBadRequest)
		}
	}
}

// uploadPNG uploads a fresh PNG and returns its id, validated against the spec.
func (h *apiHarness) uploadPNG(t *testing.T, w, hgt int) uuid.UUID {
	t.Helper()
	body, ct := multipartBody(t, "file", "photo.png", pngBytes(t, w, hgt))
	status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
	}
	return decodeImage(t, respBody).Id
}

func TestAPIGetImageOriginalAndTransform(t *testing.T) {
	h := newAPIHarness(t, nil)
	data := pngBytes(t, 60, 40)
	body, ct := multipartBody(t, "file", "photo.png", data)
	status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
	}
	id := decodeImage(t, respBody).Id.String()

	// No params: the stored original is returned verbatim.
	status, header, orig := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id, "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("get original status = %d, want %d", status, http.StatusOK)
	}
	if ctype := header.Get("Content-Type"); ctype != "image/png" {
		t.Errorf("original Content-Type = %q, want image/png", ctype)
	}
	if !bytes.Equal(orig, data) {
		t.Error("original body does not match the uploaded bytes")
	}
	if header.Get("Cache-Control") == "" {
		t.Error("original response missing Cache-Control")
	}

	// With params: a resized WebP derivative.
	status, header, deriv := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id+"?w=30&fmt=webp", "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("get derivative status = %d, want %d", status, http.StatusOK)
	}
	if ctype := header.Get("Content-Type"); ctype != "image/webp" {
		t.Errorf("derivative Content-Type = %q, want image/webp", ctype)
	}
	if len(deriv) == 0 {
		t.Error("derivative body is empty")
	}
	if header.Get("Cache-Control") == "" {
		t.Error("derivative response missing Cache-Control")
	}
}

// TestAPIUploadAndTransformNewFormats exercises the full stack for the newly
// accepted input formats (heic/avif/tiff): upload is accepted with the real
// detected type, the original serves back unchanged, a transform without an
// explicit fmt is rejected (the server never re-encodes to these formats), and
// a transform with fmt=jpeg decodes via libvips and returns a web-safe jpeg.
func TestAPIUploadAndTransformNewFormats(t *testing.T) {
	h := newAPIHarness(t, nil)
	cases := []struct {
		file  string
		mime  string
		w, ht int
	}{
		{"sample.heic", "image/heic", 128, 64},
		{"sample.avif", "image/avif", 32, 24},
		{"sample.tiff", "image/tiff", 32, 24},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data := readImageprocFixture(t, tc.file)
			body, ct := multipartBody(t, "file", tc.file, data)
			status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
			if status != http.StatusCreated {
				t.Fatalf("upload status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
			}
			img := decodeImage(t, respBody)
			if img.MimeType != tc.mime || int(img.Width) != tc.w || int(img.Height) != tc.ht {
				t.Errorf("image = %s %dx%d, want %s %dx%d", img.MimeType, img.Width, img.Height, tc.mime, tc.w, tc.ht)
			}
			id := img.Id.String()

			// Identity serve returns the stored original with its own type.
			status, header, orig := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id, "", nil, true))
			if status != http.StatusOK {
				t.Fatalf("get original status = %d, want %d", status, http.StatusOK)
			}
			if ctype := header.Get("Content-Type"); ctype != tc.mime {
				t.Errorf("original Content-Type = %q, want %q", ctype, tc.mime)
			}
			if !bytes.Equal(orig, data) {
				t.Error("served original does not match uploaded bytes")
			}

			// A transform without an explicit web fmt is a 400.
			status, _, _ = h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id+"?w=16", "", nil, true))
			if status != http.StatusBadRequest {
				t.Errorf("transform without fmt status = %d, want %d", status, http.StatusBadRequest)
			}

			// With fmt=jpeg it decodes via libvips and returns a jpeg derivative.
			status, header, deriv := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id+"?w=16&fmt=jpeg", "", nil, true))
			if status != http.StatusOK {
				t.Fatalf("transform status = %d, want %d", status, http.StatusOK)
			}
			if ctype := header.Get("Content-Type"); ctype != "image/jpeg" {
				t.Errorf("derivative Content-Type = %q, want image/jpeg", ctype)
			}
			if len(deriv) == 0 {
				t.Error("derivative body is empty")
			}
		})
	}
}

func TestAPIGetImageStripLossless(t *testing.T) {
	h := newAPIHarness(t, nil)
	data := jpegWithExif(t, 48, 32)
	body, ct := multipartBody(t, "file", "photo.jpg", data)
	status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
	}
	id := decodeImage(t, respBody).Id.String()

	status, header, stripped := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id+"?strip=true", "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("strip status = %d, want %d", status, http.StatusOK)
	}
	if ctype := header.Get("Content-Type"); ctype != "image/jpeg" {
		t.Errorf("stripped Content-Type = %q, want image/jpeg", ctype)
	}
	if bytes.Contains(stripped, []byte(gpsMarker)) {
		t.Error("stripped derivative still contains the EXIF marker")
	}
}

func TestAPIGetImageStripUnsupportedFormat(t *testing.T) {
	h := newAPIHarness(t, nil)
	body, ct := multipartBody(t, "file", "anim.gif", gifBytes(t, 20, 20))
	status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
	}
	id := decodeImage(t, respBody).Id.String()

	// Lossless strip is unsupported for GIF: a strip-only request is a 415.
	status, _, _ = h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id+"?strip=true", "", nil, true))
	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("strip status = %d, want %d", status, http.StatusUnsupportedMediaType)
	}
}

func TestAPIGetImageMeta(t *testing.T) {
	h := newAPIHarness(t, nil)
	id := h.uploadPNG(t, 12, 8)

	status, header, body := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id.String()+"/meta", "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("meta status = %d, want %d", status, http.StatusOK)
	}
	if cc := header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("meta Cache-Control = %q, want no-cache", cc)
	}
	img := decodeImage(t, body)
	if img.Id != id || img.Width != 12 || img.Height != 8 {
		t.Errorf("meta = %+v, want id %v 12x8", img, id)
	}
}

func TestAPIGetImageNotFound(t *testing.T) {
	h := newAPIHarness(t, nil)
	missing := uuid.New().String()
	for _, path := range []string{"/v1/images/" + missing, "/v1/images/" + missing + "/meta"} {
		status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, path, "", nil, true))
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", path, status, http.StatusNotFound)
		}
	}
}

func TestAPITransformCacheRoundTrip(t *testing.T) {
	h := newAPIHarness(t, nil)
	id := h.uploadPNG(t, 60, 40)
	path := "/v1/images/" + id.String() + "?w=30&fmt=webp&q=80"

	// First GET generates the derivative (cache miss)...
	status, _, first := h.doValidated(t, h.newReq(t, http.MethodGet, path, "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("miss status = %d, want %d", status, http.StatusOK)
	}
	// ...the second serves the same bytes from the cache.
	status, header, second := h.doValidated(t, h.newReq(t, http.MethodGet, path, "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("hit status = %d, want %d", status, http.StatusOK)
	}
	if ct := header.Get("Content-Type"); ct != "image/webp" {
		t.Errorf("hit Content-Type = %q, want image/webp", ct)
	}
	if !bytes.Equal(first, second) {
		t.Error("cache hit served different bytes than the generating request")
	}
}

func TestAPIDeleteImage(t *testing.T) {
	h := newAPIHarness(t, nil)
	id := h.uploadPNG(t, 20, 10)
	// Materialize a derivative so the purge has something real to remove.
	h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+id.String()+"?w=10", "", nil, true))

	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodDelete, "/v1/images/"+id.String(), "", nil, true))
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", status, http.StatusNoContent)
	}

	// The image is gone from every read path.
	for _, path := range []string{"/v1/images/" + id.String(), "/v1/images/" + id.String() + "/meta"} {
		status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, path, "", nil, true))
		if status != http.StatusNotFound {
			t.Errorf("GET %s after delete = %d, want %d", path, status, http.StatusNotFound)
		}
	}
	// And a second delete is a 404, not an error.
	status, _, _ = h.doValidated(t, h.newReq(t, http.MethodDelete, "/v1/images/"+id.String(), "", nil, true))
	if status != http.StatusNotFound {
		t.Errorf("second delete = %d, want %d", status, http.StatusNotFound)
	}
}

func TestAPIDeleteImageUnauthorized(t *testing.T) {
	h := newAPIHarness(t, nil)
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodDelete, "/v1/images/"+uuid.New().String(), "", nil, false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestAPIGetImageUnauthorized(t *testing.T) {
	h := newAPIHarness(t, nil)
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+uuid.New().String(), "", nil, false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestAPIUploadFromURLUnauthorized(t *testing.T) {
	h := newAPIHarness(t, nil)
	req := h.newReq(t, http.MethodPost, "/v1/images/from-url", "application/json",
		strings.NewReader(`{"url":"https://example.com/a.png"}`), false)
	status, _, _ := h.doValidated(t, req)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

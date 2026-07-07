//go:build e2e

package api

// e2e_test.go targets the real, already-running `server` Docker container
// over the network (see `make test-e2e`), instead of wrapping the router
// in-process with httptest like the apitest suite does. It is the only test
// path that exercises the Dockerfile, the libvips runtime lib, non-root
// volume permissions, env-var wiring, and ENTRYPOINT.
//
// Scope, deliberately curated (does not mirror every apitest case):
//   - Config-mutation tests (TestAPIUploadImageTooLarge, TestAPIUploadImageRateLimited)
//     cannot be reproduced here: the live container's config is fixed for its
//     whole process lifetime from .env, so there is no mutate hook.
//   - The >10MB 413 case and the 429 rate-limit case are skipped: slow/flaky
//     against a shared, developer-configured RATE_LIMIT_PER_MIN, and already
//     covered by both the apitest suite and unit tests with miniredis.
//   - GIF is skipped entirely (its fixture helper is apitest-tagged, not
//     visible here); the strip-unsupported-format case uses a tiny literal
//     WebP fixture instead, which is also a lossless-strip-unsupported format.
//
// Data lifecycle: the live container's Postgres/Redis/storage volumes are the
// SAME ones a developer's `make up` session uses. Never TRUNCATE or FlushDB.
// Every test cleans up only what it created, via t.Cleanup calling the real
// DELETE endpoint (see cleanupDelete). Do not add t.Parallel() to these tests:
// several reuse fixed-byte fixtures, and concurrent runs would race on the
// shared content-hash dedup + delete-in-cleanup design.
//
// Reuses untagged fixture helpers already in this package for free:
// pngBytes, multipartBody, decodeImage (upload_test.go), jpegWithExif,
// gpsMarker (images_strip_test.go).

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
)

// e2eWebP1x1 is a minimal valid 1x1 lossless WebP (VP8L) file: a format with
// no lossless strip support, used only for the strip-415 case. Duplicated
// from internal/imageproc/detect_test.go's unexported webp1x1 (different
// package, not importable); keep it in sync if that literal ever changes.
var e2eWebP1x1 = []byte{
	'R', 'I', 'F', 'F', 0x1a, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P',
	'V', 'P', '8', 'L', 0x0d, 0x00, 0x00, 0x00,
	0x2f, 0x00, 0x00, 0x00, 0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe, 0x07, 0x00,
}

// e2eHarness targets a live base URL instead of an httptest.Server. Config
// (API key, upload limits, rate limit) is whatever the running container's
// .env holds and cannot be mutated per test — see the file doc comment.
type e2eHarness struct {
	baseURL string
	client  *http.Client
	router  routers.Router
	apiKey  string
}

// newE2EHarness reads E2E_BASE_URL (default http://localhost:8080) and
// API_KEY from the environment; `make test-e2e` sources .env before running
// go test, so this is the same API_KEY the live container was started with.
func newE2EHarness(t *testing.T) *e2eHarness {
	t.Helper()

	baseURL := os.Getenv("E2E_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		t.Fatal("API_KEY is not set; run via `make test-e2e` (it sources .env)")
	}

	spec, err := gen.GetSwagger()
	if err != nil {
		t.Fatalf("load embedded spec: %v", err)
	}
	spec.Servers = openapi3.Servers{&openapi3.Server{URL: baseURL}}
	router, err := gorillamux.NewRouter(spec)
	if err != nil {
		t.Fatalf("build spec router: %v", err)
	}

	return &e2eHarness{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
		router:  router,
		apiKey:  apiKey,
	}
}

// doValidated sends req, validating the request and the response against the
// OpenAPI spec, exactly like apiHarness.doValidated but over a real network
// client instead of an httptest server's client.
func (h *e2eHarness) doValidated(t *testing.T, req *http.Request) (int, http.Header, []byte) {
	t.Helper()

	route, pathParams, err := h.router.FindRoute(req)
	if err != nil {
		t.Fatalf("FindRoute(%s %s): %v", req.Method, req.URL, err)
	}
	reqInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
		Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if err := openapi3filter.ValidateRequest(req.Context(), reqInput); err != nil {
		t.Fatalf("request violates spec: %v", err)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
	}
	respInput.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(req.Context(), respInput); err != nil {
		t.Errorf("response violates spec (%s %s -> %d): %v", req.Method, req.URL.Path, resp.StatusCode, err)
	}
	return resp.StatusCode, resp.Header, body
}

// newReq builds a request against the live container, optionally keyed. Only
// call this from the test body (not from t.Cleanup): it uses t.Context(),
// which testing documents as already canceled by the time Cleanup functions
// run. Cleanup requests must use cleanupDelete instead.
func (h *e2eHarness) newReq(t *testing.T, method, path, contentType string, body io.Reader, withKey bool) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.baseURL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if withKey {
		req.Header.Set("X-API-Key", h.apiKey)
	}
	return req
}

// cleanupDelete issues a best-effort DELETE against the live container from
// inside a t.Cleanup callback. It deliberately uses context.Background()
// with its own timeout rather than t.Context() or doValidated (see the
// file/newReq doc comments on why t.Context() is unsafe here), and treats
// both 204 and 404 as success: idempotent double-delete is expected when a
// test already deleted the image itself before its own cleanup runs.
func (h *e2eHarness) cleanupDelete(t *testing.T, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.baseURL+"/v1/images/"+id, nil)
	if err != nil {
		t.Errorf("cleanup: build DELETE request for %s: %v", id, err)
		return
	}
	req.Header.Set("X-API-Key", h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		t.Errorf("cleanup: DELETE %s: %v", id, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		t.Errorf("cleanup: DELETE %s status = %d, want 204 or 404", id, resp.StatusCode)
	}
}

// uploadAndTrack uploads data via multipart, asserts 201, and schedules a
// cleanupDelete so repeated `make test-e2e` runs (and the developer's own
// `make up` session) never accumulate rows or files in the persistent
// pg-data/image-data volumes. Content-hash dedup means a byte-identical
// fixture reused across separate `go test` invocations returns the SAME
// existing record rather than creating a new one; that's fine as-is —
// cleanup deletes it either way, and the deleted row is gone before the
// next run's identical bytes are uploaded, so there is no cross-run leakage.
func (h *e2eHarness) uploadAndTrack(t *testing.T, filename string, data []byte) gen.Image {
	t.Helper()
	body, ct := multipartBody(t, "file", filename, data)
	status, _, respBody := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d (body: %s)", status, http.StatusCreated, respBody)
	}
	img := decodeImage(t, respBody)
	t.Cleanup(func() { h.cleanupDelete(t, img.Id.String()) })
	return img
}

func TestE2EHealthz(t *testing.T) {
	h := newE2EHarness(t)
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, "/healthz", "", nil, false))
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
}

func TestE2EUploadMultipartDedup(t *testing.T) {
	h := newE2EHarness(t)
	// Odd, specific dimensions: vanishingly unlikely to collide with bytes a
	// developer might have uploaded manually while poking at `make up`.
	data := pngBytes(t, 61, 37)

	first := h.uploadAndTrack(t, "e2e-dedup.png", data)
	if first.MimeType != "image/png" || first.Width != 61 || first.Height != 37 {
		t.Errorf("image = %+v, want 61x37 image/png", first)
	}

	// Byte-identical re-upload dedups to the same record; both cleanups
	// target the same id, which cleanupDelete tolerates (204 then 404).
	second := h.uploadAndTrack(t, "e2e-dedup-again.png", data)
	if second.Id != first.Id {
		t.Errorf("dedup id = %v, want %v", second.Id, first.Id)
	}
}

func TestE2EUploadUnauthorized(t *testing.T) {
	h := newE2EHarness(t)
	body, ct := multipartBody(t, "file", "a.png", pngBytes(t, 1, 1))
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, false))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestE2EUploadUnsupportedType(t *testing.T) {
	h := newE2EHarness(t)
	body, ct := multipartBody(t, "file", "a.txt", []byte("not an image"))
	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodPost, "/v1/images", ct, body, true))
	if status != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", status, http.StatusUnsupportedMediaType)
	}
}

// TestE2EUploadFromURLBlockedSSRF exercises the real SSRF guard end-to-end
// with no config mutation needed: metadata and loopback destinations must be
// rejected before any dial.
func TestE2EUploadFromURLBlockedSSRF(t *testing.T) {
	h := newE2EHarness(t)
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost/image.png",
	} {
		req := h.newReq(t, http.MethodPost, "/v1/images/from-url", "application/json",
			strings.NewReader(`{"url":"`+target+`"}`), true)
		status, _, _ := h.doValidated(t, req)
		if status != http.StatusBadRequest {
			t.Errorf("POST from-url %q status = %d, want %d", target, status, http.StatusBadRequest)
		}
	}
}

func TestE2EGetOriginalAndTransform(t *testing.T) {
	h := newE2EHarness(t)
	data := pngBytes(t, 63, 41)
	img := h.uploadAndTrack(t, "e2e-transform.png", data)
	id := img.Id.String()

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
}

func TestE2EStripLossless(t *testing.T) {
	h := newE2EHarness(t)
	data := jpegWithExif(t, 48, 32)
	img := h.uploadAndTrack(t, "e2e-strip.jpg", data)

	status, header, stripped := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+img.Id.String()+"?strip=true", "", nil, true))
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

// TestE2EStripUnsupportedFormat: WebP has no lossless strip support, so a
// strip-only request is a 415 (GIF's equivalent case is already covered by
// the in-process apitest suite; see the file doc comment).
func TestE2EStripUnsupportedFormat(t *testing.T) {
	h := newE2EHarness(t)
	img := h.uploadAndTrack(t, "e2e-strip.webp", e2eWebP1x1)

	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+img.Id.String()+"?strip=true", "", nil, true))
	if status != http.StatusUnsupportedMediaType {
		t.Errorf("strip status = %d, want %d", status, http.StatusUnsupportedMediaType)
	}
}

func TestE2EGetImageMeta(t *testing.T) {
	h := newE2EHarness(t)
	img := h.uploadAndTrack(t, "e2e-meta.png", pngBytes(t, 17, 13))

	status, header, body := h.doValidated(t, h.newReq(t, http.MethodGet, "/v1/images/"+img.Id.String()+"/meta", "", nil, true))
	if status != http.StatusOK {
		t.Fatalf("meta status = %d, want %d", status, http.StatusOK)
	}
	if cc := header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("meta Cache-Control = %q, want no-cache", cc)
	}
	meta := decodeImage(t, body)
	if meta.Id != img.Id || meta.Width != 17 || meta.Height != 13 {
		t.Errorf("meta = %+v, want id %v 17x13", meta, img.Id)
	}
}

func TestE2EDeleteImageLifecycle(t *testing.T) {
	h := newE2EHarness(t)
	img := h.uploadAndTrack(t, "e2e-delete.png", pngBytes(t, 23, 19))
	id := img.Id.String()

	status, _, _ := h.doValidated(t, h.newReq(t, http.MethodDelete, "/v1/images/"+id, "", nil, true))
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", status, http.StatusNoContent)
	}

	// Idempotent second delete: not found, not an error.
	status, _, _ = h.doValidated(t, h.newReq(t, http.MethodDelete, "/v1/images/"+id, "", nil, true))
	if status != http.StatusNotFound {
		t.Errorf("second delete status = %d, want %d", status, http.StatusNotFound)
	}

	// Every read path agrees the image is gone.
	for _, path := range []string{"/v1/images/" + id, "/v1/images/" + id + "/meta"} {
		status, _, _ := h.doValidated(t, h.newReq(t, http.MethodGet, path, "", nil, true))
		if status != http.StatusNotFound {
			t.Errorf("GET %s after delete = %d, want %d", path, status, http.StatusNotFound)
		}
	}
	// img's own uploadAndTrack cleanup will DELETE again below; cleanupDelete
	// tolerates the resulting 404.
}

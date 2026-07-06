//go:build apitest

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pyaethu-aung/image-server/internal/config"
)

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

func TestAPIUploadFromURLUnauthorized(t *testing.T) {
	h := newAPIHarness(t, nil)
	req := h.newReq(t, http.MethodPost, "/v1/images/from-url", "application/json",
		strings.NewReader(`{"url":"https://example.com/a.png"}`), false)
	status, _, _ := h.doValidated(t, req)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

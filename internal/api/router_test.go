package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestRouter builds the full router around a Server with test fakes, so
// requests exercise the real middleware chain and generated wrappers.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	rdb, _ := newTestRedis(t)
	return NewRouter(NewServer(testConfig(), nil, nil, rdb, nil))
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newTestRouter(t))
	defer srv.Close()

	// No X-API-Key: /healthz must stay public.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`body["status"] = %q, want "ok"`, body["status"])
	}
}

// TestRouterBindingError covers the ErrorHandlerFunc: a non-UUID id fails
// parameter binding before the handler runs and must return the spec's
// Error shape with a 400.
func TestRouterBindingError(t *testing.T) {
	srv := httptest.NewServer(newTestRouter(t))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/images/not-a-uuid", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if e.Code != "bad_request" {
		t.Errorf("error code = %q, want %q", e.Code, "bad_request")
	}
}

// Every endpoint is implemented as of step 5, enforced at compile time by
// the gen.ServerInterface assertion in server.go, so the old 501 router test
// has no subject left and was removed.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/config"
)

const testAPIKey = "test-key"

func testConfig() config.Config {
	return config.Config{
		APIKey:             testAPIKey,
		MaxUploadBytes:     1 << 20,
		MaxPixels:          1_000_000,
		RateLimitPerMin:    100,
		DerivativeCacheTTL: time.Hour,
	}
}

// newTestRedis returns a redis client backed by miniredis and the miniredis
// instance itself (for failure injection). Both close with the test.
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// securedRequest marks req the way the generated wrappers mark /v1 routes.
// The string context key is dictated by the generated code.
func securedRequest(req *http.Request) *http.Request {
	ctx := context.WithValue(req.Context(), gen.ApiKeyAuthScopes, []string{}) //nolint:staticcheck // key type matches gen wrappers
	return req.WithContext(ctx)
}

// nextRecorder is a terminal handler that records whether it was reached.
type nextRecorder struct{ called bool }

func (n *nextRecorder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	n.called = true
	w.WriteHeader(http.StatusNoContent)
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) gen.Error {
	t.Helper()
	var e gen.Error
	if err := json.NewDecoder(rec.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e
}

func TestAuthMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		secured    bool
		apiKey     string // header value; "" = no header
		wantNext   bool
		wantStatus int
		wantCode   string
	}{
		{name: "public route passes without key", secured: false, wantNext: true},
		{name: "secured route without key", secured: true, wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
		{name: "secured route with wrong key", secured: true, apiKey: "wrong", wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
		{name: "secured route with correct key", secured: true, apiKey: testAPIKey, wantNext: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdb, _ := newTestRedis(t)
			s := NewServer(testConfig(), nil, nil, rdb, nil)
			next := &nextRecorder{}

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images", nil)
			if tt.secured {
				req = securedRequest(req)
			}
			if tt.apiKey != "" {
				req.Header.Set("X-API-Key", tt.apiKey)
			}
			rec := httptest.NewRecorder()
			s.authMiddleware(next).ServeHTTP(rec, req)

			if next.called != tt.wantNext {
				t.Errorf("next called = %v, want %v", next.called, tt.wantNext)
			}
			if !tt.wantNext {
				if rec.Code != tt.wantStatus {
					t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
				}
				if e := decodeError(t, rec); e.Code != tt.wantCode {
					t.Errorf("error code = %q, want %q", e.Code, tt.wantCode)
				}
			}
		})
	}
}

func TestRateLimitMiddlewarePublicRouteBypasses(t *testing.T) {
	rdb, mr := newTestRedis(t)
	mr.Close() // even with Redis down, public routes never consult it
	s := NewServer(testConfig(), nil, nil, rdb, nil)
	next := &nextRecorder{}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	s.rateLimitMiddleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if !next.called {
		t.Error("next not called for public route")
	}
}

func TestRateLimitMiddlewareUnderLimit(t *testing.T) {
	rdb, _ := newTestRedis(t)
	s := NewServer(testConfig(), nil, nil, rdb, nil)
	next := &nextRecorder{}

	req := securedRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images", nil))
	req.Header.Set("X-API-Key", testAPIKey)
	rec := httptest.NewRecorder()
	s.rateLimitMiddleware(next).ServeHTTP(rec, req)

	if !next.called {
		t.Errorf("next not called, status %d", rec.Code)
	}
}

func TestRateLimitMiddlewareOverLimit(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitPerMin = 1
	rdb, _ := newTestRedis(t)
	s := NewServer(cfg, nil, nil, rdb, nil)

	handler := s.rateLimitMiddleware(&nextRecorder{})
	newReq := func() *http.Request {
		req := securedRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images", nil))
		req.Header.Set("X-API-Key", testAPIKey)
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, newReq())
	if first.Code != http.StatusNoContent {
		t.Fatalf("first request status = %d, want %d", first.Code, http.StatusNoContent)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, newReq())
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
	if ra := second.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", ra)
	}
	if e := decodeError(t, second); e.Code != "rate_limited" {
		t.Errorf("error code = %q, want %q", e.Code, "rate_limited")
	}
}

func TestRateLimitMiddlewareFailsOpenWhenRedisDown(t *testing.T) {
	rdb, mr := newTestRedis(t)
	mr.Close()
	s := NewServer(testConfig(), nil, nil, rdb, nil)
	next := &nextRecorder{}

	req := securedRequest(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/images", nil))
	req.Header.Set("X-API-Key", testAPIKey)
	s.rateLimitMiddleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if !next.called {
		t.Error("rate limiter did not fail open with Redis down")
	}
}

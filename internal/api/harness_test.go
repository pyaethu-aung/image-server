//go:build apitest

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
	"github.com/pyaethu-aung/image-server/internal/config"
	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/fetch"
	"github.com/pyaethu-aung/image-server/internal/storage"
)

// Shared per-process state: one pool and one applied schema for all apitest
// tests. make test-api boots Postgres/Redis via compose and sources .env.
var (
	apitestRedisAddr string
	apitestPool      *pgxpool.Pool
)

func TestMain(m *testing.M) {
	dbURL := os.Getenv("DATABASE_URL")
	apitestRedisAddr = os.Getenv("REDIS_ADDR")
	if dbURL == "" || apitestRedisAddr == "" {
		fmt.Fprintln(os.Stderr, "apitest: DATABASE_URL and REDIS_ADDR must be set; run via `make test-api`")
		os.Exit(1)
	}

	if err := applyMigrations(dbURL); err != nil {
		fmt.Fprintf(os.Stderr, "apitest: migrations: %v\n", err)
		os.Exit(1)
	}

	var err error
	apitestPool, err = pgxpool.New(context.Background(), dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apitest: connect postgres: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	apitestPool.Close()
	os.Exit(code)
}

// applyMigrations brings the schema up idempotently via the golang-migrate
// library; make test-api deliberately does not run make migrate.
func applyMigrations(dbURL string) error {
	abs, err := filepath.Abs("../../migrations")
	if err != nil {
		return err
	}
	// golang-migrate's pgx/v5 database driver registers the pgx5:// scheme.
	mURL := dbURL
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(mURL, prefix) {
			mURL = "pgx5://" + strings.TrimPrefix(mURL, prefix)
			break
		}
	}
	mig, err := migrate.New("file://"+abs, mURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = mig.Close() }()
	if err := mig.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// apiHarness runs the real router (real Postgres, real Redis, real SSRF
// guard, local storage in t.TempDir()) and validates every request and
// response against the embedded OpenAPI spec.
type apiHarness struct {
	srv    *httptest.Server
	router routers.Router
	apiKey string
}

func newAPIHarness(t *testing.T, mutate func(*config.Config)) *apiHarness {
	t.Helper()

	cfg := config.Config{
		APIKey:             "apitest-key",
		MaxUploadBytes:     1 << 20,
		MaxPixels:          50_000_000,
		RateLimitPerMin:    1000,
		DerivativeCacheTTL: time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: apitestRedisAddr})
	fetcher := fetch.New(10*time.Second, cfg.MaxUploadBytes)
	srv := httptest.NewServer(NewRouter(NewServer(cfg, st, db.New(apitestPool), rdb, fetcher)))

	spec, err := gen.GetSwagger()
	if err != nil {
		t.Fatalf("load embedded spec: %v", err)
	}
	// Point the spec's server at this test instance so route matching works.
	spec.Servers = openapi3.Servers{&openapi3.Server{URL: srv.URL}}
	router, err := gorillamux.NewRouter(spec)
	if err != nil {
		t.Fatalf("build spec router: %v", err)
	}

	t.Cleanup(func() {
		srv.Close()
		ctx := context.Background()
		if _, err := apitestPool.Exec(ctx, "TRUNCATE images"); err != nil {
			t.Errorf("cleanup truncate: %v", err)
		}
		if err := rdb.FlushDB(ctx).Err(); err != nil {
			t.Errorf("cleanup flushdb: %v", err)
		}
		_ = rdb.Close()
	})

	return &apiHarness{srv: srv, router: router, apiKey: cfg.APIKey}
}

// doValidated sends req, validating the request and the response against
// the OpenAPI spec. Server-side auth is what's under test, so client-side
// security validation is a no-op.
func (h *apiHarness) doValidated(t *testing.T, req *http.Request) (int, http.Header, []byte) {
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

	resp, err := h.srv.Client().Do(req)
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

// newReq builds a request against the harness server, optionally keyed.
func (h *apiHarness) newReq(t *testing.T, method, path, contentType string, body io.Reader, withKey bool) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.srv.URL+path, body)
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

# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## Project Status

**All five implementation steps complete — the core HTTP service is fully built.** The compose stack (Postgres 18, Redis 8, server image with libvips), migrations, sqlc setup, config package, the `Storage` interface with its local filesystem backend (`internal/storage/`), the upload path, the transform-on-read path, derivative caching, and delete are built and tested: `POST /v1/images` (multipart) and `POST /v1/images/from-url` (SSRF-guarded via `internal/fetch/`), magic-byte detection and the bomb guard (`internal/imageproc/`), `GET /v1/images/{id}` (original, or transformed via libvips using the `w`/`h`/`fmt`/`q`/`fit` params, each derivative generated once and cached under `derivatives/` with Redis markers), `GET /v1/images/{id}/meta`, `DELETE /v1/images/{id}` (purges original, derivatives, metadata, and Redis state), API-key auth + Redis rate-limit middleware, DB/Redis wiring in `main.go`, and the spec-validated `apitest` harness. Every generated endpoint is implemented (compile-time `ServerInterface` assertion; the `Unimplemented` embed is gone). Next up is the post-core roadmap below. This document is the implementation contract; update it as reality diverges from the plan.

## What This Is

An image upload and transformation server in Go. Users upload images (multipart or by URL), originals are stored on the local filesystem behind a storage interface, and images are served back with on-the-fly transforms (resize, format conversion, quality, fit mode) driven by query params. Generated derivatives are cached so the same transform is never recomputed. S3-compatible storage is a planned second backend, not part of the initial build.

## Tech Stack (fixed, do not substitute)

- Go 1.26+ with the `chi` router
- `h2non/bimg` (libvips) for image processing. libvips must be installed on the host for local builds; the Docker image handles it in containers.
- Local filesystem for object storage (first `Storage` implementation; S3 comes later)
- PostgreSQL via `pgx` + `sqlc` for image metadata
- Redis for derivative cache keys and per-key rate limiting
- `oapi-codegen` for chi server interfaces + models generated from the OpenAPI spec; `kin-openapi` for request/response validation in API tests
- `docker-compose` for the local stack (Postgres, Redis)
- Config via env vars (`caarlos0/env` or plain `os.Getenv`)

## Commands

All workflows go through the Makefile:

```
make setup        # one-time: wire git hooks (core.hooksPath -> .githooks/)
make up           # build + boot the full stack detached (server + Postgres + Redis)
make down         # stop the stack (keeps volumes)
make run          # start the server locally
make migrate      # apply SQL migrations (DATABASE_URL from env or .env)
make sqlc-gen     # regenerate sqlc code from queries
make openapi-gen  # regenerate chi interfaces + models from the OpenAPI spec
make lint         # run golangci-lint (config in .golangci.yml)
make test         # run all unit tests
make coverage     # print overall coverage (excludes cmd/ and generated code)
make test-api     # validate every endpoint against the OpenAPI spec
make test-e2e     # full-stack e2e tests against the real server container (LOCAL-ONLY, not run in CI)
```

After editing anything under `internal/db/queries/` or `migrations/`, run `make sqlc-gen`; after editing `docs/openapi/image-server.yaml`, run `make openapi-gen`. Never hand-edit generated files (`internal/api/gen/`, sqlc output). The pre-commit hook fails if generated code is out of sync.

## Project Structure

```
cmd/server/          entrypoint (main.go: wire config, storage, db, redis, router)
internal/api/        handlers, middleware (auth, rate limit), router
internal/api/gen/    oapi-codegen output (generated, never hand-edit)
internal/storage/    Storage interface + local filesystem implementation
internal/imageproc/  bimg transforms, param parsing, image validation
internal/fetch/      SSRF guard + pinned-dial HTTP client for from-url
internal/db/         sqlc-generated code + queries
internal/config/     env parsing
migrations/          SQL migrations
docs/openapi/        image-server.yaml (OpenAPI 3.0.3, source of truth for the API)
.githooks/           pre-commit, commit-msg, pre-push (activated via make setup)
oapi-codegen.yaml    codegen config for the spec
docker-compose.yml
Makefile
```

## Spec-First API Workflow

`docs/openapi/image-server.yaml` is the source of truth for the API. Any endpoint change starts there:

1. Edit the spec
2. Run `make openapi-gen` (regenerates `internal/api/gen/server.gen.go`)
3. Implement or update the handler methods on the generated `ServerInterface`
4. Update the API spec tests if response shapes changed

Never change handler behavior without updating the spec in the same commit; the pre-commit hook blocks spec/codegen drift and the pre-push API spec gate blocks behavioral drift.

## Quality Gates Before Push

Git hooks live in `.githooks/` (committed) and are activated once per clone with `make setup`. Never bypass them with `--no-verify`.

- **pre-commit**: `go mod tidy` must be a no-op; generated code (`internal/api/gen/`, sqlc output) must be in sync with the spec/queries
- **commit-msg**: subject line max 72 chars (hard fail), 50 preferred
- **pre-push, gate 1 (lint)**: `make lint` runs golangci-lint (standard set plus gosec, noctx, bodyclose, and friends; config in `.golangci.yml`, generated code excluded). Fix findings, don't silence them without a reason
- **pre-push, gate 2 (coverage)**: per-layer `go test -cover` on `internal/api`, `internal/imageproc`, `internal/storage`, `internal/fetch` plus overall coverage, all at a **90% threshold**. Excluded from coverage: `cmd/`, `internal/api/gen/`, `internal/db` (generated). Layers with no source files yet are skipped, so the gate works during incremental build-out. The logic lives in `scripts/coverage-gate.sh`, shared with CI; change thresholds there, never in just one place
- **pre-push, gate 3 (API spec)**: `make test-api` boots Postgres + Redis via docker compose and runs build-tagged (`apitest`) tests that validate every endpoint's requests and responses against the OpenAPI spec
- **pre-push, gate 4 (full-stack e2e, LOCAL-ONLY)**: `make test-e2e` boots the full stack including the real `server` Docker container (`--build --wait`), applies migrations, and runs build-tagged (`e2e`) tests in `internal/api/` against it over the network via its published port. `--wait` can only block until the container is actually ready (DB/Redis connected, listener bound), not just running, because the Dockerfile's `HEALTHCHECK` calls the binary's own `server healthcheck` subcommand (`cmd/server/main.go`), which probes its own `/healthz` over loopback and exits 0/1 — no `curl` in the runtime image. **Unlike gates 1-3, CI does not re-run this gate** — `docker.yml` builds and pushes the image but runs no tests against it, so `--no-verify` or an unconfigured clone has zero backstop here for container-level regressions (Dockerfile, libvips runtime lib, volume permissions, env-var wiring, `ENTRYPOINT`)

This mirrors the hook setup in `yomafleet/better-marketing-service` (minus its Snyk/SonarQube stages).

CI (`.github/workflows/`) re-enforces gates 1-3 server-side, so `--no-verify` or an unconfigured clone cannot bypass *those*: `ci.yml` (lint, coverage gate via the shared script, generated-code drift check, API spec tests against Postgres/Redis service containers), `docker.yml` (image build on PRs, push to GHCR on main/tags, **no tests run against the built image**), `security.yml` (govulncheck on PRs and weekly), `semantic.yml` (Conventional Commits PR titles). Dependabot watches gomod, docker, and github-actions weekly. Gate 4 (e2e) is intentionally local-only; see above.

## Architecture Decisions

### Storage is an interface, local filesystem is the first implementation

Define in `internal/storage/`:

```go
type Storage interface {
    Put(ctx context.Context, key string, r io.Reader, contentType string) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Delete(ctx context.Context, key string) error
    Exists(ctx context.Context, key string) (bool, error)
    List(ctx context.Context, prefix string) ([]string, error)
}
```

Handlers and services depend only on this interface. The local backend stores objects under a configured root directory (`STORAGE_PATH`), with keys mapped to file paths. Adding a future backend (S3, OneDrive, Dropbox) must be a new file in `internal/storage/`, not a rewrite. Do not leak filesystem details (paths, `os.File`) outside that package; keys are opaque strings to callers.

`List` returns every key with a given byte prefix (S3-compatible prefix semantics, not a directory boundary), so derivative cleanup can enumerate `derivatives/<image_id>/` authoritatively from storage rather than depending on evictable Redis state. Backends return `ErrNotFound` from `Get` on a missing key and `ErrInvalidKey` for keys that are empty or would escape the namespace.

Local backend requirements:
- Sanitize keys before mapping to paths: reject `..`, absolute paths, and anything escaping `STORAGE_PATH` (path-traversal guard). The implementation combines `filepath.IsLocal` key validation with an `os.Root` handle so escapes are also rejected at the syscall level, including via pre-existing symlinks.
- Write via temp file + rename so a crashed write never leaves a partial object readable
- In Docker, `STORAGE_PATH` is a mounted volume so images survive container restarts

### Data model

`images` table: `id` (uuid), `original_filename`, `content_hash` (sha256), `mime_type`, `width`, `height`, `size_bytes`, `storage_key`, `created_at`.

Dedup on `content_hash` with a unique constraint: an identical upload returns the existing record instead of storing a duplicate. `storage_key` is backend-agnostic (a key into the `Storage` interface, not a file path).

### Derivative caching

- Cache key = hash of `image_id` + normalized transform params. Normalize before hashing (sorted params, defaults filled in) so `?w=100&h=200` and `?h=200&w=100` hit the same key.
- Derivatives live under a separate storage prefix (e.g. `derivatives/`) from originals. Check Redis for the cache key, and `Storage.Exists`, before regenerating.
- Serve responses with appropriate `Cache-Control` headers.

### Metadata stripping (`strip` param)

- `strip=true` on `GET /v1/images/{id}` removes metadata (EXIF including GPS, XMP, IPTC, comments) from the served image. The stored original is never modified; the stripped result is a cached derivative like any other (`strip` is part of the `CacheKey` canonical form).
- Two regimes: (1) strip combined with a resize/format change re-encodes via libvips with `StripMetadata` set (`internal/imageproc/apply.go`); (2) a strip-only request (`IsStripOnly`) uses a lossless, byte-preserving segment/chunk removal that never re-encodes pixels (`internal/imageproc/strip.go`, pure Go, no cgo). Lossless strip is implemented for JPEG and PNG; other formats on their own return `415` (checked via `CanStripLossless` before storage is touched), with `fmt` as the re-encode escape hatch.

### Endpoints

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/images` | multipart upload, returns id + metadata |
| POST | `/v1/images/from-url` | JSON `{"url": "..."}`, server fetches |
| GET | `/v1/images/{id}` | original, or transformed via `w`, `h`, `fmt` (jpeg/png/webp), `q`, `fit` (cover/contain), `strip` (true/false) |
| GET | `/v1/images/{id}/meta` | metadata JSON |
| DELETE | `/v1/images/{id}` | delete original, derivatives, metadata |
| GET | `/healthz` | no auth |

All `/v1` routes require an `X-API-Key` header matching the configured env key, enforced in middleware, plus per-key rate limiting backed by Redis.

## Security Requirements (non-negotiable, do not skip or weaken)

1. **Magic-byte validation**: detect the real image type from content, never trust the file extension or Content-Type header.
2. **Max upload size**: env-configurable, default 10MB. Enforce with `http.MaxBytesReader`, not just a header check.
3. **Decompression-bomb guard**: reject images whose decoded pixel count (width x height) exceeds a configured cap before running transforms.
4. **SSRF protection on `from-url`**: this is the most security-sensitive code in the repo. The guard must:
   - Allow only `http` and `https` schemes
   - Resolve the hostname and reject private (RFC 1918), loopback, link-local ranges, and `169.254.169.254` (cloud metadata) for both IPv4 and IPv6
   - Re-resolve and re-check on every redirect (use a custom `CheckRedirect`), since a public host can redirect to an internal one
   - Pin the dial to the validated IP (custom `DialContext`) to prevent DNS-rebinding between the check and the fetch
5. **Path-traversal guard in local storage**: storage keys must never escape `STORAGE_PATH` (see storage section above).
6. Parameterized queries only (sqlc gives this for free). No secrets in code or committed files; everything sensitive comes from env vars.

Never generate or accept code that disables TLS verification or bypasses the API-key auth, including "just for testing".

## Testing

- Table-driven unit tests are the house style. Unit-test coverage must stay at or above 90% per layer and overall (enforced by the pre-push hook).
- Required coverage: `internal/imageproc` transform param parsing (valid/invalid/edge values for `w`, `h`, `fmt`, `q`, `fit`), the SSRF guard (private ranges, loopback, metadata IP, redirects to internal hosts, IPv6 forms, non-http schemes), and local storage key sanitization (traversal attempts).
- SSRF tests must not make real network calls; inject a resolver or test against the validation function directly.
- The local `Storage` backend makes integration-style tests cheap: use `t.TempDir()`, no mocks needed.
- **API spec tests** (`make test-api`): build-tagged `apitest` tests in `internal/api/` boot the chi router in-process with `httptest`, real Postgres/Redis from compose, and storage in `t.TempDir()`. Every request and response is validated against `docs/openapi/image-server.yaml` using `kin-openapi/openapi3filter`. One always-on unit test loads the spec via the kin-openapi loader so a malformed spec fails plain `make test` too.
- The apitest harness (built in step 3) lives in `internal/api/harness_test.go` + `apitest_test.go`: `TestMain` applies migrations idempotently via the golang-migrate library, tests run the real router against compose Postgres/Redis with storage in `t.TempDir()`, and every request/response is validated against the spec (gorillamux + openapi3filter). Cleanup is TRUNCATE + FlushDB per test. Go build tags are additive, so `go test -tags=apitest ./internal/api/...` also re-runs the untagged unit tests in that package. Accepted as-is (they are fast). If that ever becomes annoying, move the harness into its own package (e.g. `internal/api/apitest/`) and point `make test-api` there so the target runs spec tests only.
- The from-url happy path is unit-tested with a fake fetcher only: the real SSRF guard correctly refuses loopback, so a live 201 in the apitest harness would need real egress (intentional split, asserted by `TestAPIUploadFromURLBlocked`).
- **Full-stack e2e tests** (`make test-e2e`, LOCAL-ONLY, not run in CI): build-tagged `e2e` tests in `internal/api/e2e_test.go` hit the real, already-running `server` Docker container over the network (base URL `E2E_BASE_URL`, default `http://localhost:8080`), instead of `httptest.NewServer`. This is the one path that exercises the Dockerfile, the libvips runtime lib, non-root volume permissions, env-var wiring, and `ENTRYPOINT`, none of which `apitest` touches. `e2e_test.go` has its own minimal `e2eHarness` (base URL + `*http.Client`, no `httptest.Server`) but reuses the package's untagged fixture helpers (`pngBytes`, `multipartBody`, `decodeImage`) for free. It does not mirror `TestAPIUploadImageTooLarge` or `TestAPIUploadImageRateLimited`: those need per-test config mutation, impossible against a live container whose config is fixed for its process lifetime from `.env`. Because the live container shares the SAME persistent `pg-data`/`image-data` volumes as a developer's `make up` session, tests never `TRUNCATE`/`FlushDB`; each test cleans up only the images it created via `t.Cleanup` calling the real `DELETE /v1/images/{id}` endpoint (see `e2eHarness.cleanupDelete`, which deliberately avoids `t.Context()` since `testing.T.Context()` is already canceled by the time `Cleanup` functions run). `go test -tags=e2e ./internal/api/...` is additive like `apitest`, so it also re-runs the package's untagged unit tests.

## Implementation Order

Build and checkpoint in this order, one layer at a time:

1. Scaffold project structure + `docker-compose.yml` + migrations + sqlc setup + first `make openapi-gen` run (Makefile, hooks, and the OpenAPI spec already exist)
2. `Storage` interface + local filesystem implementation
3. Upload paths (`POST /v1/images`, then `/from-url` with the SSRF guard), implementing the generated `ServerInterface`; the API spec test harness lands here
4. Transform-on-read (`GET /v1/images/{id}` with query params)
5. Derivative caching (Redis keys + derivative prefix + Cache-Control)

Auth and rate-limit middleware land with step 3 (first authenticated endpoints). Do not batch multiple layers into one pass; finish and verify each before starting the next. Every step must leave the pre-push gates green (90% coverage on implemented layers, spec tests passing).

## Roadmap After Core

1. **MCP server interface**: expose the service as an MCP server so Claude and other MCP clients can upload and transform images through tools. Keep handler logic in service-layer functions that both the HTTP handlers and MCP tool handlers can call, so the MCP layer is a thin adapter. Local storage keeps this deployable as a single process with no cloud dependencies.
2. **S3 storage backend**: add an S3 implementation of `Storage` (AWS SDK for Go v2) as a second backend selected via config. This must not require changes outside `internal/storage/` and config wiring. For local testing use a maintained S3-compatible server such as Garage (do not use MinIO: the repo was archived in April 2026 and receives no security patches).

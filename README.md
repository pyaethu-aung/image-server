# image-server

An image upload and transformation service in Go. Upload images via multipart form or by URL, store originals on local disk behind a pluggable storage interface, and serve them back with on-the-fly transforms (resize, format conversion, quality, fit) via URL query params. Generated derivatives are cached so repeated transforms are served from cache, not recomputed.

> **Status: core HTTP service complete.** Uploads (`POST /v1/images`, `POST /v1/images/from-url`), reads (`GET /v1/images/{id}`, original or transformed, plus `/meta`), derivative caching, and `DELETE` are all live and tested. See [CLAUDE.md](CLAUDE.md) for the architecture decisions. Next on the roadmap: an MCP server interface, then an S3 storage backend.

## Features

- **Upload** via multipart file or by URL (server fetches it, with SSRF protection)
- **Local filesystem storage** for originals, behind a `Storage` interface (S3 planned as a second backend)
- **On-the-fly transforms** on read: width, height, format (jpeg/png/webp), quality, fit mode
- **Derivative caching**: each unique transform is generated once, stored under a separate prefix, and tracked in Redis
- **Deduplication**: identical uploads (same sha256) return the existing record
- **Security built in**: magic-byte type validation, upload size limits, decompression-bomb guard, SSRF protection, path-traversal guard, API-key auth, per-key rate limiting

## Tech Stack

| Concern | Choice |
|---|---|
| Language / router | Go 1.26+, `chi` |
| Image processing | `h2non/bimg` (libvips) |
| Object storage | Local filesystem (S3 via AWS SDK v2 planned) |
| Metadata | PostgreSQL via `pgx` + `sqlc` |
| Cache / rate limiting | Redis |
| Local stack | `docker-compose` (Postgres, Redis) |

## Getting Started

### Prerequisites

- Docker and Docker Compose
- Go 1.26+ and libvips (only for running the server outside Docker: `brew install vips` on macOS)

### Run the stack

```sh
git clone https://github.com/pyaethu-aung/image-server.git
cd image-server
make setup             # wire git hooks (one-time; see Quality gates below)
cp .env.example .env   # set API_KEY and adjust as needed
make up
```

This builds the server image and boots it with Postgres and Redis, detached. Uploaded images are stored in a Docker volume mounted at `STORAGE_PATH`. Then apply migrations:

```sh
make migrate
```

### Makefile targets

```sh
make setup        # one-time: activate the committed git hooks
make up           # build and boot the full stack, detached
make down         # stop the stack (keeps volumes)
make run          # run the server locally (expects the compose services up)
make migrate      # apply SQL migrations (DATABASE_URL from env or .env)
make sqlc-gen     # regenerate database code from SQL queries
make openapi-gen  # regenerate server interfaces from the OpenAPI spec
make lint         # run golangci-lint
make test         # run all unit tests
make coverage     # print overall coverage
make test-api     # validate every endpoint against the OpenAPI spec
```

### Quality gates (git hooks)

The repo ships hooks in `.githooks/`, activated by `make setup` (`git config core.hooksPath .githooks`):

- **pre-commit**: `go mod tidy` cleanliness and generated-code sync checks
- **commit-msg**: 50/72 subject-line rule
- **pre-push**: golangci-lint, unit-test coverage gate (90% per layer and overall, generated code excluded), and API spec tests (`make test-api`)

CI re-runs the same gates on every PR (lint, coverage, generated-code drift, spec tests against real Postgres/Redis), builds the Docker image, runs `govulncheck`, and validates PR titles as Conventional Commits. Dependabot keeps Go modules, Docker base images, and Actions current.

### Configuration

All configuration is via environment variables. Never commit real credentials.

| Variable | Description | Default |
|---|---|---|
| `API_KEY` | Key clients must send in `X-API-Key` | (required) |
| `LISTEN_ADDR` | HTTP listen address | `:8080` |
| `DATABASE_URL` | Postgres connection string | (required) |
| `REDIS_ADDR` | Redis address | `localhost:6379` |
| `STORAGE_PATH` | Root directory for stored images | `./data/images` |
| `MAX_UPLOAD_BYTES` | Max upload size | `10485760` (10MB) |
| `MAX_PIXELS` | Max decoded pixel count (bomb guard) | `50000000` |
| `RATE_LIMIT_PER_MIN` | Requests per minute per API key | `120` |
| `CACHE_CONTROL_MAX_AGE` | `Cache-Control` max-age (seconds) on served image bytes | `31536000` (1 year) |
| `DERIVATIVE_CACHE_TTL` | TTL for Redis derivative-cache markers (Go duration) | `720h` (30 days) |

## API

The OpenAPI 3.0.3 spec at [`docs/openapi/image-server.yaml`](docs/openapi/image-server.yaml) is the source of truth for this API; server interfaces are generated from it with `oapi-codegen`. The examples below are illustrative.

All `/v1` routes require an `X-API-Key` header. Examples assume the server on `localhost:8080` and `$API_KEY` exported.

### Upload an image (multipart)

```sh
curl -X POST http://localhost:8080/v1/images \
  -H "X-API-Key: $API_KEY" \
  -F "file=@photo.jpg"
```

Response:

```json
{
  "id": "1b4e28ba-2fa1-4d3b-9558-b7f6f18e3c2a",
  "original_filename": "photo.jpg",
  "content_hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "mime_type": "image/jpeg",
  "width": 4032,
  "height": 3024,
  "size_bytes": 2843190,
  "created_at": "2026-07-06T12:00:00Z"
}
```

Uploading a byte-identical file again returns the existing record (dedup by sha256).

### Upload from a URL

```sh
curl -X POST http://localhost:8080/v1/images/from-url \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/photo.jpg"}'
```

Only public `http(s)` URLs are accepted. Requests resolving to private, loopback, link-local, or cloud-metadata addresses are rejected, including via redirects.

### Get an image (original or transformed)

```sh
# Original
curl -H "X-API-Key: $API_KEY" \
  http://localhost:8080/v1/images/1b4e28ba-2fa1-4d3b-9558-b7f6f18e3c2a -o out.jpg

# Resized to 800px wide, converted to webp at quality 80
curl -H "X-API-Key: $API_KEY" \
  "http://localhost:8080/v1/images/1b4e28ba-2fa1-4d3b-9558-b7f6f18e3c2a?w=800&fmt=webp&q=80" -o out.webp

# 400x400 thumbnail, cropped to fill
curl -H "X-API-Key: $API_KEY" \
  "http://localhost:8080/v1/images/1b4e28ba-2fa1-4d3b-9558-b7f6f18e3c2a?w=400&h=400&fit=cover" -o thumb.jpg
```

Transform query params:

| Param | Values | Description |
|---|---|---|
| `w` | positive int | target width in pixels |
| `h` | positive int | target height in pixels |
| `fmt` | `jpeg`, `png`, `webp` | output format |
| `q` | 1 to 100 | output quality (lossy formats) |
| `fit` | `cover`, `contain` | `cover` crops to fill w x h; `contain` fits within w x h |

Each unique combination is generated once and cached; responses include `Cache-Control` headers.

### Get image metadata

```sh
curl -H "X-API-Key: $API_KEY" \
  http://localhost:8080/v1/images/1b4e28ba-2fa1-4d3b-9558-b7f6f18e3c2a/meta
```

### Delete an image

```sh
curl -X DELETE -H "X-API-Key: $API_KEY" \
  http://localhost:8080/v1/images/1b4e28ba-2fa1-4d3b-9558-b7f6f18e3c2a
```

Deletes the original, cached derivatives, and metadata.

### Health check

```sh
curl http://localhost:8080/healthz
```

No auth required.

## Project Structure

```
cmd/server/          entrypoint
internal/api/        handlers, middleware, router
internal/storage/    Storage interface + local filesystem implementation
internal/imageproc/  bimg transforms + validation
internal/db/         sqlc-generated code + queries
internal/config/     env parsing
migrations/          SQL migrations
docker-compose.yml
Makefile
```

Storage is defined as an interface (`Put`, `Get`, `Delete`, `Exists`) with the local filesystem as the first implementation, so additional backends (S3, OneDrive, Dropbox) can be added without touching handler code.

## Testing

```sh
make test      # unit tests
make test-api  # spec-driven API tests (starts Postgres + Redis via compose)
```

Table-driven unit tests cover transform param parsing, the SSRF guard, and storage key sanitization; the pre-push hook enforces 90% coverage. API tests boot the router in-process and validate every request/response against the OpenAPI spec with `kin-openapi`.

## Roadmap

- [x] Core HTTP service (scaffold, storage, upload, transform, caching)
- [ ] MCP server interface exposing upload/transform as tools
- [ ] S3 storage backend (AWS SDK for Go v2, Garage or LocalStack for local testing)

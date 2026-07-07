# Core HTTP Service — Implementation Plan & Handoff Checklist

> **Purpose.** This is a self-contained handoff for implementing the README Roadmap item
> **"Core HTTP service (scaffold, storage, upload, transform, caching)"**. It is written so a
> fresh agent or a different account can execute it without prior conversation context. Check
> off `- [ ]` items as they land. Keep this file updated as the source of truth for progress.
>
> **Authoritative contract:** `CLAUDE.md` (build order, security requirements, gates) and
> `docs/openapi/image-server.yaml` (the API, already fully specified). If anything here conflicts
> with `CLAUDE.md`, `CLAUDE.md` wins — and update this file.

## Progress

- **Step 1 — scaffold: complete** (pre-existing).
- **Step 2 — storage: complete and merged to `main`.**
- **Step 3 — uploads: complete and merged to `main`** (PR #7). imageproc detection (100%),
  fetch SSRF guard (96.0%), api plumbing + middleware + main.go wiring, upload handlers,
  apitest harness, CLAUDE.md sync. All gates green; live contract check done.
- **Step 4 — transforms: complete and merged to `main`** (PR #9). 6 commits on
  `feat/transforms`: imageproc transform layer (ParseTransform + bimg Apply, 95.9%),
  CacheControlMaxAge config, GetImage/GetImageMeta handlers (api 94.5%), spec-validated
  apitests, CLAUDE.md + README sync. Gates green (lint 0, overall 95.4%, `make test-api`
  pass, `-race` clean); live smoke test done (cover/contain dims, meta, 400/404/401,
  DELETE still 501). See Step 4 deviations below.
- **Step 5 — caching: complete and merged to `main`** (PR #10). 6 commits on `feat/caching`:
  CacheKey (pinned known-value test), DerivativeCacheTTL config, derivative cache + DeleteImage
  (api 92.7%, imageproc 96.1%, config 100%), apitests, CLAUDE.md + README sync (roadmap
  box checked). Gates green (lint 0, overall 94.5%, `make test-api` pass, `-race` clean);
  live check done (miss 24ms → hit 1.4ms byte-identical, FLUSHDB repair, DELETE purge
  verified in storage + Redis, 404s). See Step 5 deviations below.
- **Final acceptance — complete** (2026-07-07, on merged `main`). Booted the full compose
  stack (`make up` + `make migrate`), ran the README end-to-end flow: 15/15 checks passed
  (healthz, 401 auth gate, multipart 201, dedup same id, `?w=64&fmt=webp&q=80` → `image/webp`
  + `Cache-Control: public, max-age=31536000, immutable`, cache-hit byte-identical, `/meta`,
  bad-param 400, `from-url http://169.254.169.254/` → 400, DELETE 204, both read paths → 404
  after delete, idempotent second DELETE → 404). All six security requirements verified present
  in source; no `InsecureSkipVerify` anywhere.

## Current state

- Compose stack boots; `/healthz`, both uploads, and both reads (original/transformed +
  `/meta`) are live end-to-end. Only `DELETE` still 501s.
- OpenAPI spec at `docs/openapi/image-server.yaml` **already defines every endpoint**; the chi
  `ServerInterface`, models, wrapper, and embedded spec are generated in
  `internal/api/gen/server.gen.go`. **No spec edits are expected for any step below.**
- sqlc queries exist (`internal/db/`): `CreateImage`, `GetImage`, `GetImageByContentHash`,
  `DeleteImage`, all `RETURNING`, plus the `Image` model. `images` table has a UNIQUE constraint
  on `content_hash` (dedup).
- `internal/config/config.go` parses all env vars (APIKey, ListenAddr, DatabaseURL, RedisAddr,
  StoragePath, MaxUploadBytes, MaxPixels, RateLimitPerMin) with stdlib only.
- **Built:** `internal/storage/` (Step 2); `internal/imageproc/` detection, `internal/fetch/`,
  upload handlers, auth/rate-limit middleware, DB pool / Redis wiring in `main.go`, the
  apitest harness (Step 3); `internal/imageproc/` transform + apply (bimg) and the
  `GetImage`/`GetImageMeta` handlers (Step 4).
- **Missing:** derivative caching + `DeleteImage` (Step 5); `DELETE` currently 501.

## Delivery model

Four **sequential, stacked** PRs in the fixed `CLAUDE.md` order. Merge is a human gate, so each
branch is cut off the previous one. **Never batch layers into one PR** (`CLAUDE.md` rule). Each
step must leave the gates green and ends at an opened PR, then stop.

`feat/storage-local` → `feat/uploads` → `feat/transforms` → `feat/caching`

## Gates (identical at pre-push hook and CI — never weaken, never `--no-verify`)

- [ ] **Gate 1 — lint:** `make lint` (golangci-lint incl. gosec, noctx, bodyclose, unparam).
- [ ] **Gate 2 — coverage:** `./scripts/coverage-gate.sh` — **90%** per layer
      (`internal/api`, `internal/imageproc`, `internal/storage`, and add `internal/fetch`) and overall.
      Excludes `cmd/`, `internal/api/gen`, `internal/db`. Change thresholds only in this shared script.
- [ ] **Gate 3 — API spec:** `make test-api` (boots Postgres+Redis via compose, runs
      `go test -tags=apitest ./internal/api/...`).

> **Coverage gotcha (critical):** `coverage-gate.sh` runs **untagged** `go test -cover`, so
> `//go:build apitest` files contribute **nothing** to the 90% gate. All `internal/api` coverage
> must come from untagged unit tests → build the handlers behind fakeable seams (`imageStore`,
> `imageFetcher` interfaces) and use `alicebob/miniredis/v2` + `t.TempDir()` storage in unit tests.

Optional helper (if the `go-dev` / develop-go-feature skill is installed): `dgf-gates`,
`dgf-gates --coverage`, `dgf-gates --api`, `dgf-server start|url|stop`, `/test-api`. Everything is
reproducible with the `make` targets above if the skill is absent.

## Verified facts (checked against generated code — do not re-derive)

- **Auth hook:** every `/v1` wrapper in `server.gen.go` sets
  `ctx = context.WithValue(ctx, gen.ApiKeyAuthScopes, []string{})`; `Healthz` does **not**. Auth &
  rate-limit middleware detect a secured route by the presence of that context value → `/healthz`
  is public automatically.
- **Middleware order:** `HandlerWithOptions` applies `Middlewares` as `handler = mw(handler)` in
  slice order, so the **last** element is outermost (runs first). Use
  `Middlewares: []gen.MiddlewareFunc{s.rateLimitMiddleware, s.authMiddleware}` → auth runs before
  rate-limit (don't spend rate budget on unauthenticated requests).
- **Types:** `gen.ImageID` is a type alias of `github.com/google/uuid.UUID`, so `db.Image.ID` maps
  directly to `gen.Image.Id`. `gen.Image` fields: `Id, ContentHash, MimeType, OriginalFilename
  (string)`, `Width/Height (int)`, `SizeBytes (int64)`, `CreatedAt (time.Time)`. `db.Image` uses
  `int32` widths + `pgtype.Timestamptz` → convert (`.Time`).
- `*pgxpool.Pool` satisfies `db.DBTX`, so `db.New(pool)` works. `*db.Queries` satisfies the
  `imageStore` seam below.
- The generated wrappers do **not** parse request bodies — multipart/JSON parsing is the handler's job.

## Cross-cutting decisions (settled up front)

- [x] **`Storage.List` is in the interface from Step 2** (so Step 5 delete can purge derivatives
      authoritatively instead of trusting an evictable Redis set). `CLAUDE.md` interface snippet
      updated with `List`. **(done in Step 2)**
- [x] **SSRF + safe fetch → new `internal/fetch` package** (network/IP logic, injectable resolver,
      no real-network tests). Add `check_layer "Fetch" "./internal/fetch/..."` to
      `scripts/coverage-gate.sh` and the `CLAUDE.md` gate list. **(done in Step 3)**
- [x] **Magic-byte detection + bomb guard → `internal/imageproc`** using stdlib `image.DecodeConfig`
      (header-only: format from magic bytes + dimensions, no pixel decode). No libvips in Step 3.
      **(done in Step 3)**
- **Storage keys:** originals `originals/<hash[0:2]>/<hash[2:4]>/<hash>`; derivatives
  `derivatives/<image_id>/<transformHash>.<ext>`.
- **New deps** land with the step that needs them: `redis/go-redis/v9`, `go-redis/redis_rate/v10`,
  `golang.org/x/image` (webp), test-only `alicebob/miniredis/v2` + golang-migrate lib (Step 3);
  `h2non/bimg` (Step 4). Run `go mod tidy` before each gate run (pre-commit enforces no-op).

---

## Step 2 — `Storage` interface + local filesystem backend  ·  branch `feat/storage-local`  ·  ✅ COMPLETE

Pure package, no HTTP surface. Not wired into `main.go` this step (no consumer until Step 3).
**Status:** 3 commits on `feat/storage-local`, gates green, coverage 94.6%. PR #6 merged.

**Build**
- [x] `internal/storage/storage.go`: `Storage` interface (5 methods incl. `List`) + sentinel errors
      `ErrNotFound`, `ErrInvalidKey`.
- [x] `internal/storage/local.go`: `Local` struct, `NewLocal(root) (*Local, error)`, unexported
      `cleanKey`, methods, `var _ Storage = (*Local)(nil)`.
- [x] **Path-traversal guard, layer 1** (`cleanKey`): reject empty, NUL byte, backslash; require
      `filepath.IsLocal(key)`; reject `filepath.Clean(key) == "."`; return cleaned relative key.
- [x] **Path-traversal guard, layer 2:** all I/O through `os.OpenRoot(root)` → `*os.Root`
      (syscall-level rejection of traversal + symlink escape). Confirmed on Go 1.26: `Open`,
      `OpenFile`, `MkdirAll`, `Rename`, `Remove`, `Stat`, `FS`, `Name` all present; no `Root.CreateTemp`
      (temp names built manually).
- [x] **Atomic `Put`:** MkdirAll parent (0o750) → temp `<dir>/.tmp-<crypto/rand hex>` with
      `O_WRONLY|O_CREATE|O_EXCL` (0o600) → `io.Copy` → `Sync` → `Close` (check both) →
      `root.Rename(tmp, key)` → deferred best-effort temp cleanup on failure (`_ =` for errcheck).
      `contentType` intentionally unused (mime lives in DB) — commented.
- [x] **Error contract:** `Get` missing → `ErrNotFound` (converts `fs.ErrNotExist`); `Delete`
      idempotent (missing = nil); `Exists` missing → `(false, nil)`; invalid key → `ErrInvalidKey`;
      `List(prefix)` → opaque forward-slash keys, empty slice on no match.
- [x] gosec **G304**: production code passed clean (the `*os.Root` methods sidestep G304); no
      `//nosec` needed in `local.go`.

**Test (`internal/storage/local_test.go`, 94.6%, `t.TempDir()`, no mocks)**
- [x] `NewLocal`: creates missing dir; errors on empty root; errors when root is an existing file.
- [x] Key-sanitization table (`..`, `../etc/passwd`, `foo/../../bar`, `/etc/passwd`, leading `/`,
      NUL, backslash, `.`, valid `abc` / `derivatives/abc` / `a/b/c.jpg`, `foo/../bar`→`bar`);
      asserts `ErrInvalidKey` + outside canary untouched.
- [x] Symlink-escape blocked (symlink inside root → outside; Get/Put fail).
- [x] Round-trip: Put → Exists(true) → Get(bytes equal) → Delete → Exists(false) → Get(ErrNotFound).
- [x] Nested-dir create; overwrite; reader-error-mid-stream (Put errors AND no leftover `.tmp-*`);
      Delete idempotent; `List` (prefix filter, empty on no match, ignores temp files/other prefixes).
      Extra branch coverage: ops on a non-dir parent (ENOTDIR), rename-onto-directory, read-only-dir.
- [x] Permission-error tests guarded with `if os.Geteuid()==0 { t.Skip }`.

**Docs / finish**
- [x] Updated `CLAUDE.md` Storage interface snippet to include `List` (+ traversal-guard/sentinel note,
      status → step 2 complete).
- [x] Gates green (lint 0 issues, coverage gate storage 94.6%, `make test-api` pass, `-race` clean).
- [~] Open PR `feat/storage-local`: commits ready; **push + PR is manual** (remote access blocked
      for the current account). Body prepared in `docs/pr-feat-storage-local.md`.

**Deviation from plan (recorded):** the gosec finding surfaced in the **test file** (G302 chmod on
fixture dirs, G304 reading a temp file), not `local.go`. Resolved by excluding gosec from `_test.go`
in `.golangci.yml` (production code keeps full gosec coverage) rather than the anticipated
`//nosec G304` in `local.go`.

---

## Step 3 — Uploads + auth/rate-limit + DB/Redis wiring + apitest harness  ·  branch `feat/uploads`  ·  ✅ COMPLETE

Largest, most security-sensitive step. Off `main` (storage already merged). Implement `Healthz`,
`UploadImage`, `UploadImageFromURL`; embed `gen.Unimplemented` (other 3 endpoints → 501).

**Deviations from plan (recorded):** gosec flagged G120 (`ParseMultipartForm`) and G115
(int→int32) as false positives — resolved with a constant memory threshold + a `MaxInt32`
dimension guard and justified `//nolint:gosec` comments, not the anticipated G107 nolint
(unneeded: `http.NewRequestWithContext` + `client.Do` doesn't trigger G107). `main.go` wiring
landed with the plumbing commit (the router signature change would have broken the build
otherwise). Deferred P2s are listed in the PR body.

**Build — plumbing**
- [x] `internal/api/server.go`: `Server` struct (`gen.Unimplemented`, cfg, `storage.Storage`,
      `imageStore`, `*redis.Client`, `imageFetcher`) + `NewServer(...)`.
- [x] `internal/api/store.go`: `imageStore` interface (CreateImage/GetImageByContentHash/GetImage/
      DeleteImage — satisfied by `*db.Queries`) + `imageFetcher` interface (test seams).
- [x] `internal/api/router.go` (rewrite): mount `gen.HandlerWithOptions` with
      `Middlewares: {s.rateLimitMiddleware, s.authMiddleware}` (auth last = outermost),
      `ErrorHandlerFunc` (binding errors → 400 in `gen.Error` shape), + chi RequestID, Recoverer,
      slog request logger. Move `Healthz` onto the `Server`.
- [x] `internal/api/responses.go`: `writeError(w,status,code,msg)` → `gen.Error`;
      `toAPIImage(db.Image) → gen.Image` (Id: i.ID; int32→int; `.Time`).
- [x] `cmd/server/main.go`: `pgxpool.New`+`Ping` (`defer Close`), `redis.NewClient`+`Ping`
      (`defer Close`), `storage.NewLocal(cfg.StoragePath)`, `fetch.New(30s)`, pass all to `NewServer`.

**Build — image validation (`internal/imageproc/detect.go`)**
- [x] `DetectImage(data) → (format, mime, w, h, err)` via `image.DecodeConfig` (magic bytes +
      dims, no pixel decode); unsupported → sentinel (→ 415). Bomb-guard helper.

**Build — SSRF (`internal/fetch/`), the most security-sensitive code**
- [x] `isPublicIP(ip)`: reject loopback, `IsPrivate` (RFC1918 + ULA), link-local unicast/multicast
      (covers `169.254.169.254`), multicast, unspecified; unmap IPv4-mapped IPv6 first
      (`::ffff:169.254.169.254` must block); explicit metadata-IP assertions.
- [x] `SafeDialer{Resolver, Base}` with injectable `Resolver`; `DialContext` resolves host,
      validates **every** IP, then **dials the validated IP directly (pinned)** — no second lookup,
      closing the DNS-rebinding window. **Do not** validate-then-`http.Get` (reopens the window).
- [x] `http.Client`: that `DialContext`, **TLS verification ON**, `CheckRedirect` enforcing
      http/https + hop cap (each redirect re-dials → re-validates). Response body capped via
      `io.LimitReader(MaxUploadBytes+1)`; `defer resp.Body.Close()` (bodyclose). Justify gosec G107
      with `//nolint:gosec // URL validated + IP-pinned by SafeDialer`.

**Build — middleware (`internal/api/middleware.go`)**
- [x] `authMiddleware`: pass through if `ApiKeyAuthScopes` absent (`/healthz`); else
      `subtle.ConstantTimeCompare(X-API-Key, cfg.APIKey)` → 401 on mismatch.
- [x] `rateLimitMiddleware`: `go-redis/redis_rate/v10` GCRA, `PerMinute(cfg.RateLimitPerMin)`, key
      `ratelimit:<sha256(apiKey)[:16]>` (never store raw key); over → 429 + `Retry-After`; Redis-down
      → **fail-open** + error log. Only for secured routes.

**Build — handlers (`internal/api/upload.go`)**
- [x] `UploadImage` (ordering is load-bearing): `http.MaxBytesReader` **before** parse →
      `ParseMultipartForm` → `FormFile("file")` → `io.ReadAll` (overflow via
      `errors.As(*http.MaxBytesError)` → **413**; other → 400) → `DetectImage` (unsupported → **415**;
      ignore filename/Content-Type) → bomb guard `w*h > MaxPixels` → **400** → sha256 → dedup
      `GetImageByContentHash` (hit → 201 existing) → `storage.Put(originals/<sharded hash>)` →
      `CreateImage` (unique-violation `23505` via `errors.As(*pgconn.PgError)` → re-fetch, 201) → **201**.
- [x] `UploadImageFromURL`: bound JSON body → decode `gen.UploadImageFromURLJSONRequestBody` →
      `fetch.Fetch` (blocked/scheme → 400, oversize → 413, non-image → 415) → shared `ingest`
      (bomb, sha256, dedup, Put, CreateImage) → 201.

**Build — apitest harness (`//go:build apitest` in `internal/api/`)**
- [x] `harness_test.go` `TestMain`: read `DATABASE_URL`/`REDIS_ADDR`, open pool+rdb, apply
      migrations idempotently via golang-migrate **library** reading `../../migrations` (`make
      test-api` does not migrate). `newHarness(t)`: `storage.NewLocal(t.TempDir())`, real db+rdb,
      per-test cfg (tiny `MaxUploadBytes`, `RateLimitPerMin=1`), cleanup TRUNCATE + FlushDB.
- [x] Spec validation via `gen.GetSwagger()` + `gorillamux` router +
      `openapi3filter.ValidateRequest`/`ValidateResponse` around every call.
- [x] Extend untagged `spec_test.go::TestSpecIsValid` to also build the gorillamux router.

**Build — coverage gate**
- [x] Add `check_layer "Fetch" "./internal/fetch/..."` to `scripts/coverage-gate.sh`.

**Test (untagged, count toward gate)**
- [x] `internal/fetch/ssrf_test.go` (no network): `isPublicIP` table (v4 10/172.16/192.168/127/
      169.254.169.254/0.0.0.0/224.x; v6 ::1/fe80/fc00/fd00:ec2::254/::/IPv4-mapped; public 8.8.8.8,
      2606:4700::); `DialContext` with fake resolver (private blocks, mixed blocks, public dials
      validated IP via fake base dial); scheme rejection; redirect hop cap + redirect-to-internal.
- [x] `internal/imageproc/detect_test.go`: stdlib-encoded jpeg/png/gif (+ webp fixture) →
      format/mime/dims; truncated/garbage → unsupported (415); bomb-guard boundaries.
- [x] `internal/api/middleware_test.go`: auth (no/wrong/correct/public); rate-limit via miniredis
      (under/over + Retry-After/Redis-down fail-open).
- [x] `internal/api/upload_test.go` (fake `imageStore` + fake `imageFetcher` + `t.TempDir()` +
      miniredis): UploadImage 201/dedup 201/23505-race 201/missing-file 400/oversize 413/non-image
      415/bomb 400; UploadImageFromURL 201/blocked 400/oversize 413/non-image 415/bad-JSON 400.

**Test (apitest, spec-validated, not counted)**
- [x] multipart 201/401/415/413/429; from-url 400 (point at `http://169.254.169.254/` or
      `http://localhost/` — exercises the REAL guard) + 401. From-url happy path is covered by the
      unit test (fake fetcher); the real guard correctly blocks loopback — documented intentional split.

**Finish**
- [x] Gates green (`--coverage` + `make test-api`). Live check: start service, run `/test-api` (or
      manual curl) on upload endpoints. Diff-review (errors mapped, ctx propagated, SQL parameterized,
      authz on every /v1 route, no secrets in logs). Fix P0/P1. Open PR `feat/uploads`. **Stop.**

---

## Step 4 — Transform on read (`GET /{id}` + `/meta`)  ·  branch `feat/transforms`  ·  ✅ COMPLETE (merged, PR #9)

Off `main` (uploads already merged, not stacked as originally planned). Introduces `h2non/bimg`.

**Deviations from plan (recorded):** `contain` dims are computed in `Apply` (bimg with both
dims set fills the box, so the aspect-fit size is passed explicitly); the gosec finding was
**G705** (taint on serving transformed bytes) — resolved with a justified `//nolint` plus
defense-in-depth `X-Content-Type-Options: nosniff` on all image responses; `Apply` fixtures
are generated in-code with stdlib encoders (no `testdata/` dir needed); `transform.go` +
`apply.go` landed as one commit (the bimg go.mod change belongs with `apply.go` and the
pre-commit tidy check forbids splitting them); the router "unimplemented 501" test was
retargeted at `DELETE`, the last 501 endpoint; libvips installed locally via brew (8.18.4).
**Deferred P2** (in PR body): spec's GET 200 enumerates jpeg/png/webp but uploads accept
gif — a gif original serves as `image/gif` (correct, spec under-documents).

**Build**
- [x] `internal/imageproc/transform.go`: `Transform` struct (Width/Height/Format/Quality/Fit; zero =
      unset), `Format`/`Fit` typed consts, `Format.ContentType()`, `IsIdentity()`;
      `ParseTransform(url.Values) (Transform, error)` (pure, no bimg; enforce w≥1, h≥1, q∈1..100,
      fmt∈{jpeg,png,webp}, fit∈{cover,contain}; `*ParamError{Param,Msg}`). Normalization folded in →
      URL param order irrelevant by construction. (Take `url.Values`, not `gen.GetImageParams`, to
      keep imageproc free of generated code and be the single validation source of truth.)
- [x] `internal/imageproc/apply.go` (only bimg/cgo file): `Apply(src, t, maxPixels) → (out,
      contentType, err)`. `cover`→`Crop:true, Gravity:Centre`; `contain`→aspect-preserving fit
      (computed via `containDims`); unset Format→keep source type. Re-applies `MaxPixels` bomb
      guard on the source before processing.
- [x] `internal/api/images.go`: `GetImage` — `db.GetImage(id)` first (`pgx.ErrNoRows`→404) →
      `ParseTransform` (`*ParamError`→400) → `IsIdentity()` ? serve original (`storage.Get`, mime,
      immutable Cache-Control, `io.Copy`) : generate via `Apply` and serve (Step 5 wraps this in the
      cache). `GetImageMeta` — `db.GetImage` → `toAPIImage` → JSON, `Cache-Control: no-cache`.
- [x] `internal/config/config.go`: add `CacheControlMaxAge` (env `CACHE_CONTROL_MAX_AGE`, default 1y);
      update `.env.example`. Cache-Control for image bytes: `public, max-age=<n>, immutable` (never on errors).

**Test**
- [x] `ParseTransform` table — all `CLAUDE.md`-named cases: w/h valid≥1 + invalid (0/neg/non-int/
      empty); q boundaries 1/100 + invalid 0/101/non-int; each fmt + invalid + empty; fit
      cover/contain + invalid; combinations; identity → `IsIdentity()`.
- [x] `Apply` with in-code fixtures: resize, format conversion, quality, cover-vs-contain dims,
      bomb-guard rejection (runs under `make test`; libvips present locally + CI).
- [x] `internal/api` untagged: GetImage original-serve + generate-serve + 404 + 400 (+ storage-miss
      and corrupt-source 500s); GetImageMeta 200 + 404.
- [x] apitest: transform round-trip (upload→GET params→200 valid `image/*` + Cache-Control), meta,
      404, 401.
- [x] `config_test.go`: cover the new field.

**Finish**
- [x] Gates green (lint 0; API 94.5% / Imageproc 95.9% / overall 95.4%; `make test-api` pass);
      live smoke test on GET endpoints; diff-review clean (no P0/P1). PR #9 opened and **merged**.

---

## Step 5 — Derivative caching + `DELETE` purge  ·  branch `feat/caching`  ·  ✅ COMPLETE (merged, PR #10)

Off `main` (transforms already merged). Wrap Step 4's transform in a cache; implement `DeleteImage`.

**Deviations from plan (recorded):** the marker check uses Redis `EXISTS` rather than `GET`
(the value carries no information); extra tests beyond the plan: stale-marker regeneration
(marker present, object gone → regenerate not 500), Redis-down fail-open (storage.Exists
covers, no regeneration), and best-effort cleanup (broken storage → still 204); with every
endpoint implemented the `gen.Unimplemented` embed was removed in favor of a compile-time
`ServerInterface` assertion and the obsolete 501 router test deleted; lint round: SA4000 in
a determinism test + unparam on a test helper, both fixed in the tests. **Deferred P2** (PR
body): a GET racing a DELETE can leave an unservable orphan derivative in storage (Redis
state expires with TTL) — inherent to the documented best-effort design.

**Build**
- [x] `internal/imageproc/transform.go`: add `CacheKey(imageID uuid.UUID, t Transform) string` =
      `hex(sha256(imageID + "|" + canonical))`; canonical = fixed field order, unset → stable
      sentinel (`w=0`/`fmt=`); do not default `fmt` to source type in the key. Deterministic across
      runs (`?w=100&h=200` == `?h=200&w=100`). A pinned known-value test guards the serialization.
- [x] `internal/api/cache.go`: Redis helper (marker exists/set, per-image set add/enumerate/purge),
      all fail-open with logging.
- [x] `internal/api/images.go`: insert cache path into `GetImage` — compute `ck`+`derivKey` →
      Redis marker (hit → serve from storage) → miss: `storage.Exists(derivKey)` (repair:
      repopulate marker+set, serve) → true miss: `Get` original → `Apply` → `Put(derivKey)` →
      set marker + `SADD imgderivs:<id>` (both TTL `DerivativeCacheTTL`) → serve. (Storage is
      authoritative; Redis is a best-effort accelerator.)
- [x] `internal/api/images.go`: `DeleteImage` — `db.DeleteImage(id)` first (atomic
      `DELETE ... RETURNING`, no tx; `ErrNoRows`→404) → `storage.Delete(original)` (idempotent) →
      purge derivatives via `storage.List("derivatives/"+id+"/")` + Delete each → Redis `DEL` markers
      + `DEL imgderivs:<id>` → **204**. Post-DB steps best-effort (log, still 204).
- [x] `internal/config/config.go`: add `DerivativeCacheTTL` (env `DERIVATIVE_CACHE_TTL`, default 720h);
      `.env.example` + config_test.

**Test**
- [x] `CacheKey` determinism table (order-independence, distinct params/ids differ, sentinel
      stability, pinned known value).
- [x] `internal/api` untagged (miniredis + `t.TempDir()` + real bimg): cache MISS (generates, Put +
      marker + set populated); cache HIT (no regeneration — asserted via storage spy);
      Redis-evicted-storage-hit repair; stale-marker regeneration; Redis-down fail-open;
      `DeleteImage` purge (original + all derivatives via List + set + markers gone); 404 unknown
      id; DB error 500; 401; best-effort cleanup 204.
- [x] apitest: transform cache hit/miss end-to-end (byte-identical); delete-then-GET → 404 (both
      read paths); idempotent second DELETE → 404; DELETE 401; spec-validated.

**Finish**
- [x] Gates green (lint 0; API 92.7% / Imageproc 96.1% / overall 94.5%; `make test-api` pass);
      live check covering GET-with-params (hit/miss/repair) + DELETE purge; diff-review clean
      (no P0/P1). **PR #10 merged to `main`.**

---

## Final acceptance (after Step 5 merges)  ·  ✅ COMPLETE (2026-07-07)

- [x] `make up` + `make migrate`, then the README curl examples end-to-end: multipart upload → 201;
      re-upload same bytes → same record (dedup); `GET ?w=64&fmt=webp&q=80` → webp + Cache-Control;
      second identical GET served from cache (byte-identical); `GET /meta`; `DELETE` → 204; then
      `GET` → 404 (both read paths); idempotent second `DELETE` → 404;
      `from-url http://169.254.169.254/` → 400. 15/15 checks passed. (from-url happy path is
      covered by the unit test with a fake fetcher: the SSRF guard correctly blocks all local
      egress, so a live public fetch is not exercised in the local stack.)
- [x] Update README Roadmap: **Core HTTP service** box checked (commit d67ce7b).
- [x] All six `CLAUDE.md` security requirements implemented and tested (magic-byte via
      `image.DecodeConfig`, `http.MaxBytesReader`, bomb guard `MaxPixels`, SSRF pinned-dial +
      redirect re-check, path-traversal `os.Root`/`filepath.IsLocal`, parameterized sqlc queries +
      no secrets; no `InsecureSkipVerify` anywhere).

## Commit & PR discipline

- Conventional Commits; subject ≤ 50 (hard ≤ 72). One logical change per commit; split by category
  (migration/model, repository, service, handler+wiring, integration tests, doc). Never `--no-verify`.
- One PR per step, branched off the previous. PR body: summary, gate results as test plan, and a
  **Deferred (P2)** section. Stop after opening each PR (merge is a human gate).
- Version bump + tag/release is a separate human-gated step after all four PRs merge.

## Files touched (map)

- New: `internal/storage/{storage,local,local_test}.go`
- New: `internal/fetch/{ssrf,client,ssrf_test}.go`
- New: `internal/imageproc/{detect,transform,apply}.go` (+ tests, `testdata/`)
- New: `internal/api/{server,store,upload,middleware,responses,images,cache}.go` (+ untagged tests +
  `//go:build apitest` harness files)
- Rewrite: `internal/api/router.go`, `cmd/server/main.go`
- Edit: `scripts/coverage-gate.sh` (Fetch layer), `internal/config/config.go` (+2 fields),
  `.env.example`, `CLAUDE.md` (Storage `List` + Fetch gate), `README.md` (Roadmap checkbox)
- Reuse as-is: `internal/db/*` (behind `imageStore` seam), `internal/api/gen/*` (generated),
  `docs/openapi/image-server.yaml` (no changes expected)

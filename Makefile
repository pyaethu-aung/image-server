# Targets that need runtime config (run, migrate, test-api) source .env
# themselves; nothing is exported globally so unit tests stay hermetic.

# Pinned codegen tools (run via go run so no global install is needed)
OAPI_CODEGEN := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1
SQLC         := go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0
MIGRATE      := go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1

.PHONY: setup up down run migrate sqlc-gen openapi-gen test coverage test-api

## setup: wire git hooks (run once after clone)
setup:
	git config core.hooksPath .githooks
	@echo "✅ git hooks wired to .githooks/"

## up: build and boot the full stack (server + Postgres + Redis), detached
up:
	@test -f .env || { echo "❌ .env not found; run: cp .env.example .env"; exit 1; }
	docker compose up -d --build --wait

## down: stop the stack (volumes are kept; add -v manually to wipe data)
down:
	docker compose down

## run: start the server locally (expects compose services up)
run:
	@if [ -f .env ]; then set -a; . ./.env; set +a; fi; \
	go run ./cmd/server

## migrate: apply SQL migrations (DATABASE_URL from env, or .env as fallback)
migrate:
	@if [ -z "$$DATABASE_URL" ] && [ -f .env ]; then set -a; . ./.env; set +a; fi; \
	test -n "$$DATABASE_URL" || { echo "❌ DATABASE_URL is not set (export it or create .env)"; exit 1; }; \
	$(MIGRATE) -path migrations -database "$$DATABASE_URL" up

## sqlc-gen: regenerate database code from SQL queries
sqlc-gen:
	@if [ -f sqlc.yaml ]; then \
		$(SQLC) generate; \
	else \
		echo "⏭  sqlc-gen: sqlc.yaml not found (skipped until scaffold)"; \
	fi

## openapi-gen: regenerate chi server interfaces + models from the OpenAPI spec
openapi-gen:
	@mkdir -p internal/api/gen
	$(OAPI_CODEGEN) -config oapi-codegen.yaml docs/openapi/image-server.yaml

## test: run all unit tests with coverage profile
test:
	go test -v -coverprofile=coverage.out -covermode=atomic ./...

## coverage: print overall coverage (excludes cmd/, generated code)
coverage:
	@pkgs=$$(go list ./... 2>/dev/null | grep -v '/cmd/' | grep -v '/internal/api/gen' | grep -v '/internal/db' || true); \
	if [ -z "$$pkgs" ]; then \
		echo "⏭  coverage: no testable packages yet"; \
	else \
		echo "$$pkgs" | xargs go test -coverprofile=coverage.out -covermode=atomic > /dev/null; \
		go tool cover -func=coverage.out | grep '^total:'; \
	fi

## test-api: validate every endpoint against the OpenAPI spec (needs docker)
# Build tags are additive: this also re-runs the untagged unit tests in
# internal/api (accepted, they are fast). The apitest-tagged spec harness
# lands in implementation step 3; see the testing notes in CLAUDE.md.
test-api:
	@if [ ! -f docker-compose.yml ]; then \
		echo "⏭  test-api: docker-compose.yml not found (skipped until scaffold)"; \
	else \
		docker info > /dev/null 2>&1 || { echo "❌ test-api: docker is not running (needed for Postgres/Redis)"; exit 1; }; \
		docker compose up -d postgres redis && \
		{ if [ -f .env ]; then set -a; . ./.env; set +a; fi; \
		go test -v -tags=apitest ./internal/api/...; }; \
	fi

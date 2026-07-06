# Pinned codegen tools (run via go run so no global install is needed)
OAPI_CODEGEN := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1
SQLC         := go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0
MIGRATE      := go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1

.PHONY: setup run migrate sqlc-gen openapi-gen test coverage test-api

## setup: wire git hooks (run once after clone)
setup:
	git config core.hooksPath .githooks
	@echo "✅ git hooks wired to .githooks/"

## run: start the server locally (expects compose services up)
run:
	go run ./cmd/server

## migrate: apply SQL migrations (requires DATABASE_URL)
migrate:
	@test -n "$(DATABASE_URL)" || { echo "❌ DATABASE_URL is not set"; exit 1; }
	$(MIGRATE) -path migrations -database "$(DATABASE_URL)" up

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
test-api:
	@if [ ! -f docker-compose.yml ]; then \
		echo "⏭  test-api: docker-compose.yml not found (skipped until scaffold)"; \
	else \
		docker info > /dev/null 2>&1 || { echo "❌ test-api: docker is not running (needed for Postgres/Redis)"; exit 1; }; \
		docker compose up -d postgres redis && \
		go test -v -tags=apitest ./internal/api/...; \
	fi

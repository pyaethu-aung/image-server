#!/usr/bin/env bash
# Coverage gate: 90% minimum per layer and overall.
# Shared by .githooks/pre-push and .github/workflows/ci.yml so the local
# and CI thresholds can never drift.

set -euo pipefail

THRESHOLD=90
FAIL=0

check_layer() {
  local label="$1"
  local path="$2"

  # Skip layers that have no source files yet (incremental build-out)
  local dir="${path%/...}"
  if [[ -z "$(find "$dir" -name "*.go" ! -name "*_test.go" 2>/dev/null | head -1)" ]]; then
    echo "  ⏭  $label: no source files yet (skipped)"
    return
  fi

  output=$(go test -cover "$path" 2>&1)
  coverage=$(echo "$output" | grep -oE 'coverage: [0-9]+\.[0-9]+' | grep -oE '[0-9]+\.[0-9]+' | head -1)

  if [[ -z "$coverage" ]]; then
    echo "  ⚠️  $label: no test files found (0%)"
    FAIL=1
    return
  fi

  int_coverage=${coverage%.*}
  if (( int_coverage < THRESHOLD )); then
    echo "  ❌ $label: ${coverage}% (need ${THRESHOLD}%)"
    FAIL=1
  else
    echo "  ✅ $label: ${coverage}%"
  fi
}

check_layer "API"       "./internal/api/..."
check_layer "Imageproc" "./internal/imageproc/..."
check_layer "Storage"   "./internal/storage/..."
check_layer "Fetch"     "./internal/fetch/..."

# Overall coverage.
# Exclude: entrypoint (cmd/), oapi-codegen output (internal/api/gen),
# and sqlc-generated code (internal/db).
pkgs=$(go list ./... 2>/dev/null \
  | grep -v '/cmd/' \
  | grep -v '/internal/api/gen' \
  | grep -v '/internal/db' \
  || true)

if [[ -z "$pkgs" ]]; then
  echo "  ⏭  Overall: no testable packages yet (skipped)"
else
  echo "$pkgs" | xargs go test -coverprofile=coverage.out -covermode=atomic > /dev/null 2>&1
  overall=$(go tool cover -func=coverage.out | grep '^total:' | grep -oE '[0-9]+\.[0-9]+')
  int_overall=${overall%.*}
  if (( int_overall < THRESHOLD )); then
    echo "  ❌ Overall: ${overall}% (need ${THRESHOLD}%)"
    FAIL=1
  else
    echo "  ✅ Overall: ${overall}%"
  fi
fi

if [[ "$FAIL" -eq 1 ]]; then
  echo ""
  echo "❌ Coverage gate failed: fix failing layers before pushing."
  exit 1
fi

echo "✅ Coverage gate passed"

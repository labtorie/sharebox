#!/usr/bin/env bash
# Runs the automated test suites. See docs/testing.md.
set -euo pipefail
cd "$(dirname "$0")/.."
echo "==> Go unit + integration tests (race detector)"
go test -race -short ./...
echo "==> Large-file streaming (11 GiB sparse file)"
go test -run 'LargeFile|LargeStream' ./internal/fileserver ./internal/tunnel
echo "==> TypeScript tunnel <-> Go host interop"
go build -o bin/interop ./tests/interop
(cd apps/guest-web && SBX_INTEROP_BIN="$PWD/../../bin/interop" npx vitest run)
echo "==> Browser end-to-end (Google Chrome via Playwright)"
scripts/build.sh server cli >/dev/null
(cd apps/guest-web && npx playwright test)

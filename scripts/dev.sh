#!/usr/bin/env bash
# Local development: coordination server + TURN on localhost and the desktop
# app in Wails dev mode (hot reload), using a separate config directory.
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./cmd/sharebox-server -dev &
SERVER=$!
trap 'kill $SERVER' EXIT
export SHAREBOX_SERVER=http://127.0.0.1:8080
export SHAREBOX_CONFIG_DIR="${SHAREBOX_CONFIG_DIR:-$PWD/.dev-config}"
(cd apps/desktop && "${WAILS:-wails}" dev)

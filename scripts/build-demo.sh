#!/bin/sh
# Rebuilds the browser playground in docs/demo from the current source.
set -eu
cd "$(dirname "$0")/.."
GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o docs/demo/oakdb.wasm ./cmd/oakdb
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" docs/demo/
node tests/browser/smoke.mjs

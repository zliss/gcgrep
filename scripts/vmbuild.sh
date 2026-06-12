#!/bin/bash
# Sync the source tree to the build VM, run fmt/vet/tests there, then
# cross-compile binaries and test binaries for darwin/arm64 and
# windows/amd64 into ./dist (fetched back from the VM).
# Usage: scripts/vmbuild.sh [--quick]   (--quick skips tests)
set -euo pipefail
SRC="$(cd "$(dirname "$0")/.." && pwd)"
REMOTE=vm
RDIR='~/build/gcgrep'

rsync -a --delete --exclude dist --exclude .git "$SRC/" "$REMOTE:build/gcgrep/"

QUICK="${1:-}"
ssh "$REMOTE" "set -euo pipefail
cd $RDIR
export GOFLAGS=-mod=mod
go mod tidy
test -z \"\$(gofmt -l .)\" || { echo 'gofmt needed:'; gofmt -l .; exit 1; }
go vet ./...
if [ \"$QUICK\" != --quick ]; then go test ./... ; fi
mkdir -p dist
GOOS=linux   GOARCH=arm64 go build -trimpath -o dist/gcgrep-linux-arm64   ./cmd/gcgrep
GOOS=darwin  GOARCH=arm64 go build -trimpath -o dist/gcgrep-darwin-arm64  ./cmd/gcgrep
GOOS=windows GOARCH=amd64 go build -trimpath -o dist/gcgrep-windows-amd64.exe ./cmd/gcgrep
for pkg in index ignore daemon symbol; do
  GOOS=darwin  GOARCH=arm64 go test -c -o dist/\${pkg}_test_darwin  ./internal/\$pkg
  GOOS=windows GOARCH=amd64 go test -c -o dist/\${pkg}_test_windows.exe ./internal/\$pkg
done
"
mkdir -p "$SRC/dist"
rsync -a "$REMOTE:build/gcgrep/dist/" "$SRC/dist/"
rsync -a "$REMOTE:build/gcgrep/go.sum" "$REMOTE:build/gcgrep/go.mod" "$SRC/" 2>/dev/null || \
  scp -q "$REMOTE:build/gcgrep/go.sum" "$REMOTE:build/gcgrep/go.mod" "$SRC/"
echo "OK: binaries in dist/"

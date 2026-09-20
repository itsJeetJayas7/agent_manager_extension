#!/usr/bin/env bash
# Build the Agent Conductor daemon for the host platform (and release targets
# when AC_RELEASE=1) into dist-bin/.
set -euo pipefail
cd "$(dirname "$0")/.."

OUT=dist-bin
mkdir -p "$OUT"

if command -v go >/dev/null 2>&1; then GO=go
elif [ -x /opt/homebrew/bin/go ]; then GO=/opt/homebrew/bin/go
elif [ -x /usr/local/go/bin/go ]; then GO=/usr/local/go/bin/go
else echo "go toolchain not found" >&2; exit 1; fi

cd daemon
$GO vet ./...
build() {
  local os=$1 arch=$2 name=$3
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch $GO build -trimpath -ldflags "-s -w" -o "../$OUT/$name" ./cmd/agent-conductor
  echo "built $OUT/$name"
}

case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) build darwin arm64 agent-conductor-darwin-arm64 ;;
  Darwin/x86_64) build darwin amd64 agent-conductor-darwin-amd64 ;;
  Linux/x86_64) build linux amd64 agent-conductor-linux-amd64 ;;
  Linux/aarch64) build linux arm64 agent-conductor-linux-arm64 ;;
  *) echo "unsupported host"; exit 1 ;;
esac

if [ "${AC_RELEASE:-0}" = "1" ]; then
  build darwin arm64 agent-conductor-darwin-arm64
  build darwin amd64 agent-conductor-darwin-amd64
  build linux amd64 agent-conductor-linux-amd64
  build linux arm64 agent-conductor-linux-arm64
fi

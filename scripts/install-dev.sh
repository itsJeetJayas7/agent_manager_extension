#!/usr/bin/env bash
# Use the same platform-aware build and package path as a release.
set -euo pipefail
cd "$(dirname "$0")/.."
npm ci
npm run package
VERSION=$(node -p 'require("./package.json").version')
CODE_BIN="$(command -v code || true)"
if [[ -z "$CODE_BIN" && -x "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code" ]]; then
  CODE_BIN="/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"
fi
if [[ -z "$CODE_BIN" ]]; then
  echo "VSIX ready: agent-conductor-$VERSION.vsix; install with Extensions: Install from VSIX…"
else
  "$CODE_BIN" --install-extension "agent-conductor-$VERSION.vsix" --force
fi

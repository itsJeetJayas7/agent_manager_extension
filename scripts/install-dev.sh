#!/usr/bin/env bash
# Developer setup: install npm deps, build daemon + extension, install the
# VSIX into the local VS Code.
set -euo pipefail
cd "$(dirname "$0")/.."

npm install
bash scripts/build-daemon.sh
npm run build:ext
npm run build:webview
mkdir -p dist/bin
cp "dist-bin/agent-conductor-"* dist/bin/agent-conductor
chmod +x dist/bin/agent-conductor
echo "dev build ready — press F5 in VS Code for the Extension Development Host"

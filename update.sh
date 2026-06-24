#!/usr/bin/env bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_BIN="$HOME/go-dist/go/bin/go"

echo "==> Pulling latest changes..."
cd "$SCRIPT_DIR"
git pull origin main

echo "==> Rebuilding..."
"$GO_BIN" build -o mtproto-checker .

echo "==> Done! Run ./mtproto-checker to start."

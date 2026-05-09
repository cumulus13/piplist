#!/usr/bin/env bash
set -e

BINARY="piplist"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Building $BINARY..."
cd "$SOURCE_DIR"

if ! command -v go &>/dev/null; then
  echo "ERROR: Go is not installed. Please install Go first: https://go.dev/dl/"
  exit 1
fi

go build -ldflags="-s -w" -o "$BINARY" .

echo "==> Installing to $INSTALL_DIR/$BINARY ..."
if [ -w "$INSTALL_DIR" ]; then
  cp "$BINARY" "$INSTALL_DIR/$BINARY"
else
  sudo cp "$BINARY" "$INSTALL_DIR/$BINARY"
fi

echo ""
echo "✓ Installed! Try it:"
echo "  piplist"
echo "  piplist -g requests"
echo "  piplist flask"

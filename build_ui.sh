#!/bin/bash

set -e

echo "=== Building OlcRTC ==="
echo ""

# Build olcrtc binary
echo "[1/2] Building olcrtc binary..."
cd "$(dirname "$0")"
go build -o olcrtc ./cmd/olcrtc/main.go
echo "✓ olcrtc binary built: ./olcrtc"

# Build UI binary
echo ""
echo "[2/2] Building UI binary..."
cd ui
go build -o ../ui .
cd ..
echo "✓ UI binary built: ./ui"

echo ""
echo "=== Build Complete ==="
echo "Binaries ready:"
echo "  - ./olcrtc"
echo "  - ./ui"

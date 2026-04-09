#!/bin/bash

set -e

CONTAINER_NAME="olcrtc-server"
IMAGE_NAME="docker.io/library/golang:1.26-alpine"
REPO_URL="https://github.com/openlibrecommunity/olcrtc.git"
WORK_DIR="/tmp/olcrtc-deploy"

echo "=== OlcRTC Server Deployment Script ==="
echo ""

if ! command -v podman &> /dev/null; then
    echo "[!] Installing Podman..."
    
    if command -v apt &> /dev/null; then
        echo "[*] Detected apt (Debian/Ubuntu)"
        sudo apt update
        sudo apt install -y podman
    elif command -v dnf &> /dev/null; then
        echo "[*] Detected dnf (Fedora/RHEL)"
        sudo dnf install -y podman
    elif command -v yum &> /dev/null; then
        echo "[*] Detected yum (CentOS/RHEL)"
        sudo yum install -y podman
    elif command -v pacman &> /dev/null; then
        echo "[*] Detected pacman (Arch)"
        sudo pacman -Sy --noconfirm podman
    else
        echo "[X] Unsupported package manager. Install podman manually."
        exit 1
    fi
fi

echo "[+] Using Podman"
echo ""
read -p "Enter Telemost Room ID: " ROOM_ID

if [ -z "$ROOM_ID" ]; then
    echo "[X] Room ID cannot be empty"
    exit 1
fi

echo ""
echo "[*] Stopping old instance..."
podman stop $CONTAINER_NAME 2>/dev/null || true
podman rm $CONTAINER_NAME 2>/dev/null || true

echo "[*] Cleaning workspace..."
rm -rf $WORK_DIR
mkdir -p $WORK_DIR

echo "[*] Cloning repository..."
git clone --depth 1 $REPO_URL $WORK_DIR

echo "[*] Pulling Go image..."
podman pull $IMAGE_NAME

echo "[*] Building OlcRTC..."
podman run --rm \
    -v $WORK_DIR:/app:Z \
    -w /app \
    $IMAGE_NAME \
    sh -c "apk add --no-cache git && go mod tidy && go build -o olcrtc cmd/olcrtc/main.go"

if [ ! -f "$WORK_DIR/olcrtc" ]; then
    echo "[X] Build failed"
    exit 1
fi

KEY_FILE="$HOME/.olcrtc_key"

if [ -f "$KEY_FILE" ]; then
    echo "[*] Loading existing encryption key..."
    KEY=$(cat "$KEY_FILE")
else
    echo "[*] Generating new encryption key..."
    KEY=$(openssl rand -hex 32)
    echo "$KEY" > "$KEY_FILE"
    chmod 600 "$KEY_FILE"
    echo ""
    echo "=========================================="
    echo "NEW ENCRYPTION KEY (saved to $KEY_FILE):"
    echo "$KEY"
    echo "=========================================="
    echo ""
fi

echo "[*] Starting OlcRTC server..."
podman run -d \
    --name $CONTAINER_NAME \
    --restart unless-stopped \
    -v $WORK_DIR:/app:Z \
    -w /app \
    $IMAGE_NAME \
    ./olcrtc -mode srv -id "$ROOM_ID" -key "$KEY"

sleep 2

echo ""
echo "[+] Server started successfully!"
echo ""
echo "Container name: $CONTAINER_NAME"
echo "Room ID: $ROOM_ID"
echo "Encryption key: $KEY"
echo ""
echo "View logs:"
echo "  podman logs -f $CONTAINER_NAME"
echo ""
echo "Stop server:"
echo "  podman stop $CONTAINER_NAME"
echo ""
echo "Client command:"
echo "  ./olcrtc -mode cnc -id \"$ROOM_ID\" -key \"$KEY\" -socks-port 1080"
echo ""

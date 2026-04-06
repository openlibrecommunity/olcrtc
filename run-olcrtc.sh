#!/bin/bash

set -e

CONTAINER_NAME="olcrtc-server"
IMAGE_NAME="golang:1.23-alpine"
REPO_URL="https://github.com/zarazaex69/olcrtc.git"
WORK_DIR="/tmp/olcrtc-deploy"

echo "=== OlcRTC Server Deployment Script ==="
echo ""

if command -v podman &> /dev/null; then
    RUNTIME="podman"
    echo "[+] Using Podman"
elif command -v docker &> /dev/null; then
    RUNTIME="docker"
    echo "[+] Using Docker"
else
    echo "[!] Installing container runtime..."
    
    if command -v apt &> /dev/null; then
        echo "[*] Detected apt (Debian/Ubuntu)"
        sudo apt update
        sudo apt install -y podman
        RUNTIME="podman"
    elif command -v dnf &> /dev/null; then
        echo "[*] Detected dnf (Fedora/RHEL)"
        sudo dnf install -y podman
        RUNTIME="podman"
    elif command -v yum &> /dev/null; then
        echo "[*] Detected yum (CentOS/RHEL)"
        sudo yum install -y podman
        RUNTIME="podman"
    elif command -v pacman &> /dev/null; then
        echo "[*] Detected pacman (Arch)"
        sudo pacman -Sy --noconfirm podman
        RUNTIME="podman"
    else
        echo "[X] Unsupported package manager. Install podman or docker manually."
        exit 1
    fi
fi

echo ""
read -p "Enter Telemost Room ID: " ROOM_ID

if [ -z "$ROOM_ID" ]; then
    echo "[X] Room ID cannot be empty"
    exit 1
fi

echo ""
echo "[*] Stopping old instance..."
$RUNTIME stop $CONTAINER_NAME 2>/dev/null || true
$RUNTIME rm $CONTAINER_NAME 2>/dev/null || true

echo "[*] Cleaning workspace..."
rm -rf $WORK_DIR
mkdir -p $WORK_DIR

echo "[*] Cloning repository..."
git clone --depth 1 $REPO_URL $WORK_DIR

echo "[*] Pulling Go image..."
$RUNTIME pull $IMAGE_NAME

echo "[*] Building OlcRTC..."
$RUNTIME run --rm \
    -v $WORK_DIR:/app:Z \
    -w /app \
    $IMAGE_NAME \
    sh -c "apk add --no-cache git && go build -o olcrtc cmd/olcrtc/main.go"

if [ ! -f "$WORK_DIR/olcrtc" ]; then
    echo "[X] Build failed"
    exit 1
fi

echo "[*] Generating encryption key..."
KEY=$(openssl rand -hex 32)
echo ""
echo "=========================================="
echo "ENCRYPTION KEY (save this!):"
echo "$KEY"
echo "=========================================="
echo ""

echo "[*] Starting OlcRTC server..."
$RUNTIME run -d \
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
echo "  $RUNTIME logs -f $CONTAINER_NAME"
echo ""
echo "Stop server:"
echo "  $RUNTIME stop $CONTAINER_NAME"
echo ""
echo "Client command:"
echo "  ./olcrtc -mode cnc -id \"$ROOM_ID\" -key \"$KEY\" -socks-port 1080"
echo ""

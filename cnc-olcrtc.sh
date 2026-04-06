#!/bin/bash

set -e

CONTAINER_NAME="olcrtc-client"
IMAGE_NAME="golang:1.23-alpine"
REPO_URL="https://github.com/zarazaex69/olcrtc.git"
WORK_DIR="/tmp/olcrtc-client"
SOCKS_PORT="8808"

echo "=== OlcRTC Client Deployment Script ==="
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
read -p "Enter Encryption Key (hex): " KEY

if [ -z "$KEY" ]; then
    echo "[X] Encryption key cannot be empty"
    exit 1
fi

echo ""
read -p "SOCKS5 port [default: 8808]: " PORT_INPUT
SOCKS_PORT=${PORT_INPUT:-8808}

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

echo "[*] Starting OlcRTC client..."
$RUNTIME run -d \
    --name $CONTAINER_NAME \
    --restart unless-stopped \
    -p 127.0.0.1:$SOCKS_PORT:$SOCKS_PORT \
    -v $WORK_DIR:/app:Z \
    -w /app \
    $IMAGE_NAME \
    ./olcrtc -mode cnc -id "$ROOM_ID" -key "$KEY" -socks-port $SOCKS_PORT

sleep 2

echo ""
echo "[+] Client started successfully!"
echo ""
echo "Container name: $CONTAINER_NAME"
echo "Room ID: $ROOM_ID"
echo "SOCKS5 proxy: 127.0.0.1:$SOCKS_PORT"
echo ""
echo "View logs:"
echo "  $RUNTIME logs -f $CONTAINER_NAME"
echo ""
echo "Stop client:"
echo "  $RUNTIME stop $CONTAINER_NAME"
echo ""
echo "Test proxy:"
echo "  export all_proxy=socks5h://127.0.0.1:$SOCKS_PORT"
echo "  curl -fsSL https://ifconfig.me"
echo ""

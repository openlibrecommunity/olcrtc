#!/bin/bash
set -e

CONTAINER_NAME="olcrtc-client"
IMAGE_NAME="docker.io/library/golang:1.26-alpine"
REPO_URL="https://github.com/openlibrecommunity/olcrtc.git"
WORK_DIR="/tmp/olcrtc-client"

SOCKS_IP="127.0.0.1"
SOCKS_PORT="8808"

echo "=== OlcRTC Client Deployment Script (Docker) ==="
echo ""

if ! command -v docker &> /dev/null; then
    echo "[!] Installing Docker..."

    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
    elif command -v sudo &> /dev/null; then
        SUDO="sudo"
    elif command -v doas &> /dev/null; then
        SUDO="doas"
    else
        echo "[X] No sudo/doas found and not running as root. Cannot install docker."
        exit 1
    fi

    if command -v apt &> /dev/null; then
        echo "[*] Detected apt (Debian/Ubuntu)"
        $SUDO apt update
        $SUDO apt install -y docker.io
    elif command -v dnf &> /dev/null; then
        echo "[*] Detected dnf (Fedora/RHEL)"
        $SUDO dnf install -y docker
    elif command -v yum &> /dev/null; then
        echo "[*] Detected yum (CentOS/RHEL)"
        $SUDO yum install -y docker
    elif command -v pacman &> /dev/null; then
        echo "[*] Detected pacman (Arch)"
        $SUDO pacman -Sy --noconfirm docker
    else
        echo "[X] Unsupported package manager. Install docker manually."
        exit 1
    fi

    echo "[*] Starting Docker service..."
    $SUDO systemctl enable --now docker || true
fi

echo "[+] Using Docker"
echo ""
echo "Select provider:"
echo "  1) telemost"
echo "  2) jazz"
read -p "Enter choice [1-2, default: 1]: " PROVIDER_CHOICE

case "$PROVIDER_CHOICE" in
    2)
        PROVIDER="jazz"
        ;;
    *)
        PROVIDER="telemost"
        ;;
esac

echo "[*] Using provider: $PROVIDER"
echo ""

if [ "$PROVIDER" = "jazz" ]; then
    read -p "Enter Room ID (format: roomId:password from server): " ROOM_ID
    if [ -z "$ROOM_ID" ]; then
        echo "[X] Room ID cannot be empty"
        exit 1
    fi
else
    read -p "Enter Room ID: " ROOM_ID
    if [ -z "$ROOM_ID" ]; then
        echo "[X] Room ID cannot be empty"
        exit 1
    fi
fi

echo ""
read -p "Enter Encryption Key (hex): " KEY
if [ -z "$KEY" ]; then
    echo "[X] Encryption key cannot be empty"
    exit 1
fi
echo ""
read -p "SOCKS5 ip [default: 127.0.0.1]: " IP_INPUT
SOCKS_IP=${IP_INPUT:-127.0.0.1}

echo ""
read -p "SOCKS5 port [default: 8808]: " PORT_INPUT
SOCKS_PORT=${PORT_INPUT:-8808}

echo ""
echo "[*] Stopping old instance..."
docker stop $CONTAINER_NAME 2>/dev/null || true
docker rm $CONTAINER_NAME 2>/dev/null || true

echo "[*] Cleaning workspace..."
rm -rf $WORK_DIR
mkdir -p $WORK_DIR

echo "[*] Cloning repository..."
git clone --depth 1 $REPO_URL $WORK_DIR

echo "[*] Pulling Go image..."
docker pull $IMAGE_NAME

echo "[*] Building OlcRTC..."
docker run --rm \
    -v $WORK_DIR:/app \
    -w /app \
    $IMAGE_NAME \
    sh -c "go mod tidy && go build -o olcrtc cmd/olcrtc/main.go"

if [ ! -f "$WORK_DIR/olcrtc" ]; then
    echo "[X] Build failed"
    exit 1
fi

echo "[*] Starting OlcRTC client..."
docker run -d \
    --name $CONTAINER_NAME \
    --restart unless-stopped \
    -p $SOCKS_IP:$SOCKS_PORT:$SOCKS_PORT \
    -v $WORK_DIR:/app \
    -w /app \
    $IMAGE_NAME \
    ./olcrtc -mode cnc -provider "$PROVIDER" -id "$ROOM_ID" -key "$KEY" -socks-port $SOCKS_PORT -socks-host 0.0.0.0

sleep 2

echo ""
echo "[+] Client started successfully!"
echo ""
echo "Container name: $CONTAINER_NAME"
echo "Provider: $PROVIDER"
echo "Room ID: $ROOM_ID"
echo "SOCKS5 proxy: $SOCKS_IP:$SOCKS_PORT"
echo ""
echo "View logs:"
echo "  docker logs -f $CONTAINER_NAME"
echo ""
echo "Stop client:"
echo "  docker stop $CONTAINER_NAME"
echo ""
echo "Test proxy:"
echo "  export all_proxy=socks5h://$SOCKS_IP:$SOCKS_PORT"
echo "  curl -fsSL https://ifconfig.me"
echo ""

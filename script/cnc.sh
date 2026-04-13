#!/bin/bash

echo "ЕСЛИ У ВАС ЕСТЬ ПРОБЛЕМЫ - Я В КУРСЕ, ПРОЕКТ В БЕТЕ, ПО ПРОБЛЕМАМ В ЧАТ t.me/openlibrecommunity ИЛИ ВООБЩЕ НЕКУДА, ЖДИТЕ РЕЛИЗА"


set -e

CONTAINER_NAME="olcrtc-client"
IMAGE_NAME="docker.io/library/golang:1.26-alpine"
REPO_URL="https://github.com/openlibrecommunity/olcrtc.git"
WORK_DIR="/tmp/olcrtc-client"
SOCKS_PORT="8808"

echo "=== OlcRTC Client Deployment Script ==="
echo ""

if ! command -v podman &> /dev/null; then
    echo "[!] Installing Podman..."

    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
    elif command -v sudo &> /dev/null; then
        SUDO="sudo"
    elif command -v doas &> /dev/null; then
        SUDO="doas"
    else
        echo "[X] No sudo/doas found and not running as root. Cannot install podman."
        exit 1
    fi

    if command -v apt &> /dev/null; then
        echo "[*] Detected apt (Debian/Ubuntu)"
        $SUDO apt update
        $SUDO apt install -y podman
    elif command -v dnf &> /dev/null; then
        echo "[*] Detected dnf (Fedora/RHEL)"
        $SUDO dnf install -y podman
    elif command -v yum &> /dev/null; then
        echo "[*] Detected yum (CentOS/RHEL)"
        $SUDO yum install -y podman
    elif command -v pacman &> /dev/null; then
        echo "[*] Detected pacman (Arch)"
        $SUDO pacman -Sy --noconfirm podman
    else
        echo "[X] Unsupported package manager. Install podman manually."
        exit 1
    fi
fi

echo "[+] Using Podman"
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
    echo "Jazz room options:"
    echo "  1) Connect to existing room (enter roomId:password)"
    echo "  2) Auto-generate room (server will create)"
    read -p "Enter choice [1-2, default: 1]: " JAZZ_CHOICE
    
    case "$JAZZ_CHOICE" in
        2)
            ROOM_ID="any"
            echo "[*] Will auto-generate Jazz room"
            ;;
        *)
            read -p "Enter Room ID (format: roomId:password): " ROOM_ID
            if [ -z "$ROOM_ID" ]; then
                echo "[X] Room ID cannot be empty"
                exit 1
            fi
            ;;
    esac
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
read -p "SOCKS5 port [default: 8808]: " PORT_INPUT
SOCKS_PORT=${PORT_INPUT:-8808}

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
    sh -c "go mod tidy && go build -o olcrtc cmd/olcrtc/main.go"

if [ ! -f "$WORK_DIR/olcrtc" ]; then
    echo "[X] Build failed"
    exit 1
fi

echo "[*] Starting OlcRTC client..."
podman run -d \
    --name $CONTAINER_NAME \
    --restart unless-stopped \
    -p 127.0.0.1:$SOCKS_PORT:$SOCKS_PORT \
    -v $WORK_DIR:/app:Z \
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
echo "SOCKS5 proxy: 127.0.0.1:$SOCKS_PORT"
echo ""
echo "View logs:"
echo "  podman logs -f $CONTAINER_NAME"
echo ""
echo "Stop client:"
echo "  podman stop $CONTAINER_NAME"
echo ""
echo "Test proxy:"
echo "  export all_proxy=socks5h://127.0.0.1:$SOCKS_PORT"
echo "  curl -fsSL https://ifconfig.me"
echo ""

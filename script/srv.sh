#!/bin/bash

echo "ЕСЛИ У ВАС ЕСТЬ ПРОБЛЕМЫ - Я В КУРСЕ, ПРОЕКТ В БЕТЕ, ПО ПРОБЛЕМАМ В ЧАТ t.me/openlibrecommunity ИЛИ ВООБЩЕ НЕКУДА, ЖДИТЕ РЕЛИЗА"

set -e

CONTAINER_NAME="olcrtc-server"
IMAGE_NAME="docker.io/library/golang:1.26-alpine"
REPO_URL="https://github.com/openlibrecommunity/olcrtc.git"
WORK_DIR="/tmp/olcrtc-deploy"

echo "=== OlcRTC Server Deployment Script ==="
echo ""

if ! command -v podman &> /dev/null; then
    echo "[!] Installing Podman..."

    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
    else
        SUDO="sudo"
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
    echo "  1) Auto-generate new room (recommended)"
    echo "  2) Use specific room ID (enter roomId:password)"
    read -p "Enter choice [1-2, default: 1]: " JAZZ_CHOICE
    
    case "$JAZZ_CHOICE" in
        2)
            read -p "Enter Room ID (format: roomId:password): " ROOM_ID
            if [ -z "$ROOM_ID" ]; then
                echo "[X] Room ID cannot be empty"
                exit 1
            fi
            ;;
        *)
            ROOM_ID="any"
            echo "[*] Will auto-generate Jazz room"
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
read -p "Use SOCKS5 proxy for egress? (y/N): " USE_PROXY

EXTRA_ARGS=()

if [[ "$USE_PROXY" =~ ^[Yy]$ ]]; then
    read -p "Enter SOCKS5 proxy address [default: 127.0.0.1]: " PROXY_ADDR_INPUT
    SOCKS_PROXY_ADDR=${PROXY_ADDR_INPUT:-127.0.0.1}

    read -p "Enter SOCKS5 proxy port [default: 1080]: " PROXY_PORT_INPUT
    SOCKS_PROXY_PORT=${PROXY_PORT_INPUT:-1080}

    echo "[*] Will use SOCKS5 proxy: $SOCKS_PROXY_ADDR:$SOCKS_PROXY_PORT"
    EXTRA_ARGS+=(-socks-proxy "$SOCKS_PROXY_ADDR" -socks-proxy-port "$SOCKS_PROXY_PORT")
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
    sh -c "go mod tidy && go build -o olcrtc cmd/olcrtc/main.go"

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
    ./olcrtc -mode srv -provider "$PROVIDER" -id "$ROOM_ID" -key "$KEY" "${EXTRA_ARGS[@]}"

sleep 2

echo ""
echo "[+] Server started successfully!"
echo ""
echo "Container name: $CONTAINER_NAME"
echo "Provider:       $PROVIDER"
echo "Room ID:        $ROOM_ID"
echo "Encryption key: $KEY"

if [ ${#EXTRA_ARGS[@]} -gt 0 ]; then
    echo "SOCKS5 proxy:   $SOCKS_PROXY_ADDR:$SOCKS_PROXY_PORT"
fi

echo ""
echo "View logs:"
echo "  podman logs -f $CONTAINER_NAME"
echo ""
echo "Stop server:"
echo "  podman stop $CONTAINER_NAME"
echo ""
echo "Client command:"
echo "  ./olcrtc -mode cnc -provider \"$PROVIDER\" -id \"$ROOM_ID\" -key \"$KEY\" -socks-port 1080"
echo ""
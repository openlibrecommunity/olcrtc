#!/bin/bash

echo "ЕСЛИ У ВАС ЕСТЬ ПРОБЛЕМЫ - Я В КУРСЕ, ПРОЕКТ В БЕТЕ, ПО ПРОБЛЕМАМ В ЧАТ t.me/openlibrecommunity ИЛИ ВООБЩЕ НЕКУДА, ЖДИТЕ РЕЛИЗА"

set -e

PODMAN_ID=$(tr -dc 'a-z0-9' </dev/urandom | head -c 8)
CONTAINER_NAME="olcrtc-server-$PODMAN_ID"
IMAGE_NAME="docker.io/library/golang:1.26-alpine3.22"
REPO_URL="https://github.com/openlibrecommunity/olcrtc.git"
WORK_DIR="/tmp/olcrtc-deploy-$PODMAN_ID"
BRANCH="master"
NO_CACHE=0

while [[ $# -gt 0 ]]; do
    case $1 in
        --branch=*)
            BRANCH="${1#*=}"
            shift
            ;;
        --no-cache)
            NO_CACHE=1
            shift
            ;;
        *)
            shift
            ;;
    esac
done

echo "=== OlcRTC Server Deployment Script ==="
echo ""
echo "[*] Using branch: $BRANCH"
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

validate_key() {
    case "$1" in
        *[!0-9a-fA-F]*)
            return 1
            ;;
    esac
    [ "${#1}" -eq 64 ]
}

echo "Select carrier:"
echo "  1) jitsi"
echo "  2) telemost"
echo "  3) wbstream"
read -p "Enter choice [1-3, default: 1]: " CARRIER_CHOICE

case "$CARRIER_CHOICE" in
    2)
        CARRIER="telemost"
        ;;
    3)
        CARRIER="wbstream"
        ;;
    *)
        CARRIER="jitsi"
        ;;
esac

echo "[*] Using carrier: $CARRIER"
echo ""

WB_TOKEN=""
if [ "$CARRIER" = "wbstream" ]; then
    echo "wbstream account token (auth.token), optional."
    echo "Empty = anonymous guest. Required for datachannel (needs moderator rights, canPublishData=true)."
    read -p "wbstream auth.token (Enter to skip): " WB_TOKEN
    echo ""
fi

echo "Select transport:"
echo "  1) datachannel"
echo "  2) videochannel"
echo "  3) seichannel"
echo "  4) vp8channel"
read -p "Enter choice [1-4, default: 1]: " TRANSPORT_CHOICE

case "$TRANSPORT_CHOICE" in
    2)
        TRANSPORT="videochannel"
        ;;
    3)
        TRANSPORT="seichannel"
        ;;
    4)
        TRANSPORT="vp8channel"
        ;;
    *)
        TRANSPORT="datachannel"
        ;;
esac

echo "[*] Using transport: $TRANSPORT"
echo ""

GEN_ROOM=0

if [ "$CARRIER" = "jitsi" ]; then
    echo ""
    echo "Выберите Jitsi-сервер (проверьте в браузере, какой работает в вашей сети):"
    echo "  1) https://meet.small-dm.ru/"
    echo "  2) https://meet1.arbitr.ru/"
    echo "  3) https://meet.handyweb.org/"
    echo "  4) Другой (ввести вручную)"
    read -p "Введите номер [1-4, по умолчанию: 1]: " JITSI_SERVER_CHOICE

    case "$JITSI_SERVER_CHOICE" in
        2)
            JITSI_BASE_URL="https://meet1.arbitr.ru"
            ;;
        3)
            JITSI_BASE_URL="https://meet.handyweb.org"
            ;;
        4)
            read -p "Введите URL Jitsi-сервера: " JITSI_BASE_INPUT
            JITSI_BASE_URL="${JITSI_BASE_INPUT%/}"
            if [ -z "$JITSI_BASE_URL" ]; then
                echo "[X] URL не может быть пустым"
                exit 1
            fi
            ;;
        *)
            JITSI_BASE_URL="https://meet.small-dm.ru"
            ;;
    esac

    echo "Room options:"
    echo "  1) Auto-generate new room (recommended)"
    echo "  2) Use specific room name or URL"
    read -p "Enter choice [1-2, default: 1]: " ROOM_CHOICE

    case "$ROOM_CHOICE" in
        2)
            read -p "Enter Jitsi room name or URL: " JITSI_ROOM_INPUT
            if [ -z "$JITSI_ROOM_INPUT" ]; then
                echo "[X] Jitsi room name/URL cannot be empty"
                exit 1
            fi

            case "$JITSI_ROOM_INPUT" in
                http://*|https://*|*/*)
                    ROOM_ID="$JITSI_ROOM_INPUT"
                    ;;
                *)
                    ROOM_ID="$JITSI_BASE_URL/$JITSI_ROOM_INPUT"
                    ;;
            esac
            ;;
        *)
            JITSI_ROOM="olcrtc-$PODMAN_ID"
            ROOM_ID="$JITSI_BASE_URL/$JITSI_ROOM"
            echo "[*] Generated Jitsi room URL: $ROOM_ID"
            ;;
    esac
else
    read -p "Enter Room ID: " ROOM_ID
    if [ -z "$ROOM_ID" ]; then
        echo "[X] Room ID/URL cannot be empty"
        exit 1
    fi
fi

echo ""
read -p "DNS server [default: 8.8.8.8:53]: " DNS_INPUT
DNS=${DNS_INPUT:-8.8.8.8:53}

echo ""
read -p "Use SOCKS5 proxy for egress? (y/N): " USE_PROXY

SOCKS_PROXY_ADDR=""
SOCKS_PROXY_PORT=0

if [[ "$USE_PROXY" =~ ^[Yy]$ ]]; then
    read -p "Enter SOCKS5 proxy address [default: 127.0.0.1]: " PROXY_ADDR_INPUT
    SOCKS_PROXY_ADDR=${PROXY_ADDR_INPUT:-127.0.0.1}

    read -p "Enter SOCKS5 proxy port [default: 1080]: " PROXY_PORT_INPUT
    SOCKS_PROXY_PORT=${PROXY_PORT_INPUT:-1080}

    echo "[*] Will use SOCKS5 proxy: $SOCKS_PROXY_ADDR:$SOCKS_PROXY_PORT"
fi

# Transport-specific settings
VIDEO_W=1920; VIDEO_H=1080; VIDEO_FPS=30; VIDEO_BITRATE="2M"; VIDEO_HW="none"
VIDEO_CODEC="qrcode"; VIDEO_QR_SIZE=0; VIDEO_QR_RECOVERY="low"
VIDEO_TILE_MODULE=4; VIDEO_TILE_RS=20
VP8_FPS=25; VP8_BATCH=1
SEI_FPS=60; SEI_BATCH=64; SEI_FRAG=900; SEI_ACK=2000

if [ "$TRANSPORT" = "videochannel" ]; then
    echo ""
    echo "--- Videochannel settings ---"

    echo ""
    echo "Video codec:"
    echo "  1) qrcode"
    echo "  2) tile (requires 1080x1080)"
    read -p "Enter choice [1-2, default: 1]: " VCODEC_CHOICE

    case "$VCODEC_CHOICE" in
        2)
            VIDEO_CODEC="tile"
            VIDEO_W=1080
            VIDEO_H=1080
            echo "[*] Tile codec selected - forcing 1080x1080"

            read -p "Tile module size in pixels 1..270 [default: 4]: " VTILE_MOD_INPUT
            VIDEO_TILE_MODULE=${VTILE_MOD_INPUT:-4}

            read -p "Tile Reed-Solomon parity percent 0..200 [default: 20]: " VTILE_RS_INPUT
            VIDEO_TILE_RS=${VTILE_RS_INPUT:-20}
            ;;
        *)
            VIDEO_CODEC="qrcode"

            read -p "Video width [default: 1920]: " VW_INPUT
            VIDEO_W=${VW_INPUT:-1920}

            read -p "Video height [default: 1080]: " VH_INPUT
            VIDEO_H=${VH_INPUT:-1080}

            read -p "QR error correction (low/medium/high/highest) [default: low]: " VQREC_INPUT
            VIDEO_QR_RECOVERY=${VQREC_INPUT:-low}

            read -p "QR fragment size bytes [default: 0 (auto)]: " VQRSZ_INPUT
            VIDEO_QR_SIZE=${VQRSZ_INPUT:-0}
            ;;
    esac

    read -p "Video FPS [default: 30]: " VFPS_INPUT
    VIDEO_FPS=${VFPS_INPUT:-30}

    read -p "Video bitrate [default: 2M]: " VBRT_INPUT
    VIDEO_BITRATE=${VBRT_INPUT:-2M}

    read -p "Hardware acceleration (none/nvenc) [default: none]: " VHW_INPUT
    VIDEO_HW=${VHW_INPUT:-none}
fi

if [ "$TRANSPORT" = "vp8channel" ]; then
    echo ""
    echo "--- VP8channel settings ---"

    read -p "VP8 FPS [default: 25]: " VP8FPS_INPUT
    VP8_FPS=${VP8FPS_INPUT:-25}

    read -p "VP8 batch size (frames per tick) [default: 1]: " VP8BATCH_INPUT
    VP8_BATCH=${VP8BATCH_INPUT:-1}
fi

if [ "$TRANSPORT" = "seichannel" ]; then
    echo ""
    echo "--- SEIchannel settings ---"

    read -p "SEI FPS [default: 60]: " SEIFPS_INPUT
    SEI_FPS=${SEIFPS_INPUT:-60}

    read -p "SEI batch size (frames per tick) [default: 64]: " SEIBATCH_INPUT
    SEI_BATCH=${SEIBATCH_INPUT:-64}

    read -p "SEI fragment size in bytes [default: 900]: " SEIFRAG_INPUT
    SEI_FRAG=${SEIFRAG_INPUT:-900}

    read -p "SEI ACK timeout in milliseconds [default: 2000]: " SEIACK_INPUT
    SEI_ACK=${SEIACK_INPUT:-2000}
fi

echo ""
echo "[*] Cleaning workspace..."
rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

CACHE_DIR="${OLCRTC_CACHE_DIR:-$HOME/.cache/olcrtc}"
GOMOD_CACHE="$CACHE_DIR/gomod"
GO_BUILD_CACHE="$CACHE_DIR/gobuild"

if [ "$NO_CACHE" = "1" ]; then
    echo "[*] --no-cache: purging Go cache at $CACHE_DIR"
    chmod -R u+w "$GOMOD_CACHE" "$GO_BUILD_CACHE" 2>/dev/null || true
    if ! rm -rf "$GOMOD_CACHE" "$GO_BUILD_CACHE" 2>/dev/null; then
        echo "[*] Falling back to in-container purge (files owned by container UID)..."
        podman run --rm \
            -v "$CACHE_DIR":/cache:Z \
            "$IMAGE_NAME" \
            sh -c 'rm -rf /cache/gomod /cache/gobuild'
    fi
fi

mkdir -p "$GOMOD_CACHE" "$GO_BUILD_CACHE"
echo "[*] Using Go cache: $CACHE_DIR"

echo "[*] Cloning repository..."
git clone --depth 1 --recurse-submodules --branch "$BRANCH" "$REPO_URL" "$WORK_DIR"

echo "[*] Pulling Go image..."
podman pull "$IMAGE_NAME"

echo "[*] Building OlcRTC..."
podman run --rm \
    --network host \
    -v "$WORK_DIR":/app:Z \
    -v "$GOMOD_CACHE":/go/pkg/mod:Z \
    -v "$GO_BUILD_CACHE":/root/.cache/go-build:Z \
    -w /app \
    "$IMAGE_NAME" \
    sh -c "go mod download && go build -trimpath -ldflags='-s -w' -o olcrtc ./cmd/olcrtc"

if [ ! -f "$WORK_DIR/olcrtc" ]; then
    echo "[X] Build failed"
    exit 1
fi

if [ "$GEN_ROOM" = "1" ]; then
    echo "[*] Generating room via mode: gen..."
    GEN_CONFIG="$WORK_DIR/gen.yaml"
    cat > "$GEN_CONFIG" <<GENEOF
mode: gen
auth:
  provider: "$CARRIER"
net:
  dns: "$DNS"
gen:
  amount: 1
data: data
GENEOF
    ROOM_ID=$(podman run --rm \
        --network host \
        -v "$WORK_DIR":/app:Z \
        -w /app \
        "$IMAGE_NAME" \
        ./olcrtc gen.yaml)
    if [ -z "$ROOM_ID" ]; then
        echo "[X] Room generation failed"
        exit 1
    fi
    echo "[+] Generated room ID: $ROOM_ID"
fi

KEY_FILE="$HOME/.olcrtc_key"

if [ -f "$KEY_FILE" ]; then
    echo "[*] Loading existing encryption key..."
    KEY=$(tr -d '[:space:]' < "$KEY_FILE")
    if ! validate_key "$KEY"; then
        echo "[X] Invalid encryption key in $KEY_FILE"
        echo "    Remove the file to generate a new key, or replace it with 64 hex characters."
        exit 1
    fi
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

# Generate YAML config
CONFIG_FILE="$WORK_DIR/server.yaml"
cat > "$CONFIG_FILE" <<EOF
mode: srv
auth:
  provider: "$CARRIER"
EOF

if [ -n "$WB_TOKEN" ]; then
    cat >> "$CONFIG_FILE" <<EOF
  token: "$WB_TOKEN"
EOF
fi

cat >> "$CONFIG_FILE" <<EOF
room:
  id: "$ROOM_ID"
crypto:
  key: "$KEY"
net:
  transport: "$TRANSPORT"
  dns: "$DNS"
EOF

if [ -n "$SOCKS_PROXY_ADDR" ]; then
    cat >> "$CONFIG_FILE" <<EOF
socks:
  proxy_addr: "$SOCKS_PROXY_ADDR"
  proxy_port: $SOCKS_PROXY_PORT
EOF
fi

if [ "$TRANSPORT" = "vp8channel" ]; then
    cat >> "$CONFIG_FILE" <<EOF
vp8:
  fps: $VP8_FPS
  batch_size: $VP8_BATCH
EOF
fi

if [ "$TRANSPORT" = "seichannel" ]; then
    cat >> "$CONFIG_FILE" <<EOF
sei:
  fps: $SEI_FPS
  batch_size: $SEI_BATCH
  fragment_size: $SEI_FRAG
  ack_timeout_ms: $SEI_ACK
EOF
fi

if [ "$TRANSPORT" = "videochannel" ]; then
    cat >> "$CONFIG_FILE" <<EOF
video:
  width: $VIDEO_W
  height: $VIDEO_H
  fps: $VIDEO_FPS
  bitrate: "$VIDEO_BITRATE"
  hw: $VIDEO_HW
  codec: $VIDEO_CODEC
  qr_size: $VIDEO_QR_SIZE
  qr_recovery: $VIDEO_QR_RECOVERY
  tile_module: $VIDEO_TILE_MODULE
  tile_rs: $VIDEO_TILE_RS
EOF
fi

cat >> "$CONFIG_FILE" <<EOF
data: data
debug: false
EOF

echo "[*] Starting OlcRTC server..."
START_CMD="./olcrtc server.yaml"
if [ "$TRANSPORT" = "videochannel" ]; then
    START_CMD="apk add --no-cache ffmpeg >/dev/null && ./olcrtc server.yaml"
fi
podman run -d \
    --network host \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -v "$WORK_DIR":/app:Z \
    -w /app \
    "$IMAGE_NAME" \
    sh -c "$START_CMD"

read -p "Enter a comment for the config (default: olc - t.me/openlibrecommunity): " sub_configname
if [ -z "$sub_configname" ]; then
    sub_configname="olc - t.me/openlibrecommunity"
fi

echo ""
echo "[+] Server started successfully!"
echo ""
echo "Container name: $CONTAINER_NAME"
echo "Carrier:        $CARRIER"
echo "Transport:      $TRANSPORT"
echo "Room ID/URL:    $ROOM_ID"
echo "Encryption key: $KEY"
echo ""
TRANSPORT_PAYLOAD=""
if [ "$TRANSPORT" = "vp8channel" ]; then
    TRANSPORT_PAYLOAD="<vp8-fps=${VP8_FPS}&vp8-batch=${VP8_BATCH}>"
elif [ "$TRANSPORT" = "seichannel" ]; then
    TRANSPORT_PAYLOAD="<fps=${SEI_FPS}&batch=${SEI_BATCH}&frag=${SEI_FRAG}&ack-ms=${SEI_ACK}>"
elif [ "$TRANSPORT" = "videochannel" ]; then
    TRANSPORT_PAYLOAD="<video-w=${VIDEO_W}&video-h=${VIDEO_H}&video-fps=${VIDEO_FPS}&video-bitrate=${VIDEO_BITRATE}&video-hw=${VIDEO_HW}&video-codec=${VIDEO_CODEC}>"
    if [ "$VIDEO_CODEC" = "tile" ]; then
        TRANSPORT_PAYLOAD="<video-w=${VIDEO_W}&video-h=${VIDEO_H}&video-fps=${VIDEO_FPS}&video-bitrate=${VIDEO_BITRATE}&video-hw=${VIDEO_HW}&video-codec=${VIDEO_CODEC}&video-tile-module=${VIDEO_TILE_MODULE}&video-tile-rs=${VIDEO_TILE_RS}>"
    elif [ "$VIDEO_QR_SIZE" -gt 0 ] 2>/dev/null; then
        TRANSPORT_PAYLOAD="<video-w=${VIDEO_W}&video-h=${VIDEO_H}&video-fps=${VIDEO_FPS}&video-bitrate=${VIDEO_BITRATE}&video-hw=${VIDEO_HW}&video-codec=${VIDEO_CODEC}&video-qr-recovery=${VIDEO_QR_RECOVERY}&video-qr-size=${VIDEO_QR_SIZE}>"
    else
        TRANSPORT_PAYLOAD="<video-w=${VIDEO_W}&video-h=${VIDEO_H}&video-fps=${VIDEO_FPS}&video-bitrate=${VIDEO_BITRATE}&video-hw=${VIDEO_HW}&video-codec=${VIDEO_CODEC}&video-qr-recovery=${VIDEO_QR_RECOVERY}>"
    fi
fi

OLC_URI="olcrtc://$CARRIER?${TRANSPORT}${TRANSPORT_PAYLOAD}@$ROOM_ID#$KEY\$$sub_configname"
echo "uri: $OLC_URI"
echo ""

GR_BIN="$WORK_DIR/gr"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
esac
GR_URL="https://github.com/zarazaex69/gr/releases/latest/download/gr-${OS}-${ARCH}"

if curl -fsSL "$GR_URL" -o "$GR_BIN" 2>/dev/null; then
    chmod +x "$GR_BIN"
    echo "[*] QR code for your URI (scan with olcbox):"
    echo ""
    "$GR_BIN" -o -s "$OLC_URI" 2>/dev/null || echo "[!] QR generation failed"
    echo ""
else
    echo "[!] Could not download gr ($GR_URL), skipping QR"
fi

if [ -n "$SOCKS_PROXY_ADDR" ]; then
    echo "SOCKS5 proxy:   $SOCKS_PROXY_ADDR:$SOCKS_PROXY_PORT"
fi

echo ""
echo "View logs:"
echo "  podman logs -f $CONTAINER_NAME"
echo ""
echo "Stop server:"
echo "  podman stop $CONTAINER_NAME"
echo ""

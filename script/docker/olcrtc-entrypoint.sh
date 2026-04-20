#!/bin/sh
set -eu

die() {
    echo "olcrtc-entrypoint: $*" >&2
    exit 1
}

bool_flag() {
    case "${1:-}" in
        1|true|TRUE|yes|YES|on|ON) return 0 ;;
        *) return 1 ;;
    esac
}

make_key() {
    if command -v od >/dev/null 2>&1; then
        od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
    else
        hexdump -n 32 -e '32/1 "%02x"' /dev/urandom
    fi
}

if [ "${1:-}" = "olcrtc" ]; then
    shift
fi

if [ "$#" -gt 0 ]; then
    exec /usr/local/bin/olcrtc "$@"
fi

mode="${OLCRTC_MODE:-srv}"
room_id="${OLCRTC_ROOM_ID:-${ROOM_ID:-}}"
provider="${OLCRTC_PROVIDER:-}"
data_dir="${OLCRTC_DATA_DIR:-/usr/share/olcrtc}"
dns_server="${OLCRTC_DNS:-1.1.1.1:53}"
key="${OLCRTC_KEY:-${KEY:-}}"
key_file="${OLCRTC_KEY_FILE:-/var/lib/olcrtc/key.hex}"
socks_proxy="${OLCRTC_SOCKS_PROXY:-}"
socks_proxy_port="${OLCRTC_SOCKS_PROXY_PORT:-1080}"

[ "$mode" = "srv" ] || die "server image defaults to OLCRTC_MODE=srv; got '$mode'"
[ -n "$provider" ] || die "set OLCRTC_PROVIDER (e.g. telemost, jazz, wb_stream)"
[ -n "$room_id" ] || die "set OLCRTC_ROOM_ID to the room identifier"

if [ -z "$key" ]; then
    if [ -s "$key_file" ]; then
        key="$(tr -d '[:space:]' < "$key_file")"
    else
        key="$(make_key)"
        umask 077
        printf '%s\n' "$key" > "$key_file"
        echo "olcrtc-entrypoint: generated encryption key and saved it to $key_file" >&2
        echo "olcrtc-entrypoint: OLCRTC_KEY=$key" >&2
    fi
fi

case "$key" in
    *[!0-9a-fA-F]*)
        die "OLCRTC_KEY must be a 64-character hex string"
        ;;
esac

[ "${#key}" -eq 64 ] || die "OLCRTC_KEY must be 64 hex characters"

set -- /usr/local/bin/olcrtc \
    -mode "$mode" \
    -provider "$provider" \
    -id "$room_id" \
    -key "$key" \
    -data "$data_dir" \
    -dns "$dns_server"

if [ -n "$socks_proxy" ]; then
    set -- "$@" -socks-proxy "$socks_proxy" -socks-proxy-port "$socks_proxy_port"
fi

if bool_flag "${OLCRTC_DEBUG:-}"; then
    set -- "$@" -debug
fi

exec "$@"

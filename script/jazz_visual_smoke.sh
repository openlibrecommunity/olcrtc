#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

BIN="${BIN:-${ROOT_DIR}/build/olcrtc}"
DATA_DIR="${DATA_DIR:-${ROOT_DIR}/data}"
SOCKS_HOST="${SOCKS_HOST:-127.0.0.1}"
SOCKS_PORT="${SOCKS_PORT:-10968}"
ATTEMPTS="${ATTEMPTS:-10}"
CURL_MAX_TIME="${CURL_MAX_TIME:-20}"
PROVIDER="${PROVIDER:-jazz-visual}"
KEY="${KEY:-$(openssl rand -hex 32)}"
WARMUP_SECONDS="${WARMUP_SECONDS:-5}"
PREFLIGHT_ATTEMPTS="${PREFLIGHT_ATTEMPTS:-5}"
PREFLIGHT_DELAY="${PREFLIGHT_DELAY:-2}"
VISUAL_BACKGROUND="${VISUAL_BACKGROUND:-}"
VISUAL_DUMP="${VISUAL_DUMP:-}"

RUN_DIR="${ROOT_DIR}/tmp/jazz-visual-smoke"
SRV_LOG="${RUN_DIR}/srv.log"
CNC_LOG="${RUN_DIR}/cnc.log"

SRV_PID=""
CNC_PID=""
VISUAL_ARGS=()

if [[ -n "${VISUAL_BACKGROUND}" ]]; then
  VISUAL_ARGS=(-visual-background "${VISUAL_BACKGROUND}")
fi

DUMP_ARGS=()
if [[ -n "${VISUAL_DUMP}" ]]; then
  DUMP_ARGS=(-visual-dump "${VISUAL_DUMP}")
fi

cleanup() {
  if [[ -n "${CNC_PID}" ]] && kill -0 "${CNC_PID}" 2>/dev/null; then
    kill "${CNC_PID}" 2>/dev/null || true
  fi
  if [[ -n "${SRV_PID}" ]] && kill -0 "${SRV_PID}" 2>/dev/null; then
    kill "${SRV_PID}" 2>/dev/null || true
  fi
}

trap cleanup EXIT

mkdir -p "${RUN_DIR}"
: > "${SRV_LOG}"
: > "${CNC_LOG}"

echo "[smoke] repo=${ROOT_DIR}"
echo "[smoke] provider=${PROVIDER}"
echo "[smoke] socks=${SOCKS_HOST}:${SOCKS_PORT}"
echo "[smoke] attempts=${ATTEMPTS}"
if [[ -n "${VISUAL_BACKGROUND}" ]]; then
  echo "[smoke] visual_background=${VISUAL_BACKGROUND}"
fi
if [[ -n "${VISUAL_DUMP}" ]]; then
  echo "[smoke] visual_dump=${VISUAL_DUMP}"
fi
echo "[smoke] logs=${RUN_DIR}"

if [[ ! -x "${BIN}" ]]; then
  echo "[smoke] binary not found, building ${BIN}"
  (cd "${ROOT_DIR}" && go build -tags vpx -o "${BIN}" ./cmd/olcrtc)
fi

echo "[smoke] killing old srv/cnc processes"
pkill -f 'build/olcrtc.*-mode' || true
sleep 1

echo "[smoke] starting server"
(cd "${ROOT_DIR}" && stdbuf -oL -eL "${BIN}" \
  -mode srv \
  -provider "${PROVIDER}" \
  -id any \
  -key "${KEY}" \
  "${VISUAL_ARGS[@]}" \
  "${DUMP_ARGS[@]}" \
  -data "${DATA_DIR}" \
  -debug) > "${SRV_LOG}" 2>&1 &
SRV_PID=$!

ROOM_ID=""
for _ in $(seq 1 40); do
  if ! kill -0 "${SRV_PID}" 2>/dev/null; then
    echo "[smoke] server exited before room id was captured" >&2
    tail -n 50 "${SRV_LOG}" >&2 || true
    exit 1
  fi
  if [[ -f "${SRV_LOG}" ]]; then
    ROOM_ID="$(
      {
        grep -oE 'To connect client use: -id "[^"]+"' "${SRV_LOG}" || true
      } | sed 's/.*-id "//; s/"$//' | tail -n1
    )"
    if [[ -n "${ROOM_ID}" ]]; then
      break
    fi
  fi
  sleep 1
done

if [[ -z "${ROOM_ID}" ]]; then
  echo "[smoke] failed to extract room id from ${SRV_LOG}" >&2
  tail -n 50 "${SRV_LOG}" >&2 || true
  exit 1
fi

echo "[smoke] room=${ROOM_ID}"
echo "[smoke] starting client"
(cd "${ROOT_DIR}" && stdbuf -oL -eL "${BIN}" \
  -mode cnc \
  -provider "${PROVIDER}" \
  -id "${ROOM_ID}" \
  -key "${KEY}" \
  "${VISUAL_ARGS[@]}" \
  "${DUMP_ARGS[@]}" \
  -socks-host "${SOCKS_HOST}" \
  -socks-port "${SOCKS_PORT}" \
  -data "${DATA_DIR}" \
  -debug) > "${CNC_LOG}" 2>&1 &
CNC_PID=$!

READY=0
for _ in $(seq 1 30); do
  if grep -q "SOCKS5 proxy listening on ${SOCKS_HOST}:${SOCKS_PORT}" "${CNC_LOG}"; then
    READY=1
    break
  fi
  sleep 1
done

if [[ "${READY}" -ne 1 ]]; then
  echo "[smoke] client did not reach SOCKS ready state" >&2
  tail -n 50 "${CNC_LOG}" >&2 || true
  exit 1
fi

echo "[smoke] warmup ${WARMUP_SECONDS}s"
sleep "${WARMUP_SECONDS}"

PREFLIGHT_OK=0
for i in $(seq 1 "${PREFLIGHT_ATTEMPTS}"); do
  PREFLIGHT_LOG="${RUN_DIR}/preflight-${i}.log"
  echo "[smoke] preflight ${i}/${PREFLIGHT_ATTEMPTS}"
  if curl -sv --max-time "${CURL_MAX_TIME}" \
    --http1.1 \
    -H 'Connection: close' \
    --socks5-hostname "${SOCKS_HOST}:${SOCKS_PORT}" \
    http://example.org/ > "${PREFLIGHT_LOG}" 2>&1; then
    if grep -q "HTTP/1.1 200 OK" "${PREFLIGHT_LOG}"; then
      PREFLIGHT_OK=1
      echo "[smoke] preflight ${i}: ok"
      break
    fi
  fi
  echo "[smoke] preflight ${i}: not ready"
  sleep "${PREFLIGHT_DELAY}"
done

if [[ "${PREFLIGHT_OK}" -ne 1 ]]; then
  echo "[smoke] failed to warm up visual tunnel" >&2
  exit 1
fi

SUCCESS=0
FAIL=0
START_TS="$(date +%s)"

for i in $(seq 1 "${ATTEMPTS}"); do
  CURL_LOG="${RUN_DIR}/curl-${i}.log"
  echo "[smoke] curl ${i}/${ATTEMPTS}"
  if curl -sv --max-time "${CURL_MAX_TIME}" \
    --http1.1 \
    -H 'Connection: close' \
    --socks5-hostname "${SOCKS_HOST}:${SOCKS_PORT}" \
    http://example.org/ > "${CURL_LOG}" 2>&1; then
    if grep -q "HTTP/1.1 200 OK" "${CURL_LOG}"; then
      SUCCESS=$((SUCCESS + 1))
      echo "[smoke] curl ${i}: ok"
    else
      FAIL=$((FAIL + 1))
      echo "[smoke] curl ${i}: no 200"
    fi
  else
    FAIL=$((FAIL + 1))
    echo "[smoke] curl ${i}: failed"
  fi
done

END_TS="$(date +%s)"
ELAPSED=$((END_TS - START_TS))
TOTAL=$((SUCCESS + FAIL))
SUCCESS_PCT="0.0"
if [[ "${TOTAL}" -gt 0 ]]; then
  SUCCESS_PCT="$(awk "BEGIN { printf \"%.1f\", (${SUCCESS} * 100.0) / ${TOTAL} }")"
fi

echo
echo "[smoke] success=${SUCCESS} fail=${FAIL}"
echo "[smoke] success_rate=${SUCCESS_PCT}%"
echo "[smoke] duration=${ELAPSED}s"
echo "[smoke] srv_log=${SRV_LOG}"
echo "[smoke] cnc_log=${CNC_LOG}"
echo "[smoke] key=${KEY}"
echo "[smoke] room=${ROOM_ID}"
echo

cleanup
SRV_PID=""
CNC_PID=""

echo "[smoke] visual metrics"
grep -h "visual/" "${SRV_LOG}" "${CNC_LOG}" || true

if [[ "${FAIL}" -gt 0 ]]; then
  exit 1
fi

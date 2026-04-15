#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 input.mp4 output.yuv" >&2
  exit 1
fi

INPUT="$1"
OUTPUT="$2"

ffmpeg -y \
  -i "${INPUT}" \
  -vf "fps=15,scale=640:480:force_original_aspect_ratio=increase,crop=640:480" \
  -pix_fmt yuv420p \
  -frames:v 1 \
  -f rawvideo \
  "${OUTPUT}"

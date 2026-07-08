#!/usr/bin/env sh
set -eu

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if ! command -v gitleaks >/dev/null 2>&1; then
  echo "gitleaks not found; install https://github.com/gitleaks/gitleaks before committing" >&2
  exit 127
fi

fail=0

scan_git() {
  name="$1"
  pattern="$2"
  if git grep -n -I -E "$pattern" -- . \
    ':!docs/asset/**' \
    ':!script/secrets-check.sh' \
    ':!.gitleaks.toml'; then
    echo "secret check failed: $name" >&2
    fail=1
  fi
}

scan_git "local absolute path" '/Users/[^[:space:]"<>]+|/home/[^[:space:]"<>]+|/private/var/[^[:space:]"<>]+|/var/folders/[^[:space:]"<>]+|/Volumes/[^[:space:]"<>]+'
scan_git "repo-local secret or artifact path" '[.]secrets(/|$)|whitelist-bypass|artifacts/(wb-ios|telemost|harness)|OlcMobile[.]xcframework'
scan_git "GitHub or chat token" 'gh[opsu]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}'
scan_git "private key marker" 'BEGIN (RSA|OPENSSH|EC|DSA|PRIVATE) KEY|END (RSA|OPENSSH|EC|DSA|PRIVATE) KEY'

if git grep -n -I -E 'olcrtc://[^[:space:]"<>]+#[0-9a-fA-F]{64}' -- . \
  ':!docs/**' \
  ':!script/secrets-check.sh' \
  ':!.gitleaks.toml'; then
  echo "secret check failed: embedded olcrtc URI key outside docs" >&2
  fail=1
fi

if ! gitleaks detect --no-git --redact --no-banner --source .; then
  fail=1
fi

exit "$fail"

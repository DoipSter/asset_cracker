#!/usr/bin/env bash
# Run db/tests/*.sql against the dev database on the Pi. Every test rolls itself back.
set -euo pipefail
DB="${1:-assetcracker_dev}"
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
here="$(cd "$(dirname "$0")" && pwd)"
for file in "${here}"/tests/*.sql; do
  echo "== $(basename "${file}")"
  ssh -o BatchMode=yes "${HOST}" psql -X -q -v ON_ERROR_STOP=1 -d "${DB}" -f - < "${file}" 2>&1 \
    | sed -E 's/^psql:<stdin>:[0-9]+: //' | grep -E "NOTICE|ERROR|FAILED|DETAIL" || true
done

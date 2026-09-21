#!/usr/bin/env bash
# Open the live status page from this Mac.
#
#   tools/view.sh          # open it
#   tools/view.sh stop     # close the tunnel
#
# The service only listens on the Pi itself, so this forwards one local port to it over SSH and
# opens the browser. Nothing is opened on the network; when the tunnel closes, the page is gone.
set -euo pipefail
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
PORT="${AC_VIEW_PORT:-8377}"
SOCK="${TMPDIR:-/tmp}/assetcracker-view.sock"

if [ "${1:-}" = "stop" ]; then
  ssh -S "${SOCK}" -O exit "${HOST}" 2>/dev/null && echo "tunnel closed" || echo "no tunnel was open"
  exit 0
fi
if ! ssh -S "${SOCK}" -O check "${HOST}" 2>/dev/null; then
  ssh -f -N -M -S "${SOCK}" -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
      -L "${PORT}:127.0.0.1:8377" "${HOST}"
fi
echo "Asset Cracker: http://localhost:${PORT}/   (tools/view.sh stop to close the tunnel)"
command -v open >/dev/null && open "http://localhost:${PORT}/"

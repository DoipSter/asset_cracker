#!/usr/bin/env bash
# Build the service for the Pi and copy it there. Needs no sudo: /opt/assetcracker belongs to
# the deploy user. It does NOT start or restart anything.
#
#   deploy/pi/deploy.sh
set -euo pipefail
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
here="$(cd "$(dirname "$0")/../.." && pwd)"
out="$(mktemp -d)/assetcracker"
( cd "${here}/service" && go vet ./... && go test ./... >/dev/null \
  && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
       -ldflags "-s -w -X main.version=$(git -C "${here}" rev-parse --short HEAD)" -o "${out}" ./cmd/assetcracker )
scp -q -o BatchMode=yes "${out}" "${HOST}:/opt/assetcracker/assetcracker.new"
ssh -o BatchMode=yes "${HOST}" 'mv /opt/assetcracker/assetcracker.new /opt/assetcracker/assetcracker && ls -l /opt/assetcracker/assetcracker'

#!/usr/bin/env bash
# Build the service for the Pi and release it. Needs no sudo beyond the deploy user's
# allowlist: /opt/assetcracker belongs to that user.
#
#   deploy/pi/deploy.sh          # dev:  copy the binary to /opt/assetcracker/dev only. Run it by hand against assetcracker_dev.
#   deploy/pi/deploy.sh prod     # prod: migrate, switch the release, restart, check health, roll back if unhealthy
#   deploy/pi/deploy.sh rollback # prod: switch back to the previous release and restart
#
# Layout on the Pi:  /opt/assetcracker/releases/<git sha>/assetcracker
#                    /opt/assetcracker/current -> releases/<git sha>     (what systemd runs)
set -euo pipefail

MODE="${1:-dev}"
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
here="$(cd "$(dirname "$0")/../.." && pwd)"
remote() { ssh -o BatchMode=yes "${HOST}" "$@"; }

healthy() {  # healthy SHA : wait up to 60 s for the running service to report ok at that version
  for _ in $(seq 1 30); do
    doc="$(remote curl -s --max-time 2 http://127.0.0.1:8377/healthz || true)"
    if grep -q '"ok": true' <<<"${doc}" && grep -q "\"version\": \"$1\"" <<<"${doc}"; then return 0; fi
    sleep 2
  done
  echo "${doc}" | head -20
  return 1
}

if [ "${MODE}" = "rollback" ]; then
  prev="$(remote 'cat /opt/assetcracker/previous 2>/dev/null' || true)"
  [ -n "${prev}" ] || { echo "no previous release recorded"; exit 1; }
  remote "ln -sfn releases/${prev} /opt/assetcracker/current.new && mv -T /opt/assetcracker/current.new /opt/assetcracker/current && sudo -n systemctl restart assetcracker"
  healthy "${prev}" && echo "rolled back to ${prev}" || { echo "rollback to ${prev} is NOT healthy"; exit 1; }
  exit 0
fi

sha="$(git -C "${here}" rev-parse --short HEAD)"
# A dev build of uncommitted work is not the commit it sits on: mark it, so it can never be
# mistaken for, or written over, a real release of that sha.
[ -z "$(git -C "${here}" status --porcelain)" ] || [ "${MODE}" = "prod" ] || sha="${sha}-dirty"
if [ "${MODE}" = "prod" ]; then
  [ -z "$(git -C "${here}" status --porcelain)" ] || { echo "refusing: uncommitted changes. A prod release must be exactly a commit."; exit 1; }
  branch="$(git -C "${here}" rev-parse --abbrev-ref HEAD)"
  [ "${branch}" = "main" ] || [ "${AC_ALLOW_BRANCH:-}" = "${branch}" ] || {
    echo "refusing: prod releases come from main (on '${branch}'). To override: AC_ALLOW_BRANCH=${branch}"; exit 1; }
fi

out="$(mktemp -d)/assetcracker"
( cd "${here}/service" && go vet ./... && go test ./... >/dev/null \
  && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${sha}" -o "${out}" ./cmd/assetcracker )
if [ "${MODE}" != "prod" ]; then
  # Dev builds live apart from releases and never touch what systemd is running.
  remote "mkdir -p /opt/assetcracker/dev"
  scp -q -o BatchMode=yes "${out}" "${HOST}:/opt/assetcracker/dev/assetcracker.new"
  remote "mv /opt/assetcracker/dev/assetcracker.new /opt/assetcracker/dev/assetcracker"
  echo "copied dev build ${sha}. Run it by hand on the Pi, on its own port:"
  echo "  AC_HTTP_ADDR=127.0.0.1:8378 AC_DATABASE_URL='postgres:///assetcracker_dev?host=/var/run/postgresql' /opt/assetcracker/dev/assetcracker"
  exit 0
fi

remote "mkdir -p /opt/assetcracker/releases/${sha}"
scp -q -o BatchMode=yes "${out}" "${HOST}:/opt/assetcracker/releases/${sha}/assetcracker.new"
remote "mv /opt/assetcracker/releases/${sha}/assetcracker.new /opt/assetcracker/releases/${sha}/assetcracker"
echo "copied release ${sha}"

"${here}/db/migrate.sh" prod
prev="$(remote 'readlink /opt/assetcracker/current 2>/dev/null | sed "s|releases/||"' || true)"
remote "ln -sfn releases/${sha} /opt/assetcracker/current.new && mv -T /opt/assetcracker/current.new /opt/assetcracker/current"
[ -z "${prev}" ] || [ "${prev}" = "${sha}" ] || remote "echo ${prev} > /opt/assetcracker/previous"
remote sudo -n systemctl restart assetcracker
if healthy "${sha}"; then
  echo "release ${sha} is live and healthy"
  remote 'cd /opt/assetcracker/releases && ls -1t | tail -n +6 | xargs -r rm -rf'   # keep the newest five
else
  echo "release ${sha} is NOT healthy"
  if [ -n "${prev}" ] && [ "${prev}" != "${sha}" ]; then
    echo "rolling back to ${prev}"
    remote "ln -sfn releases/${prev} /opt/assetcracker/current.new && mv -T /opt/assetcracker/current.new /opt/assetcracker/current && sudo -n systemctl restart assetcracker"
  else
    remote sudo -n systemctl stop assetcracker
    echo "no earlier release to fall back to: service stopped"
  fi
  remote sudo -n journalctl -u assetcracker -n 30 --no-pager || true
  exit 1
fi

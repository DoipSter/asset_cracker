#!/usr/bin/env bash
# Build the service for the Pi and release it. Needs no sudo beyond the deploy user's
# allowlist: /opt/assetcracker belongs to that user.
#
#   deploy/pi/deploy.sh          # dev:  copy the binary to /opt/assetcracker/dev only. Run it by hand against assetcracker_dev.
#   deploy/pi/deploy.sh check    # prod: the pre-flight alone. Says what the Pi runs, what would go out, and whether it may.
#   deploy/pi/deploy.sh prod     # prod: migrate, switch the release, restart, check health, roll back if unhealthy
#   deploy/pi/deploy.sh rollback # prod: switch back to the previous release and restart
#
# Layout on the Pi:  /opt/assetcracker/releases/<git sha>/assetcracker
#                    /opt/assetcracker/current -> releases/<git sha>     (what systemd runs)
#                    /opt/assetcracker/previous                          (the sha before the last switch)
#                    /opt/assetcracker/releases.log                      (every switch: when, what, who, in place of what)
#                    /opt/assetcracker/release.lock/                     (held while a release is in flight)
#
# More than one agent releases to this Pi. A prod release is therefore refused unless it is a
# commit everyone can see that includes what the Pi runs now: the tree is clean, the commit is
# on origin, and the live sha is in its history. A release from a checkout that has not seen
# the last release would take that release off the Pi without anybody meaning to.
set -euo pipefail

MODE="${1:-dev}"
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
here="$(cd "$(dirname "$0")/../.." && pwd)"
remote() { ssh -o BatchMode=yes "${HOST}" "$@"; }
who="$(id -un)@$(hostname -s 2>/dev/null || hostname)"
LOCK=/opt/assetcracker/release.lock

note() {  # note WORDS : one line in the Pi's release log, so "what went out, when, by whom" has an answer
  remote "printf '%s  %s\n' \"\$(date -u +%FT%TZ)\" \"$*\" >> /opt/assetcracker/releases.log"; }

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
  cur="$(remote 'readlink /opt/assetcracker/current 2>/dev/null | sed "s|releases/||"' || true)"
  remote "ln -sfn releases/${prev} /opt/assetcracker/current.new && mv -T /opt/assetcracker/current.new /opt/assetcracker/current && sudo -n systemctl restart assetcracker"
  note "${prev}  rollback from ${cur:-?}  by ${who}"
  healthy "${prev}" && echo "rolled back to ${prev}" || { echo "rollback to ${prev} is NOT healthy"; exit 1; }
  exit 0
fi

sha="$(git -C "${here}" rev-parse --short HEAD)"
branch="$(git -C "${here}" rev-parse --abbrev-ref HEAD)"
# A dev build of uncommitted work is not the commit it sits on: mark it, so it can never be
# mistaken for, or written over, a real release of that sha.
[ -z "$(git -C "${here}" status --porcelain)" ] || [ "${MODE}" = "prod" ] || [ "${MODE}" = "check" ] || sha="${sha}-dirty"

if [ "${MODE}" = "prod" ] || [ "${MODE}" = "check" ]; then
  problems=()
  [ -z "$(git -C "${here}" status --porcelain)" ] || problems+=("uncommitted changes. A prod release must be exactly a commit.")
  [ "${branch}" = "main" ] || [ "${AC_ALLOW_BRANCH:-}" = "${branch}" ] || problems+=("prod releases come from main (on '${branch}'). To override: AC_ALLOW_BRANCH=${branch}")
  # On origin: a release is a commit the other agents can fetch, not something only this checkout has.
  git -C "${here}" fetch -q origin 2>/dev/null || echo "warning: could not fetch origin; judging the push by what was last fetched"
  git -C "${here}" merge-base --is-ancestor HEAD "origin/${branch}" 2>/dev/null \
    || problems+=("${sha} is not on origin/${branch}. Push first: a release is a commit others can see.")
  # What the Pi runs must be in this commit's history, or this release takes it off the Pi.
  cur="$(remote 'readlink /opt/assetcracker/current 2>/dev/null | sed "s|releases/||"')" || { echo "cannot reach ${HOST}"; exit 1; }
  ahead=""
  if [ -n "${cur}" ] && [ "${cur}" != "${sha}" ]; then
    if ! git -C "${here}" cat-file -e "${cur}^{commit}" 2>/dev/null; then
      [ "${AC_REPLACE:-}" = "${cur}" ] || problems+=("the Pi runs ${cur}, a commit this checkout does not have: someone else released it. Fetch their branch and release from a commit that includes it. To take ${cur} off the Pi on purpose: AC_REPLACE=${cur}")
    elif ! git -C "${here}" merge-base --is-ancestor "${cur}" HEAD; then
      [ "${AC_REPLACE:-}" = "${cur}" ] || problems+=("the Pi runs ${cur}, which is not in ${sha}'s history: someone else released it. Rebase or merge so your release includes it. To take ${cur} off the Pi on purpose: AC_REPLACE=${cur}")
    else
      ahead="$(git -C "${here}" rev-list --count "${cur}..HEAD")"
    fi
  fi
  # One release at a time. The lock is a directory: mkdir is atomic, and it names who holds it.
  holder="$(remote "cat ${LOCK}/who 2>/dev/null" || true)"
  [ -z "${holder}" ] || problems+=("another release is in flight: ${holder}. If it died, remove ${LOCK} on the Pi.")

  last="$(remote 'tail -n 1 /opt/assetcracker/releases.log 2>/dev/null' || true)"
  echo "the Pi runs ${cur:-nothing}${last:+  (last switch: ${last})}"
  if [ "${cur}" = "${sha}" ]; then echo "this checkout is at ${sha}, the release already on the Pi"
  else echo "this checkout would release ${sha} on ${branch}${ahead:+, ${ahead} commit(s) ahead of the Pi}${AC_REPLACE:+, replacing ${AC_REPLACE} on purpose}"; fi
  for p in "${problems[@]+"${problems[@]}"}"; do echo "refusing: ${p}"; done
  [ ${#problems[@]} -eq 0 ] || exit 1
  if [ "${MODE}" = "check" ]; then echo "ok: ${sha} may be released"; exit 0; fi

  # Take the lock before the build, so a second release starting now is refused at once, not after its build.
  remote "mkdir ${LOCK} 2>/dev/null && echo '${who} releasing ${sha} since '\"\$(date -u +%FT%TZ)\" > ${LOCK}/who" \
    || { echo "refusing: another release took the lock just now: $(remote "cat ${LOCK}/who 2>/dev/null" || true)"; exit 1; }
  trap 'remote "rm -rf ${LOCK}"' EXIT
fi

out="$(mktemp -d)/assetcracker"
( cd "${here}/service" && go vet ./... && go test ./... >/dev/null \
  && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${sha}" -o "${out}" ./cmd/assetcracker )
if [ "${MODE}" != "prod" ]; then
  # Dev builds live apart from releases and never touch what systemd is running.
  remote "mkdir -p /opt/assetcracker/dev"
  scp -q -o BatchMode=yes "${out}" "${HOST}:/opt/assetcracker/dev/assetcracker.new"
  remote "mv /opt/assetcracker/dev/assetcracker.new /opt/assetcracker/dev/assetcracker"
  echo "copied spare binary ${sha} to /opt/assetcracker/dev."
  echo "It is not a second service. The record is the systemd unit, on database assetcracker."
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
note "${sha}  ${branch}  by ${who}  in place of ${prev:-nothing}"
remote sudo -n systemctl restart assetcracker
if healthy "${sha}"; then
  echo "release ${sha} is live and healthy"
  remote 'cd /opt/assetcracker/releases && ls -1t | tail -n +6 | xargs -r rm -rf'   # keep the newest five
else
  echo "release ${sha} is NOT healthy"
  if [ -n "${prev}" ] && [ "${prev}" != "${sha}" ]; then
    echo "rolling back to ${prev}"
    remote "ln -sfn releases/${prev} /opt/assetcracker/current.new && mv -T /opt/assetcracker/current.new /opt/assetcracker/current && sudo -n systemctl restart assetcracker"
    note "${prev}  rollback from ${sha}, unhealthy  by ${who}"
  else
    remote sudo -n systemctl stop assetcracker
    echo "no earlier release to fall back to: service stopped"
  fi
  remote sudo -n journalctl -u assetcracker -n 30 --no-pager || true
  exit 1
fi

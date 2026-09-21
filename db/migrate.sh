#!/usr/bin/env bash
# Apply db/migrations/*.sql, in order, to a database on the Pi. Each file runs in one
# transaction and is recorded in schema_migration; files already recorded are skipped.
#
#   db/migrate.sh            # dev:  assetcracker_dev, as the deploy user, who owns it
#   db/migrate.sh prod       # prod: assetcracker, as the no-login owner role, then db/grants.sql
#
# Postgres only listens on the Pi, so SQL is piped over SSH to psql there. SQL always travels on
# stdin: ssh re-splits its arguments, so a quoted -c "..." would not survive the trip.
#
# Once a migration has reached prod it is never edited. Changes are new files, and each must
# leave the previous release of the service able to run (add first, remove later), so that
# rolling the binary back never needs the database rolled back.
set -euo pipefail

TARGET="${1:-dev}"
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
here="$(cd "$(dirname "$0")" && pwd)"

case "${TARGET}" in
  dev)
    DB="assetcracker_dev"; PREAMBLE=""
    remote_psql() { ssh -o BatchMode=yes "${HOST}" psql -X -q -v ON_ERROR_STOP=1 -d "${DB}" "$@"; } ;;
  prod)
    DB="assetcracker"; PREAMBLE="set role assetcracker_owner;"
    # psql as the postgres superuser is on the deploy user's sudo allowlist. Every statement
    # then runs as the owner role, so the objects belong to it and not to a superuser.
    remote_psql() { ssh -o BatchMode=yes "${HOST}" sudo -n -u postgres psql -X -q -v ON_ERROR_STOP=1 -d "${DB}" "$@"; } ;;
  *) echo "usage: db/migrate.sh [dev|prod]"; exit 2 ;;
esac

applied="$(echo "${PREAMBLE} select version from schema_migration" | remote_psql -At -f - 2>/dev/null | grep -v '^SET$' || true)"
for file in "${here}"/migrations/*.sql; do
  version="$(basename "${file}" .sql)"
  if grep -qx "${version}" <<<"${applied}"; then
    echo "skip   ${version}"
    continue
  fi
  echo "apply  ${version}"
  { echo "${PREAMBLE}"; cat "${file}"; echo "insert into schema_migration (version) values ('${version}');"; } \
    | remote_psql --single-transaction -f - >/dev/null
done
if [ "${TARGET}" = "prod" ]; then
  echo "grants db/grants.sql"
  { echo "${PREAMBLE}"; cat "${here}/grants.sql"; } | remote_psql --single-transaction -f - >/dev/null
fi
echo "done: ${DB} on ${HOST}"

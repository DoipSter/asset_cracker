#!/usr/bin/env bash
# Apply db/migrations/*.sql, in order, to a database on the Pi. Each file runs in one
# transaction and is recorded in schema_migration; files already recorded are skipped.
#
#   db/migrate.sh                      # the dev database
#   db/migrate.sh assetcracker_dev
#
# Postgres is only reachable on the Pi itself, so this pipes each file over SSH to psql there.
# The real database belongs to the service role, not to the deploy user; migrating it is a
# separate, deliberate step that does not exist yet.
set -euo pipefail

DB="${1:-assetcracker_dev}"
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
here="$(cd "$(dirname "$0")" && pwd)"

remote_psql() { ssh -o BatchMode=yes "${HOST}" psql -X -q -v ON_ERROR_STOP=1 -d "${DB}" "$@"; }

# SQL goes over stdin: ssh re-splits its arguments, so a quoted -c "..." would not survive.
applied="$(echo 'select version from schema_migration' | remote_psql -At -f - 2>/dev/null || true)"
for file in "${here}"/migrations/*.sql; do
  version="$(basename "${file}" .sql)"
  if grep -qx "${version}" <<<"${applied}"; then
    echo "skip   ${version}"
    continue
  fi
  echo "apply  ${version}"
  { cat "${file}"; echo "insert into schema_migration (version) values ('${version}');"; } \
    | remote_psql --single-transaction -f -
done
echo "done: ${DB} on ${HOST}"

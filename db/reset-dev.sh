#!/usr/bin/env bash
# Wipe a DEV database back to empty so the migrations can be applied from scratch. Refuses any
# database whose name does not end in _dev, and any that holds ledger entries.
set -euo pipefail
DB="${1:-assetcracker_dev}"
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
case "${DB}" in *_dev) ;; *) echo "refusing: '${DB}' is not a _dev database"; exit 1;; esac
psql_remote() { ssh -o BatchMode=yes "${HOST}" psql -X -q -At -v ON_ERROR_STOP=1 -d "${DB}" -f -; }
n="$(echo "select case when to_regclass('ledger_entry') is null then 0 else (select count(*) from ledger_entry) end" | psql_remote)"
[ "${n}" = "0" ] || { echo "refusing: ${DB} holds ${n} ledger entries"; exit 1; }
echo "drop schema public cascade; create schema public;" | psql_remote
echo "reset: ${DB} is empty"

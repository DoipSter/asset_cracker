#!/usr/bin/env bash
# One-time: give the real database the three-role shape. Run from the Mac; it works as the
# deploy user through its sudo allowlist (psql as postgres). Safe to repeat.
#
#   assetcracker_owner   no login. Owns every table. Migrations run as it.
#   assetcracker         the service. Connects by peer auth as the OS user of the same name.
#                        Owns nothing; see db/grants.sql for what it may do.
#   assetcracker_ro      no login yet. Read-only, for analysis and the MCP server later.
set -euo pipefail
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
ssh -o BatchMode=yes "${HOST}" sudo -n -u postgres psql -X -q -v ON_ERROR_STOP=1 -d postgres -f - <<'SQL'
do $$
begin
    if not exists (select 1 from pg_roles where rolname = 'assetcracker_owner') then
        create role assetcracker_owner nologin;
    end if;
    if not exists (select 1 from pg_roles where rolname = 'assetcracker_ro') then
        create role assetcracker_ro nologin;
    end if;
end $$;
alter database assetcracker owner to assetcracker_owner;
revoke connect on database assetcracker from public;
grant connect on database assetcracker to assetcracker, assetcracker_ro;
\c assetcracker
grant usage on schema public to assetcracker_ro;
alter default privileges for role assetcracker_owner in schema public grant select on tables to assetcracker_ro;
select datname, pg_get_userbyid(datdba) as owner from pg_database where datname like 'assetcracker%' order by 1;
SQL

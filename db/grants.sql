-- What the running service may do in the REAL database. Applied after every migration by
-- db/migrate.sh prod; safe to repeat.
--
-- The service role owns nothing. It can read, and it can add rows. It can change only the few
-- columns whose whole purpose is to change. It cannot delete, truncate, alter, or disable the
-- triggers that keep the ledger append-only: only the owner role can, and nothing logs in as
-- the owner.

grant usage on schema public to assetcracker;
grant select, insert on all tables in schema public to assetcracker;
grant usage, select on all sequences in schema public to assetcracker;
revoke insert on schema_migration from assetcracker;
grant execute on function ensure_month_partitions(date) to assetcracker;

grant update (strike, opens_at, closes_at, result, settlement_value, settled_at) on market to assetcracker;
grant update (active) on instrument to assetcracker;
grant update (status, retired_at, retired_reason) on strategy_version to assetcracker;
grant update (status, tripped_at, trip_reason, frozen_at, replaced_by_bucket_id, limits) on bucket to assetcracker;
grant update (status, venue_order_id) on trade_order to assetcracker;
grant update (status, decided_by, decided_at, decision_note) on proposal to assetcracker;

-- engine_state is a cache the service rewrites; it is not part of the append-only record.
grant update (saved_at, state) on engine_state to assetcracker;

-- value_snapshot is appended to once a minute by the service and charted by the home page. The
-- blanket grant above already covers the service; both are spelled out because this is the
-- table the read-only role is expected to chart from. assetcracker_ro exists only in the real
-- database (deploy/pi/setup-prod-db.sh), which is the only place this file is applied.
grant select, insert on value_snapshot to assetcracker;
grant select on value_snapshot to assetcracker_ro;

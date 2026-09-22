-- reset_sim, again: the first version (0017) deleted the decision journal row by row and
-- timed out on the record, which held 1.76 million decisions. Every decision, order and
-- fill belongs to a sim bucket, and the function already refuses to run while a real-mode
-- book exists, so those three tables are truncated whole (TRUNCATE ignores row triggers, so
-- fill's append_only trigger is not disabled). The other append-only triggers are still
-- disabled with ALTER TABLE, which needs an exclusive lock; a lock wait is now bounded so a
-- long read cannot hang the request for its whole budget. A raised error rolls everything back.
--
-- 0017 reached the record and is not edited. This file replaces the function body.

create or replace function reset_sim(confirmation text) returns jsonb
language plpgsql
security definer
set search_path = public
as $$
declare
    n_fill bigint := 0;
    n_settlement bigint := 0;
    n_order bigint := 0;
    n_bucket bigint := 0;
    n_transfer bigint := 0;
    n_snapshot bigint := 0;
    n bigint;
    t_start timestamptz := clock_timestamp();
begin
    if confirmation is distinct from 'reset sim' then
        raise exception 'reset_sim: confirmation must be exactly reset sim';
    end if;
    if exists (select 1 from ledger_transfer where mode = 'real')
       or exists (select 1 from bucket where mode = 'real') then
        raise exception 'reset_sim: a real-mode book exists; refusing to run';
    end if;

    -- A reader holding a share lock on one of these tables would make ALTER TABLE wait for
    -- the whole request. Ten seconds, then the reset fails whole and can be pressed again.
    set local lock_timeout = '10s';

    alter table settlement disable trigger append_only;
    alter table bucket_event disable trigger append_only;
    alter table bucket_skim disable trigger append_only;
    alter table ledger_entry disable trigger append_only;
    alter table ledger_transfer disable trigger append_only;
    alter table value_snapshot disable trigger append_only;

    -- The counts, before the tables go: the page reports them.
    select count(*) into n_fill from fill;
    select count(*) into n_order from trade_order;

    -- The journal, the orders and the fills, whole. With no real bucket every order, fill
    -- and decision is a sim one. trade_order references decision and fill references
    -- trade_order, so the three truncate together (a truncate refuses a table that another
    -- table still points at).
    truncate decision, trade_order, fill;

    delete from settlement s
     using bucket b
     where s.bucket_id = b.id and b.mode = 'sim';
    get diagnostics n_settlement = row_count;

    delete from settlement s
     using ledger_transfer t
     where s.transfer_id = t.id and t.mode = 'sim';

    delete from bucket_event e
     using bucket b
     where e.bucket_id = b.id and b.mode = 'sim';

    delete from bucket_skim s
     using bucket b
     where s.bucket_id = b.id and b.mode = 'sim';

    delete from metric_snapshot m
     where m.mode = 'sim'
        or m.bucket_id in (select id from bucket where mode = 'sim');

    with sim as (select id from bucket where mode = 'sim')
    update bucket set replaced_by_bucket_id = null
     where id in (select id from sim)
        or replaced_by_bucket_id in (select id from sim);

    delete from bucket where mode = 'sim';
    get diagnostics n_bucket = row_count;

    delete from ledger_entry where mode = 'sim';
    delete from ledger_transfer where mode = 'sim';
    get diagnostics n_transfer = row_count;

    delete from ledger_account where mode = 'sim' and kind = 'bucket';

    delete from value_snapshot where mode = 'sim';
    get diagnostics n_snapshot = row_count;

    delete from engine_state;

    alter table settlement enable trigger append_only;
    alter table bucket_event enable trigger append_only;
    alter table bucket_skim enable trigger append_only;
    alter table ledger_entry enable trigger append_only;
    alter table ledger_transfer enable trigger append_only;
    alter table value_snapshot enable trigger append_only;

    return jsonb_build_object(
        'fills', n_fill,
        'settlements', n_settlement,
        'orders', n_order,
        'decisions_truncated', true,
        'buckets', n_bucket,
        'transfers', n_transfer,
        'snapshots', n_snapshot,
        'took_ms', (extract(epoch from clock_timestamp() - t_start) * 1000)::bigint);
end $$;

comment on function reset_sim(text) is 'Empties the simulated books: ledger, buckets, sim orders, the decision journal (truncated), the value chart, and the engine cache. Leaves candles, quotes, markets, and the strategy registry. Refuses when a real-mode book exists.';

-- create or replace keeps the owner and the grants of 0017: execute for the migrating role
-- and for the service role. Restated so that a fresh database gets them too.
revoke all on function reset_sim(text) from public;
do $grant$
begin
    execute format('grant execute on function reset_sim(text) to %I', current_user);
    if exists (select 1 from pg_roles where rolname = 'assetcracker') then
        grant execute on function reset_sim(text) to assetcracker;
    end if;
end
$grant$;

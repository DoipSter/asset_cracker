-- Operator controls for the simulated books.
--
-- operator_setting is the orders switch the buckets page writes. One row, key 'orders',
-- value 'on' or 'off'. No row means the process keeps the AC_V3 environment value.
-- The row is meant to change, so it is not append-only.
--
-- reset_sim empties the simulated books and leaves the market record and the strategy
-- registry. It is the only delete of the ledger, and only the owner can define it: the
-- service role is granted execute and still cannot delete those tables itself. It refuses
-- to run when any real-mode transfer exists.

create table operator_setting (
    key     text primary key,
    value   text not null,
    set_at  timestamptz not null default now(),
    set_by  bigint not null references actor,
    note    text not null default ''
);
comment on table operator_setting is 'The operator switches the buckets page writes. Key orders is on or off for the live engine''s new orders.';

create function reset_sim(confirmation text) returns jsonb
language plpgsql
security definer
set search_path = public
as $$
declare
    n_fill bigint := 0;
    n_settlement bigint := 0;
    n_order bigint := 0;
    n_decision bigint := 0;
    n_bucket bigint := 0;
    n_transfer bigint := 0;
    n_snapshot bigint := 0;
    n bigint;
begin
    if confirmation is distinct from 'reset sim' then
        raise exception 'reset_sim: confirmation must be exactly reset sim';
    end if;
    if exists (select 1 from ledger_transfer where mode = 'real')
       or exists (select 1 from bucket where mode = 'real') then
        raise exception 'reset_sim: a real-mode book exists; refusing to run';
    end if;

    alter table fill disable trigger append_only;
    alter table settlement disable trigger append_only;
    alter table bucket_event disable trigger append_only;
    alter table bucket_skim disable trigger append_only;
    alter table ledger_entry disable trigger append_only;
    alter table ledger_transfer disable trigger append_only;
    alter table value_snapshot disable trigger append_only;

    delete from fill f
     using trade_order o, bucket b
     where f.order_id = o.id and o.bucket_id = b.id and b.mode = 'sim';
    get diagnostics n = row_count;
    n_fill := n_fill + n;

    delete from fill f
     using ledger_transfer t
     where f.transfer_id = t.id and t.mode = 'sim';
    get diagnostics n = row_count;
    n_fill := n_fill + n;

    delete from settlement s
     using bucket b
     where s.bucket_id = b.id and b.mode = 'sim';
    get diagnostics n_settlement = row_count;

    delete from settlement s
     using ledger_transfer t
     where s.transfer_id = t.id and t.mode = 'sim';

    delete from trade_order o
     using bucket b
     where o.bucket_id = b.id and b.mode = 'sim';
    get diagnostics n_order = row_count;

    delete from decision d
     using bucket b
     where d.bucket_id = b.id and b.mode = 'sim';
    get diagnostics n_decision = row_count;

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

    alter table fill enable trigger append_only;
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
        'decisions', n_decision,
        'buckets', n_bucket,
        'transfers', n_transfer,
        'snapshots', n_snapshot);
end $$;

comment on function reset_sim(text) is 'Empties the simulated books: ledger, buckets, sim orders, decisions, the value chart, and the engine cache. Leaves candles, quotes, markets, and the strategy registry. Refuses when a real-mode book exists.';

revoke all on function reset_sim(text) from public;
do $grant$
begin
    execute format('grant execute on function reset_sim(text) to %I', current_user);
    if exists (select 1 from pg_roles where rolname = 'assetcracker') then
        grant execute on function reset_sim(text) to assetcracker;
    end if;
end
$grant$;

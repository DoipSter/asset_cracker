-- The balance sheet is one bank. Allocating a bucket draws from that bank (replenishment).
-- A reset closes the open bank and opens the next one empty: that is how a new bank is spawned.
-- The simulated books are still emptied, as reset_sim has always done; a closed bank keeps its
-- name and the date it closed, not a second copy of the ledger. Closing one bucket, inside a
-- bank, is the other operation and is not this function.
--
-- A payday is a deposit from the owners into replenishment, on a rhythm, while its bank is open.
-- next_at and enabled change, so the row is not append-only. A reset turns the closed bank's
-- paydays off; the next bank starts with none.

create table bank (
    id         bigint generated always as identity primary key,
    name       text not null check (char_length(name) between 1 and 80),
    opened_at  timestamptz not null default now(),
    closed_at  timestamptz,
    note       text not null default '',
    check (closed_at is null or closed_at >= opened_at)
);

-- One open bank. A reset closes it before inserting the next.
create unique index bank_one_open on bank ((1)) where closed_at is null;

create table bank_event (
    id            bigint generated always as identity primary key,
    bank_id       bigint not null references bank,
    kind          text not null check (kind = 'payday'),
    amount_cents  bigint not null check (amount_cents > 0),
    every_days    integer not null check (every_days between 1 and 366),
    next_at       timestamptz not null,
    note          text not null default '',
    enabled       boolean not null default true
);
create index bank_event_due on bank_event (next_at) where enabled;

insert into bank (name, note)
select 'House', 'the books already open when the bank was named'
where not exists (select 1 from bank where closed_at is null);

-- 0019 reached the record and is not edited. This replaces the function body: after the books
-- are emptied, the open bank is closed and the next one is opened.
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
    t_start timestamptz := clock_timestamp();
begin
    if confirmation is distinct from 'reset sim' then
        raise exception 'reset_sim: confirmation must be exactly reset sim';
    end if;
    if exists (select 1 from ledger_transfer where mode = 'real')
       or exists (select 1 from bucket where mode = 'real') then
        raise exception 'reset_sim: a real-mode book exists; refusing to run';
    end if;

    set local lock_timeout = '10s';

    alter table settlement disable trigger append_only;
    alter table bucket_event disable trigger append_only;
    alter table bucket_skim disable trigger append_only;
    alter table ledger_entry disable trigger append_only;
    alter table ledger_transfer disable trigger append_only;
    alter table value_snapshot disable trigger append_only;

    select count(*) into n_fill from fill;
    select count(*) into n_order from trade_order;

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

    -- The open bank closes with its books. Its paydays stop. The next bank is empty.
    update bank set closed_at = clock_timestamp() where closed_at is null;
    update bank_event set enabled = false
     where enabled
       and bank_id in (select id from bank where closed_at is not null);
    insert into bank (name, note)
    select 'House ' || (count(*) + 1), 'opened by a reset of the simulated books'
      from bank;

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

comment on function reset_sim(text) is 'Closes the open bank and opens the next one empty: ledger, buckets, sim orders, the decision journal (truncated), the value chart, and the engine cache. Paydays of the closed bank stop. Leaves candles, quotes, markets, and the strategy registry. Refuses when a real-mode book exists.';

revoke all on function reset_sim(text) from public;
do $grant$
begin
    execute format('grant execute on function reset_sim(text) to %I', current_user);
    if exists (select 1 from pg_roles where rolname = 'assetcracker') then
        grant execute on function reset_sim(text) to assetcracker;
        grant update (next_at, enabled) on bank_event to assetcracker;
    end if;
end
$grant$;

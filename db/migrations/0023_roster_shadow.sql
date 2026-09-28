-- Every roster member's shadow, kept (TSK-53, the owner's request of 2026-09-27: "record the
-- member shadows so we can measure the dancer").
--
-- A roster version (engine/composition.go) observes, on every market, what each of its members
-- would buy: one contract, on a fresh account, at the price it would pay. That is the member's
-- shadow. Once the market settles the shadow is scored, and the roster elects each clock's owner
-- from its members' shadows over the last Lookback clocks. Until now the shadows lived only in
-- the engine's memory, pruned to those Lookback clocks, so what the roster passed over could not
-- be set against what it picked. Here each one is kept: the engine queues a shadow as it is
-- scored (or dropped unscored, days after a close with no result), and the runner writes the
-- queue once a minute.
--
--   member, market_id, side   who would have bought what
--   seen_at                   when the member first would have (NULL: memory saved before this was kept)
--   closes_at                 the market's close: the clock
--   owner                     the clock's owner when the shadow was seen, as the roster elected it
--                             ('' under assign=reserve, which has no owner, or when not kept)
--   owner_how                 how that owner came to own it: 'window' (elected from prior clocks) or
--                             'warmup' (the first member, while too few clocks have settled); a
--                             measurement of the roster's picks leaves warmup clocks out
--   cost_cents                what one contract would have cost, the fee inside as the engine prices it
--   result, pnl_cents         the market's result and 100 or 0 less the cost; both NULL when it was
--                             dropped unscored
--   recorded_at               when the runner wrote it
--
-- One row per bucket, member and market; the runner's retry after a lost answer is passed over.
-- Append-only. No money: the ledger is the record of money, this is a record of choices not made.
-- reset_sim empties it with the books, since its rows name the simulated buckets.
--
-- Expand only: a new table, and reset_sim replaced in place (0021 reached the record and is not
-- edited). The release before this one never writes the table and runs unchanged.

create table roster_shadow (
    id                  bigint generated always as identity primary key,
    bucket_id           bigint not null references bucket,
    strategy_version_id bigint not null references strategy_version,
    member              text not null check (member <> ''),
    market_id           bigint not null references market,
    side                text not null check (side in ('yes', 'no')),
    seen_at             timestamptz,
    closes_at           timestamptz not null,
    owner               text not null default '',
    owner_how           text not null default '' check (owner_how in ('', 'window', 'warmup')),
    cost_cents          integer not null check (cost_cents > 0),
    result              text check (result in ('yes', 'no')),
    pnl_cents           integer,
    recorded_at         timestamptz not null default now(),
    unique (bucket_id, member, market_id),
    check ((result is null) = (pnl_cents is null))
);
-- The measurement reads a version's clocks in order.
create index roster_shadow_version_close on roster_shadow (strategy_version_id, closes_at);
create trigger append_only before update or delete on roster_shadow for each row execute function forbid_change();

comment on table roster_shadow is 'Every roster member''s would-be unit entry on a market, scored against its result: what the roster passed over, beside what it picked. Append-only. Not money.';
comment on column roster_shadow.owner is 'The clock''s owner when this shadow was seen, as the roster elected it; empty under assign=reserve or when not kept.';

-- reset_sim, again: as 0021 left it, and it now also empties roster_shadow, before the buckets
-- its rows name.
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
    n_shadow bigint := 0;
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
    alter table roster_shadow disable trigger append_only;

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

    delete from roster_shadow s
     using bucket b
     where s.bucket_id = b.id and b.mode = 'sim';
    get diagnostics n_shadow = row_count;

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
    alter table roster_shadow enable trigger append_only;

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
        'shadows', n_shadow,
        'took_ms', (extract(epoch from clock_timestamp() - t_start) * 1000)::bigint);
end $$;

comment on function reset_sim(text) is 'Closes the open bank and opens the next one empty: ledger, buckets, sim orders, the decision journal (truncated), the value chart, the roster shadows, and the engine cache. Paydays of the closed bank stop. Leaves candles, quotes, markets, and the strategy registry. Refuses when a real-mode book exists.';

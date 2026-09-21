-- The money buckets, beside the capital the strategies trade with:
--
--   winnings        a skim off the top, kept            (ledger kind profit_pool, already here)
--   replenishment   restarts buckets that died, and
--                   receives what dead ones had left    (ledger kind common_pool, already here)
--   tax reserve     a share of gains set aside, so there is no surprise          (new)
--   fee reserve     set aside for fees that may come later: withdrawals, venue
--                   charges. Kalshi's per-trade fee is already paid on every fill. (new)
--
-- A bucket is skimmed only on gains above its own high-water mark, so one that is climbing back
-- from a loss is not charged twice on the same dollars. The rates are a dated policy, not code.

alter table ledger_account drop constraint ledger_account_kind_check;
alter table ledger_account add constraint ledger_account_kind_check check (kind in (
    'bucket', 'common_pool', 'profit_pool', 'tax_reserve', 'fee_reserve', 'venue', 'fees', 'external'));

-- One of each pool per mode and currency, now including the two reserves.
drop index ledger_account_kind_mode_currency_idx;
create unique index ledger_account_one_pool_per_mode on ledger_account (kind, mode, currency)
    where kind in ('common_pool', 'profit_pool', 'tax_reserve', 'fee_reserve');

-- The split, in basis points of a bucket's gain above its high-water mark. What is not skimmed
-- stays in the bucket and compounds. The newest row for a mode is the one in force; a change is
-- a new row, so every skim can be traced to the policy that set it.
create table skim_policy (
    id             bigint generated always as identity primary key,
    effective_at   timestamptz not null default now(),
    mode           run_mode not null,
    winnings_bps   integer not null check (winnings_bps  between 0 and 10000),
    replenish_bps  integer not null check (replenish_bps between 0 and 10000),
    tax_bps        integer not null check (tax_bps       between 0 and 10000),
    fees_bps       integer not null check (fees_bps      between 0 and 10000),
    note           text not null default '',
    set_by         bigint not null references actor,
    check (winnings_bps + replenish_bps + tax_bps + fees_bps <= 10000)
);
create trigger append_only before update or delete on skim_policy for each row execute function forbid_change();

-- Every time a bucket reached a new high, whether or not anything was taken: the record of its
-- high-water mark, and of exactly what went where.
create table bucket_skim (
    id               bigint generated always as identity primary key,
    at               timestamptz not null default now(),
    bucket_id        bigint not null references bucket,
    policy_id        bigint not null references skim_policy,
    book_cents       bigint not null,   -- cash plus open bets at cost, before the skim
    hwm_before_cents bigint not null,
    gain_cents       bigint not null check (gain_cents > 0),
    winnings_cents   bigint not null default 0 check (winnings_cents  >= 0),
    replenish_cents  bigint not null default 0 check (replenish_cents >= 0),
    tax_cents        bigint not null default 0 check (tax_cents       >= 0),
    fees_cents       bigint not null default 0 check (fees_cents      >= 0),
    hwm_after_cents  bigint not null,
    transfer_id      bigint references ledger_transfer,   -- null when every rate was zero
    check (hwm_after_cents = book_cents - winnings_cents - replenish_cents - tax_cents - fees_cents)
);
create index on bucket_skim (bucket_id, id desc);
create trigger append_only before update or delete on bucket_skim for each row execute function forbid_change();

-- Every rate starts at zero: nothing is skimmed until a person sets the rates. A tax rate in
-- particular is a fact about its owner's situation, not something to be guessed here.
insert into skim_policy (mode, winnings_bps, replenish_bps, tax_bps, fees_bps, note, set_by)
select 'sim', 0, 0, 0, 0, 'initial: nothing is skimmed until rates are set', id from actor where handle = 'service';

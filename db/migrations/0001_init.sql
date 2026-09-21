-- Asset Cracker platform: first schema. See docs/platform-brief.md.
--
-- Conventions
--   * Money is integer minor units (cents) in bigint columns ending _cents. Never floats.
--   * Prices and quantities are numeric: a contract costs 0.31, a coin trades in fractions.
--   * Every time is timestamptz, stored in UTC.
--   * mode is 'sim' or 'real'. The two never share a bucket, an account or a transfer, and the
--     database enforces that, not the application.
--   * The ledger, fills, commentary, weights and events are append-only. Corrections are new
--     rows. Triggers refuse UPDATE and DELETE.
--   * High-volume time series (price_tick, evaluation, decision) are partitioned by month.

create type run_mode as enum ('sim', 'real');

-- ---------------------------------------------------------------------------------------------
-- Who and what acts
-- ---------------------------------------------------------------------------------------------

create table actor (
    id          bigint generated always as identity primary key,
    kind        text not null check (kind in ('person', 'agent', 'system')),
    handle      text not null unique,
    created_at  timestamptz not null default now()
);
comment on table actor is 'Anyone or anything that can do something worth attributing: people, agents, the supervisor.';

-- ---------------------------------------------------------------------------------------------
-- Where things trade
-- ---------------------------------------------------------------------------------------------

create table source (
    id               bigint generated always as identity primary key,
    code             text not null unique,            -- 'kalshi', 'coinbase'
    name             text not null,
    has_market_data  boolean not null default false,
    has_execution    boolean not null default false
);

create table instrument (
    id          bigint generated always as identity primary key,
    source_id   bigint not null references source,
    kind        text not null check (kind in ('binary_contract', 'spot')),
    symbol      text not null,                        -- 'KXBTC15M' (a series), 'BTC-USD'
    underlying  text not null,                        -- 'BTC'
    currency    text not null default 'USD',
    spec        jsonb not null default '{}',          -- fee model, tick size, position rule, wind-down rule
    active      boolean not null default true,
    unique (source_id, symbol)
);
comment on column instrument.spec is 'Rules that differ by instrument type. For Kalshi: fee rate, one net position per market, settle-on-expiry.';

-- One tradable thing with its own lifetime. A Kalshi series opens a new market every 15
-- minutes; a spot instrument has a single market that never closes.
create table market (
    id                bigint generated always as identity primary key,
    instrument_id     bigint not null references instrument,
    ticker            text not null,
    strike            numeric,
    opens_at          timestamptz,
    closes_at         timestamptz,
    result            text,                           -- 'yes' / 'no' for binary contracts
    settlement_value  numeric,
    settled_at        timestamptz,
    unique (instrument_id, ticker)
);
create index on market (instrument_id, closes_at);

-- ---------------------------------------------------------------------------------------------
-- Strategies. Every version ever tried is a row here: this is the trials registry.
-- ---------------------------------------------------------------------------------------------

create table strategy (
    id           bigint generated always as identity primary key,
    family       text not null,                       -- 'kalshi15m'
    name         text not null,                       -- 'Value'
    description  text not null default '',
    unique (family, name)
);

create table strategy_version (
    id                 bigint generated always as identity primary key,
    strategy_id        bigint not null references strategy,
    version            integer not null,
    params             jsonb not null,
    code_ref           text not null,                 -- git sha the code was built from
    hypothesis         text not null default '',      -- what this version is meant to test
    parent_version_id  bigint references strategy_version,
    created_by         bigint not null references actor,
    created_at         timestamptz not null default now(),
    status             text not null default 'draft'
                       check (status in ('draft', 'probation', 'bench', 'active', 'retired')),
    retired_at         timestamptz,
    retired_reason     text,
    unique (strategy_id, version)
);
comment on table strategy_version is 'Results always attach to a version. The row count is the number of trials, which every significance figure must be corrected for.';

-- ---------------------------------------------------------------------------------------------
-- The ledger: double-entry, integer cents, append-only.
-- ---------------------------------------------------------------------------------------------

create table ledger_account (
    id        bigint generated always as identity primary key,
    kind      text not null check (kind in (
                  'bucket',         -- one bucket's virtual share of a venue account
                  'common_pool',    -- tax and reaped funds; seeds new buckets
                  'profit_pool',    -- what is kept
                  'venue',          -- the other side of every trade and settlement
                  'fees',           -- where fees go
                  'external')),     -- the owners' money outside the system
    mode      run_mode not null,
    currency  text not null default 'USD',
    name      text not null,
    unique (id, mode),
    unique (mode, name)
);
-- One of each pool per mode and currency.
create unique index on ledger_account (kind, mode, currency) where kind in ('common_pool', 'profit_pool');

create table ledger_transfer (
    id          bigint generated always as identity primary key,
    at          timestamptz not null default now(),
    mode          run_mode not null,
    reason      text not null check (reason in (
                    'deposit', 'withdrawal',           -- external <-> pools
                    'seed', 'tax', 'reap',             -- pools <-> buckets
                    'take', 'expansion',               -- common pool <-> profit pool
                    'fill', 'fee', 'settlement',       -- trading
                    'adjustment')),                    -- a documented correction
    memo        text not null default '',
    created_by  bigint not null references actor,
    unique (id, mode)
);
create index on ledger_transfer (at);

create table ledger_entry (
    id            bigint generated always as identity primary key,
    transfer_id   bigint not null,
    account_id    bigint not null,
    mode          run_mode not null,
    amount_cents  bigint not null check (amount_cents <> 0),   -- positive: into the account
    -- The composite keys are what keep sim and real apart: an entry's mode must equal both its
    -- transfer's mode and its account's mode, so no transfer can touch both worlds.
    foreign key (transfer_id, mode) references ledger_transfer (id, mode),
    foreign key (account_id, mode)  references ledger_account (id, mode)
);
create index on ledger_entry (account_id);
create index on ledger_entry (transfer_id);

-- A transfer must balance. Checked at commit, so its entries can be inserted one by one.
create function ledger_transfer_balances() returns trigger language plpgsql as $$
declare
    total bigint;
    n     integer;
begin
    select coalesce(sum(amount_cents), 0), count(*) into total, n
      from ledger_entry where transfer_id = new.transfer_id;
    if n < 2 or total <> 0 then
        raise exception 'ledger transfer % does not balance: % entries summing to % cents',
            new.transfer_id, n, total;
    end if;
    return null;
end $$;

create constraint trigger ledger_entry_balances
    after insert on ledger_entry
    deferrable initially deferred
    for each row execute function ledger_transfer_balances();

-- A transfer with no entries at all would slip past the row trigger above.
create function ledger_transfer_has_entries() returns trigger language plpgsql as $$
begin
    if not exists (select 1 from ledger_entry where transfer_id = new.id) then
        raise exception 'ledger transfer % has no entries', new.id;
    end if;
    return null;
end $$;

create constraint trigger ledger_transfer_not_empty
    after insert on ledger_transfer
    deferrable initially deferred
    for each row execute function ledger_transfer_has_entries();

create function forbid_change() returns trigger language plpgsql as $$
begin
    raise exception '% is append-only: % refused. Record a correction as a new row.', tg_table_name, tg_op;
end $$;

create trigger append_only before update or delete on ledger_transfer for each row execute function forbid_change();
create trigger append_only before update or delete on ledger_entry    for each row execute function forbid_change();

create view ledger_balance as
    select a.id as account_id, a.kind, a.mode, a.currency, a.name,
           coalesce(sum(e.amount_cents), 0)::bigint as balance_cents
      from ledger_account a left join ledger_entry e on e.account_id = a.id
     group by a.id;

-- ---------------------------------------------------------------------------------------------
-- Venue accounts and buckets.
--
-- A venue account is the one actual account at a venue (a Kalshi account, say, or its simulated
-- stand-in). It is divided into buckets: each bucket is ONE virtual subdivision of ONE venue
-- account, with its own cash in the ledger, its own limits, and one strategy version.
-- The venue only ever sees the whole account; the split exists here.
-- ---------------------------------------------------------------------------------------------

create table venue_account (
    id            bigint generated always as identity primary key,
    source_id     bigint not null references source,
    mode          run_mode not null,
    name          text not null,
    external_ref  text,                               -- the venue's own id for the account; never a credential
    unique (id, mode),
    unique (source_id, mode, name)
);
comment on table venue_account is 'Buckets in one venue account share its real position book: the venue nets opposite sides. The service must net or forbid conflicts before real trading.';

create table bucket (
    id                    bigint generated always as identity primary key,
    name                  text not null unique,
    mode                  run_mode not null,
    venue_account_id      bigint not null,
    ledger_account_id     bigint not null unique,     -- this bucket's virtual cash: exactly one
    slot                  integer,                    -- which place in the line-up it fills
    strategy_version_id   bigint not null references strategy_version,
    status                text not null default 'active'
                          check (status in ('active', 'tripped', 'winding_down', 'frozen')),
    limits                jsonb not null,             -- breaker thresholds and hard caps
    tax_rate_bps          integer not null check (tax_rate_bps between 0 and 10000),
    created_at            timestamptz not null default now(),
    tripped_at            timestamptz,
    trip_reason           text,
    frozen_at             timestamptz,
    replaced_by_bucket_id bigint references bucket,
    -- A sim bucket can only sit in a sim venue account and hold sim cash, and likewise for real.
    foreign key (venue_account_id, mode)  references venue_account (id, mode),
    foreign key (ledger_account_id, mode) references ledger_account (id, mode)
);
create index on bucket (venue_account_id);
comment on table bucket is 'Closed, never topped up: a tripped bucket winds down, is reaped into the common pool, and is frozen. Frozen buckets are kept forever.';

create function bucket_ledger_account_kind() returns trigger language plpgsql as $$
begin
    if (select kind from ledger_account where id = new.ledger_account_id) <> 'bucket' then
        raise exception 'bucket % must use a ledger account of kind ''bucket''', new.name;
    end if;
    return new;
end $$;
create trigger ledger_account_kind before insert or update of ledger_account_id on bucket
    for each row execute function bucket_ledger_account_kind();

-- The virtual cash in each venue account: what the venue's own balance should equal, once
-- open positions are accounted for. The check against the venue's figure is the service's job.
create view venue_account_virtual_cash as
    select v.id as venue_account_id, v.mode, v.name,
           coalesce(sum(lb.balance_cents), 0)::bigint as bucket_cash_cents,
           count(b.id) as buckets
      from venue_account v
      left join bucket b on b.venue_account_id = v.id
      left join ledger_balance lb on lb.account_id = b.ledger_account_id
     group by v.id;

create table bucket_event (
    id         bigint generated always as identity primary key,
    bucket_id  bigint not null references bucket,
    at         timestamptz not null default now(),
    kind       text not null check (kind in (
                   'seeded', 'taxed', 'tripped', 'winding_down', 'reaped', 'frozen', 'replaced',
                   'limits_changed', 'paused', 'resumed')),
    detail     jsonb not null default '{}',           -- for 'tripped': what was measured, against what threshold
    actor_id   bigint not null references actor
);
create index on bucket_event (bucket_id, at);
create trigger append_only before update or delete on bucket_event for each row execute function forbid_change();

-- ---------------------------------------------------------------------------------------------
-- Time series, partitioned by month. The service creates partitions ahead of need.
-- ---------------------------------------------------------------------------------------------

-- Every trade print from a market-data source.
create table price_tick (
    instrument_id  bigint not null references instrument,
    at             timestamptz not null,              -- the exchange's timestamp
    received_at    timestamptz not null,
    price          numeric not null,
    size           numeric
) partition by range (at);
create index on price_tick (instrument_id, at);

-- What was known about one market at one moment: shared by every strategy that looked at it.
create table evaluation (
    id                bigint generated always as identity,
    at                timestamptz not null,
    market_id         bigint not null references market,
    underlying_price  numeric,
    quotes            jsonb not null,                 -- bids, asks, sizes as the venue gave them
    model             jsonb not null default '{}',    -- shared model state: volatility, index offset, ...
    primary key (id, at)
) partition by range (at);
create index on evaluation (market_id, at);

-- The decision journal: one row per strategy per evaluation, whether or not it acted.
create table decision (
    id                   bigint generated always as identity,
    at                   timestamptz not null,
    evaluation_id        bigint not null,
    bucket_id            bigint not null references bucket,
    strategy_version_id  bigint not null references strategy_version,
    model_prob           double precision,            -- what the strategy believed
    market_prob          double precision,            -- what the market's price implied
    side                 text,                        -- the side it favoured
    edge                 double precision,            -- after fees and slippage
    action               text not null check (action in ('none', 'enter', 'exit')),
    blocked_by           text,                        -- the rule that stopped it, when action is 'none'
    reason               text not null default '',
    -- Human weight is applied on top of the strategy's own sizing. Both sizes are kept so the
    -- record shows what the strategy alone would have done.
    size_alone           numeric,
    human_weight         numeric not null default 1,
    size_applied         numeric,
    primary key (id, at),
    foreign key (evaluation_id, at) references evaluation (id, at)
) partition by range (at);
create index on decision (strategy_version_id, at);
create index on decision (bucket_id, at);

create function ensure_month_partitions(month date) returns void language plpgsql as $$
declare
    t      text;
    first  date := date_trunc('month', month);
    next   date := first + interval '1 month';
    suffix text := to_char(first, 'YYYY_MM');
begin
    foreach t in array array['price_tick', 'evaluation', 'decision'] loop
        execute format(
            'create table if not exists %I partition of %I for values from (%L) to (%L)',
            t || '_' || suffix, t, first, next);
    end loop;
end $$;

select ensure_month_partitions(current_date);
select ensure_month_partitions((current_date + interval '1 month')::date);

-- ---------------------------------------------------------------------------------------------
-- Orders and fills. The same tables serve the paper broker and a live one.
-- ---------------------------------------------------------------------------------------------

create table trade_order (
    id                  bigint generated always as identity primary key,
    bucket_id           bigint not null references bucket,
    market_id           bigint not null references market,
    decision_id         bigint,                       -- with decision_at: the journal row behind it
    decision_at         timestamptz,
    broker              text not null check (broker in ('paper', 'live')),
    action              text not null check (action in ('buy', 'sell')),
    side                text not null,                -- 'yes' / 'no' for contracts, 'long' for spot
    qty                 numeric not null check (qty > 0),
    limit_price         numeric,
    status              text not null default 'new'
                        check (status in ('new', 'sent', 'partial', 'filled', 'cancelled', 'rejected')),
    venue_order_id      text,
    placed_at           timestamptz not null default now(),
    foreign key (decision_id, decision_at) references decision (id, at)
);
create index on trade_order (bucket_id, placed_at);

create table fill (
    id           bigint generated always as identity primary key,
    order_id     bigint not null references trade_order,
    at           timestamptz not null,
    qty          numeric not null check (qty > 0),
    price        numeric not null,
    fee_cents    bigint not null default 0 check (fee_cents >= 0),
    transfer_id  bigint not null references ledger_transfer   -- the cash movement for this fill
);
create index on fill (order_id);
create trigger append_only before update or delete on fill for each row execute function forbid_change();

create table settlement (
    id                  bigint generated always as identity primary key,
    market_id           bigint not null references market,
    bucket_id           bigint not null references bucket,
    at                  timestamptz not null,
    side                text not null,
    qty                 numeric not null,
    payout_cents        bigint not null check (payout_cents >= 0),
    transfer_id         bigint references ledger_transfer,        -- null when the payout is zero
    unique (market_id, bucket_id, side)
);
create trigger append_only before update or delete on settlement for each row execute function forbid_change();

-- ---------------------------------------------------------------------------------------------
-- People's input: commentary and weights. Immediate, attributed, append-only.
-- ---------------------------------------------------------------------------------------------

create table commentary (
    id             bigint generated always as identity primary key,
    at             timestamptz not null default now(),
    author_id      bigint not null references actor,
    target_kind    text not null check (target_kind in (
                       'strategy', 'strategy_version', 'bucket', 'instrument', 'market',
                       'decision', 'order', 'fill', 'period', 'general')),
    target_id      bigint,                            -- null for 'period' and 'general'
    period         tstzrange,                         -- for 'period'
    body           text not null check (length(body) > 0),
    supersedes_id  bigint references commentary       -- an edit is a new row pointing at the old
);
create index on commentary (target_kind, target_id);
create trigger append_only before update or delete on commentary for each row execute function forbid_change();

create table human_weight (
    id           bigint generated always as identity primary key,
    set_at       timestamptz not null default now(),
    set_by       bigint not null references actor,
    target_kind  text not null check (target_kind in ('strategy', 'strategy_version', 'bucket', 'instrument')),
    target_id    bigint not null,
    weight       numeric not null check (weight >= 0 and weight <= 5),   -- 0 mutes, 1 is neutral
    note         text not null default ''
);
create index on human_weight (target_kind, target_id, set_at desc);
create trigger append_only before update or delete on human_weight for each row execute function forbid_change();

-- The weight in force now for each target: the latest row wins.
create view current_human_weight as
    select distinct on (target_kind, target_id) target_kind, target_id, weight, set_by, set_at, note
      from human_weight order by target_kind, target_id, set_at desc, id desc;

-- ---------------------------------------------------------------------------------------------
-- Agents propose, people approve.
-- ---------------------------------------------------------------------------------------------

create table proposal (
    id           bigint generated always as identity primary key,
    at           timestamptz not null default now(),
    proposed_by  bigint not null references actor,
    kind         text not null check (kind in ('strategy_version', 'new_bucket', 'limits_change', 'expansion', 'retire')),
    payload      jsonb not null,
    rationale    text not null default '',
    status       text not null default 'proposed' check (status in ('proposed', 'approved', 'rejected', 'withdrawn')),
    decided_by   bigint references actor,
    decided_at   timestamptz,
    decision_note text
);

-- ---------------------------------------------------------------------------------------------
-- Scores. Computed by the service or by analysis; kept so a gate decision can be audited.
-- ---------------------------------------------------------------------------------------------

create table metric_snapshot (
    id                   bigint generated always as identity primary key,
    computed_at          timestamptz not null default now(),
    strategy_version_id  bigint not null references strategy_version,
    bucket_id            bigint references bucket,
    mode          run_mode not null,
    period               tstzrange not null,
    n_decisions          integer not null,
    n_trades             integer not null,
    trials_at_the_time   integer not null,            -- how many versions had been tried: the correction factor
    metrics              jsonb not null,              -- Brier, return per dollar with CI, drawdown, ...
    gate_config          jsonb,
    gate_passed          boolean
);
create index on metric_snapshot (strategy_version_id, computed_at);

create table schema_migration (
    version     text primary key,
    applied_at  timestamptz not null default now()
);

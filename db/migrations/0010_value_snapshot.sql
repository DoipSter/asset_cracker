-- What everything was worth, minute by minute: the history behind the home page's "earned over
-- 1H / 24H / 7D / ALL" and its value chart. The service appends to it from the engines' books
-- in memory; nothing else writes it, and nothing here is money: the ledger stays the record of
-- money, this is a record of what it was MARKED at.
--
--   value_cents        marked to market: cash plus open bets at the current BID. An open bet
--                      with no bid (a round that has closed but not settled, an empty book) is
--                      worth 0 here and is counted in `unmarked`, so a dip can be told from a loss.
--   cash_cents         cash alone.
--   at_risk_cents      open bets at what they cost.
--   contributed_cents  money put IN, net of money taken out, that was not earned by trading:
--                      for 'total' it is what came from the 'external' ledger account; for a
--                      bucket or a group of buckets it is seeds, less what was reaped, less the
--                      sustainment allocation taken. EARNED between two moments is the change in
--                      (value - contributed), so a top-up or a restake never counts as profit
--                      and an allocation never counts as a loss.
--
-- Scopes and how often each is written:
--
--   'total'   key 'all'                               every minute
--   'group'   key strategies | anti | v1 | money      every minute
--   'bucket'  key = the bucket's name                 every five minutes
--   'coin'    key = the coin; value and at-risk of    every five minutes
--             the bets open on it, cash always 0
--
-- Rows per day: 5 a minute is 7,200; 24 buckets and 5 coins every five minutes is 8,352. About
-- 15,600 rows a day, 5.7 million a year, each about 120 bytes before its index (an estimate from the
-- column widths, not yet measured): in the region of a gigabyte a year on an NVMe that the evaluation table alone fills faster in a week (it takes
-- 5 rows a SECOND, each with an order book). So it is not partitioned. Buckets and coins are
-- five-minutely because nothing reads them finer than that; the total is what the phone charts.

create table value_snapshot (
    id                bigint generated always as identity primary key,
    at                timestamptz not null,
    mode              run_mode not null,
    scope             text not null check (scope in ('total', 'group', 'bucket', 'coin')),
    key               text not null,
    value_cents       bigint not null,
    cash_cents        bigint not null,
    at_risk_cents     bigint not null check (at_risk_cents >= 0),
    contributed_cents bigint not null,
    unmarked          integer not null default 0 check (unmarked >= 0)
);
create index value_snapshot_lookup on value_snapshot (mode, scope, key, at);
create trigger append_only before update or delete on value_snapshot for each row execute function forbid_change();

comment on table value_snapshot is 'What the simulated world was marked at, appended once a minute by the service. Append-only. Not money: the ledger is.';
comment on column value_snapshot.value_cents is 'Cash plus open bets at the BID. A bet with no bid counts 0 and is counted in unmarked.';
comment on column value_snapshot.contributed_cents is 'Net money put in that trading did not earn. Earned = change in (value_cents - contributed_cents).';
comment on column value_snapshot.unmarked is 'Open bets that had no bid to mark them by when this row was written.';

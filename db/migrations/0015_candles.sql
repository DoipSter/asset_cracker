-- Coinbase's daily and hourly candles for the five spot products, for the longer-horizon spot
-- protocol (docs/spot-protocol.md). RECORD ONLY: nothing trades from this table and nothing here
-- is money. cmd/candles backfills it; the service adds the newly complete candles once an hour.
-- 0013 is reserved by the frozen v3 measurement protocol; db/migrate.sh skips the gap.
--
--   at            the candle's START time (Coinbase's first field). A candle covers
--                 [at, at + granularity_s).
--   granularity_s 86400 (daily, starting 00:00 UTC) or 3600 (hourly). Any positive length is
--                 allowed, so that a finer one needs no migration.
--   open .. volume the decimal text Coinbase sent, parsed by Postgres, never through a float.
--   fetched_at    when the copy stored here was fetched.
--
-- Only COMPLETE candles are written: one whose end is in the past when it is fetched. A candle is
-- written once and never changed; the writer counts, and logs, any later copy from Coinbase that
-- differs from the stored one, so a candle stored too early shows up as a count, not as a silent
-- correction. Rows: products x days (daily) and products x hours (hourly); three years of five
-- products is about 5,500 daily and 131,500 hourly rows. Not partitioned.
--
-- NOT YET RUN ANYWHERE: written without a database.

create table candle (
    id            bigint generated always as identity primary key,
    instrument_id bigint not null references instrument,
    granularity_s integer not null check (granularity_s > 0),
    at            timestamptz not null,
    open          numeric not null,
    high          numeric not null,
    low           numeric not null,
    close         numeric not null,
    volume        numeric not null check (volume >= 0),
    fetched_at    timestamptz not null,
    unique (instrument_id, granularity_s, at),
    -- A candle starts on a multiple of its length since the Unix epoch, as Coinbase's do.
    check (extract(epoch from at)::bigint % granularity_s = 0)
);
create trigger append_only before update or delete on candle for each row execute function forbid_change();

comment on table candle is 'Coinbase candles for the spot products, complete ones only, appended by cmd/candles and the service. Append-only. Not money.';
comment on column candle.at is 'The candle''s START time; it covers [at, at + granularity_s).';
comment on column candle.fetched_at is 'When this copy was fetched. A later copy that differs is counted and logged, never stored.';

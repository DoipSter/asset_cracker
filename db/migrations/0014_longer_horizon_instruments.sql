-- Kalshi's above/below ladders for the five coins, RECORD ONLY, for the long-shot protocol
-- (docs/longshot-protocol.md, section 2: Hh, Hd, Hw). 0013 is reserved by the frozen v3
-- measurement protocol; db/migrate.sh applies files by name and skips the ones recorded, so the
-- gap is harmless.
--
-- Read from the public API on 2026-09-21: series KXBTCD KXETHD KXSOLD KXXRPD KXDOGED, strike_type
-- "greater". Each carries an hourly, a daily (closes 21:00 UTC) and a weekly (Friday 21:00 UTC)
-- event at once, so a series has no single round length and gets no round_seconds: that is what
-- keeps these markets out of the analysis and the status page's recent rounds
-- (store.FifteenMinuteSeries).
--
-- kind 'binary_ladder', not 'binary_contract': the previous release of the service builds a
-- 15-minute poller, a home-page asset and a health-check entry for every active kalshi
-- 'binary_contract', and would do so for these if it were rolled back to. It ignores any other
-- kind. The new release sends these rows to the ladder recorder only (cmd/assetcracker/ladder.go).
-- "trade": false says no strategy trades them; "ladder": true says the same thing as the kind, for
-- anything that reads the spec only.

alter table instrument drop constraint instrument_kind_check;
alter table instrument add constraint instrument_kind_check
    check (kind in ('binary_contract', 'spot', 'binary_ladder'));

insert into instrument (source_id, kind, symbol, underlying, spec)
select s.id, 'binary_ladder', v.symbol, v.underlying,
       jsonb_build_object(
           'price_from',     'coinbase:' || v.underlying || '-USD',
           'ladder',         true,
           'trade',          false,
           'strike_type',    'greater')
  from source s, (values ('KXBTCD', 'BTC'), ('KXETHD', 'ETH'), ('KXSOLD', 'SOL'), ('KXXRPD', 'XRP'), ('KXDOGED', 'DOGE'))
       as v (symbol, underlying)
 where s.code = 'kalshi';

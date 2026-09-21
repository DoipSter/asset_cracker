-- SOL, XRP and DOGE: the three coins doipster's app added on 2026-09-20. Series and product
-- names confirmed against both public APIs that day. Calibration values are his starting
-- figures (asset_cracker.py ASSETS on integration/all-features).
--
-- "trade": false means RECORD ONLY. Market data for these coins is captured from now on, but no
-- strategy runs on them: the six registered versions are his per-coin, $150 design, and his
-- current design (five coins on one shared balance) is a different strategy version that has
-- not been ported. Flip the flag in a later migration when a version that trades them exists.
-- Existing instruments have no flag and keep trading.

insert into instrument (source_id, kind, symbol, underlying, spec)
select s.id, 'spot', v.symbol, v.underlying, '{}'::jsonb
  from source s, (values ('SOL-USD', 'SOL'), ('XRP-USD', 'XRP'), ('DOGE-USD', 'DOGE')) as v (symbol, underlying)
 where s.code = 'coinbase';

insert into instrument (source_id, kind, symbol, underlying, spec)
select s.id, 'binary_contract', v.symbol, v.underlying,
       jsonb_build_object(
           'price_from',       'coinbase:' || v.underlying || '-USD',
           'round_seconds',    900,
           'settles_on',       'average of the index over the final 60 seconds',
           'fee_rate',         0.07,
           'position_rule',    'one net position per market',
           'wind_down',        'hold to settlement',
           'trade',            false,
           'index_offset_pct', 0.000057,
           'index_sd_pct',     0.000216,
           'default_sigma',    v.sigma,
           'decimals',         v.decimals)
  from source s, (values ('KXSOL15M', 'SOL', 1.1e-4, 4), ('KXXRP15M', 'XRP', 1.1e-4, 4), ('KXDOGE15M', 'DOGE', 1.2e-4, 6))
       as v (symbol, underlying, sigma, decimals)
 where s.code = 'kalshi';

-- His per-coin precision fix: BTC strikes are quoted to two decimals, not zero.
update instrument set spec = spec || '{"decimals": 2}'::jsonb where symbol = 'KXBTC15M';

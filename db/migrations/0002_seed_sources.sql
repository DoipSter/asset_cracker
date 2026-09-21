-- The first two sources and the four instruments the Python widget already watches.
-- Adding a coin or a venue later is a row here, not a code change.

insert into actor (kind, handle) values ('system', 'service');

insert into source (code, name, has_market_data, has_execution) values
    ('coinbase', 'Coinbase Exchange', true, false),   -- prices only; nothing is traded here
    ('kalshi',   'Kalshi',            true, true);

insert into instrument (source_id, kind, symbol, underlying, spec)
select s.id, 'spot', v.symbol, v.underlying, '{}'::jsonb
  from source s, (values ('BTC-USD', 'BTC'), ('ETH-USD', 'ETH')) as v (symbol, underlying)
 where s.code = 'coinbase';

-- spec.price_from names the instrument whose trades stand in for the contract's underlying.
-- Kalshi settles on CF Benchmarks' index, not on Coinbase; the gap is measured per round.
-- fee_rate is Kalshi's taker fee: rate x price x (1 - price) per contract, rounded up to a cent.
insert into instrument (source_id, kind, symbol, underlying, spec)
select s.id, 'binary_contract', v.symbol, v.underlying,
       jsonb_build_object(
           'price_from',     'coinbase:' || v.underlying || '-USD',
           'round_seconds',  900,
           'settles_on',     'average of the index over the final 60 seconds',
           'fee_rate',       0.07,
           'position_rule',  'one net position per market',
           'wind_down',      'hold to settlement')
  from source s, (values ('KXBTC15M', 'BTC'), ('KXETH15M', 'ETH')) as v (symbol, underlying)
 where s.code = 'kalshi';

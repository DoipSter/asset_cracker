-- For running the first strategy family live, in simulation.
--
-- engine_state: what a strategy family must remember across a restart and that the ledger
-- cannot tell it (its bet log with model details, counters, the learned index offsets). The
-- ledger stays the record of money: on start the service checks each bucket's cash here against
-- its ledger balance and refuses to trade that series if they disagree.
create table engine_state (
    series    text primary key,
    saved_at  timestamptz not null default now(),
    state     jsonb not null
);

-- What the strategy knew when it placed the order: model probability, edge, underlying price.
alter table trade_order add column detail jsonb not null default '{}';

-- Per-coin starting calibration for the kalshi15m family, as measured by the Python app over a
-- week of settlements (Sep 2026): how far Kalshi's index sits above Coinbase's price, how
-- uncertain that gap is, and the coin's typical volatility per sqrt(second). The engine
-- replaces the offset with its own measurements once three rounds have settled.
update instrument set spec = spec || '{"index_offset_pct": 0.000057, "index_sd_pct": 0.000144, "default_sigma": 8e-5, "decimals": 0}'::jsonb
 where symbol = 'KXBTC15M';
update instrument set spec = spec || '{"index_offset_pct": 0.0000713, "index_sd_pct": 0.0002156, "default_sigma": 9.4e-5, "decimals": 2}'::jsonb
 where symbol = 'KXETH15M';

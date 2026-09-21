-- GET /api/analysis reads each settled round once, by market: its fills and what it paid out.
-- trade_order was indexed only by (bucket_id, placed_at) and settlement not by market at all, so
-- both reads walked the whole table. Neither table is partitioned; both only grow.
--
-- Plain "create index", not "concurrently": migrations run inside one transaction. Both tables
-- were a few hundred rows when this was written (554 orders, 174 settlements on 2026-09-21), so
-- the lock is momentary. NOT YET RUN ANYWHERE: written on a machine with no database.

create index if not exists trade_order_market_id_idx on trade_order (market_id);
create index if not exists settlement_market_id_idx on settlement (market_id);

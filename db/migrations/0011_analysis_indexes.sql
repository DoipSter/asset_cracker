-- GET /api/analysis reads each settled round once, by market: its fills and what it paid out.
-- trade_order is indexed only by (bucket_id, placed_at), which does not serve a read by market
-- (READ FROM 0001_init.sql, not from a query plan: no EXPLAIN has been run). That read happens
-- once per settled window, and again every minute over the unsettled rounds (AnalysisUnsettled
-- joins trade_order to market on market_id). trade_order is not partitioned and only grows.
--
-- settlement needs nothing here. Its "unique (market_id, bucket_id, side)" already has an index
-- that leads with market_id, and that serves "where market_id = any($1) group by market_id,
-- bucket_id, side". An earlier draft of this file added a second index on settlement (market_id)
-- and said the table was not indexed by market at all. That was wrong, and the index would have
-- been one more to maintain on every settlement, inside the runner's own transaction.
--
-- Plain "create index", not "concurrently": migrations run inside one transaction. trade_order
-- was a few hundred rows when this was written (554 orders on 2026-09-21), so the lock is
-- momentary. NOT YET RUN ANYWHERE: written on a machine with no database. After applying it,
-- check with EXPLAIN that the fills read in store.AnalysisWindow and store.AnalysisUnsettled
-- reach trade_order through this index and not by walking the table.

create index if not exists trade_order_market_id_idx on trade_order (market_id);

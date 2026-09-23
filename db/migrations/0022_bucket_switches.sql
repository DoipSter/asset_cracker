-- Three switches the buckets page asked for on 2026-09-23, each a column that changes, none of
-- them a change to a status machine (INT-17).
--
-- bucket.orders_on: the bucket's own new-orders switch. Off, the engine holds it settle-only,
-- exactly as it holds a retired version's bucket: valued, settled and swept, no buy formed.
-- The engine-wide switch (operator_setting) still rules over all of them.
--
-- bucket.close_requested_at: the operator pressed × while the bucket had a position open. The
-- engine closes it (CloseBucket, as it does one that ran out) the first time it holds nothing,
-- which is the sweep after the last settlement, or the next start. Kept once set: a record of
-- when the close was asked for.
--
-- strategy_version.archived_at: a retired version put out of sight of the registry. Its row,
-- its buckets and its results are untouched; the trials count is unchanged. Null puts it back.
--
-- Expand only: every column is nullable or defaulted, so the release before this one still runs.

alter table bucket add column orders_on boolean not null default true;
alter table bucket add column close_requested_at timestamptz;
alter table strategy_version add column archived_at timestamptz;

comment on column bucket.orders_on is 'This bucket''s own new-orders switch: false is held settle-only. The engine-wide switch still rules.';
comment on column bucket.close_requested_at is 'When × was pressed with a position open. The engine closes the bucket the first time it holds nothing.';
comment on column strategy_version.archived_at is 'A retired version taken off the registry''s list. Everything about it is kept; null lists it again.';

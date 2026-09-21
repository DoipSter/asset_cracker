-- Two different things were both being called "tax".
--
--   The SUSTAINMENT ALLOCATION is ours: the share of a bucket's gain above its high-water mark
--   that the platform moves into the money buckets (winnings, replenishment, the reserves).
--   TAX is the government's: the "tax reserve" bucket holds money set aside for it, and
--   skim_policy.tax_bps is the share of the allocation that goes there.
--
-- From here the ledger calls the first one what it is. The old values stay valid, because rows
-- written under them are never edited and an older release must still be able to run.

alter table ledger_transfer drop constraint ledger_transfer_reason_check;
alter table ledger_transfer add constraint ledger_transfer_reason_check check (reason in (
    'deposit', 'withdrawal', 'seed', 'reap', 'take', 'expansion', 'fill', 'fee', 'settlement', 'adjustment',
    'sustainment',   -- the sustainment allocation out of a bucket
    'tax'));         -- kept for rows written before 2026-09-21; not used for new ones

alter table bucket_event drop constraint bucket_event_kind_check;
alter table bucket_event add constraint bucket_event_kind_check check (kind in (
    'seeded', 'tripped', 'winding_down', 'reaped', 'frozen', 'replaced', 'limits_changed', 'paused', 'resumed',
    'allocated',     -- a sustainment allocation was taken from this bucket
    'taxed'));       -- kept for older rows

comment on table skim_policy is 'The sustainment allocation: what share of a bucket''s gain above its high-water mark goes to each money bucket. Dated; the newest row per mode is in force.';
comment on column skim_policy.tax_bps is 'The share set aside for GOVERNMENT tax, into the tax reserve. Not the allocation as a whole.';
comment on table bucket_skim is 'Every new high a bucket made, and the sustainment allocation taken from it.';
comment on column bucket.tax_rate_bps is 'Unused. The allocation is set by skim_policy, not per bucket. Left in place so an older release can still insert buckets.';

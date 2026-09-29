-- The owner's per-bet cap on the rosters (TSK-55, 2026-09-28: "cap the Kelly stake per bet on the
-- rosters").
--
-- On 2026-09-27 at 21:57 PT a roster's member bet 15% of its bucket on one round, in its last two
-- and a half minutes (quarter-Kelly of a belief of about 90% NO against the market's 73%), and lost
-- $178.52. Nothing stopped it: the version's window cap (window_cap_bps) allows 25% of the smaller
-- of equity and seed in a window. Now a bucket's limits may carry bet_cap_bps: what one position,
-- one market and side, may cost at most, on that same base. The engine sizes under it
-- (engine.Size, binding "bet_cap"), whichever member fires. It belongs to the bucket, not the
-- version: the version, its registry row and the trials count are unchanged, and its record carries
-- on across the change, which is dated by the event below.
--
-- Every live roster bucket gets 5% (500 bps), the owner's choice among 2.5%, 5% and 10%. A bucket
-- the service creates for a roster version from here on is created with it (store.RosterBetCapBps),
-- and a bucket's next life keeps its last life's limits. Each change is recorded as a
-- limits_changed event, made by the owner.
--
-- Data only: no column, no constraint. The previous release ignores limits and runs unchanged.

with capped as (
    update bucket b
       set limits = b.limits || jsonb_build_object('bet_cap_bps', 500)
      from strategy_version v
     where v.id = b.strategy_version_id and b.mode = 'sim' and b.status <> 'frozen'
       and jsonb_typeof(v.params->'members') = 'array' and jsonb_array_length(v.params->'members') >= 2
       and not (b.limits ? 'bet_cap_bps')
    returning b.id
)
insert into bucket_event (bucket_id, kind, detail, actor_id)
select c.id, 'limits_changed',
       jsonb_build_object('bet_cap_bps', 500,
                          'note', 'per-bet cap set: one position may cost at most 5% of the smaller of equity and seed (the owner, 2026-09-28)'),
       (select id from actor where handle = 'brad')
  from capped c;

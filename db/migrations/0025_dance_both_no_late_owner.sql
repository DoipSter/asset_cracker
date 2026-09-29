-- Late model may not own Dance both's clocks (TSK-56, the owner's request of 2026-09-28: "stop
-- the dancer from electing Late model as owner").
--
-- A roster elects each clock's owner from its members' shadows over the prior clocks; the owner
-- gets first refusal, and a seat it leaves goes only to a LATER specialist (a smaller tau_max).
-- Late model is Dance both's latest member, so a clock it owned was closed to the other two. On
-- the record (roster_shadow, 2026-09-27 21:15 to 2026-09-28 19:45 PT) it owned 26 clocks: in all
-- 26 another member would have bought, in 16 Late itself never bought and the roster sat the clock
-- out, and its own shadows in the clocks it owned lost 43 cents a dollar. The $178.52 loss of
-- 2026-09-27 21:57 PT was Late as owner. The version's hypothesis reads "keeping Late leftovers
-- while playing Favourite/Value hours".
--
-- A bucket's limits may carry no_owner, the members the election passes over (engine
-- Composition.PassOver): never elected, warmup included, still free to take what an owner leaves.
-- Here it is set on Dance both's live bucket, with a limits_changed event by the owner. It belongs
-- to the bucket: the version, its registry row and the trials count are unchanged, and a next
-- life keeps it. The name is the one the version's params give the member.
--
-- Data only. The previous release ignores it and runs unchanged.

with target as (
    update bucket b
       set limits = b.limits || jsonb_build_object('no_owner', jsonb_build_array('Late model'))
      from strategy_version v
      join strategy st on st.id = v.strategy_id
     where v.id = b.strategy_version_id and b.mode = 'sim' and b.status <> 'frozen'
       and st.family = 'kalshi15m' and st.name = 'Dance both (conventions)'
       and jsonb_path_exists(v.params, '$.members[*] ? (@.name == "Late model")')
       and not (b.limits ? 'no_owner')
    returning b.id
)
insert into bucket_event (bucket_id, kind, detail, actor_id)
select t.id, 'limits_changed',
       jsonb_build_object('no_owner', jsonb_build_array('Late model'),
                          'note', 'Late model may not own a clock; it still takes the seats an owner leaves (the owner, 2026-09-28)'),
       (select id from actor where handle = 'brad')
  from target t;

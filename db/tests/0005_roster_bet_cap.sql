-- Checks the per-bet cap of migration 0024 and service/internal/store: a roster version's bucket
-- is created with bet_cap_bps 500 and a single shape's with none, a next life keeps its last
-- life's limits, the load reads the cap (and reads a limit that is not a number as none), and
-- 0024's rule caps a live roster bucket once, with a limits_changed event by the owner.
-- Makes every row it looks at, under names no one else uses. Rolls back. Run with:  db/test.sh
begin;

insert into actor (kind, handle) values ('system', 'test5');
insert into ledger_account (kind, mode, name) values
    ('bucket', 'sim', 'test5 roster cash'), ('bucket', 'sim', 'test5 single cash'), ('bucket', 'sim', 'test5 roster life 2 cash');
insert into source (code, name, has_market_data) values ('test5', 'Test venue 5', true);
insert into venue_account (source_id, mode, name) select id, 'sim', 'test5 paper' from source where code = 'test5';
insert into strategy (family, name) values ('test5', 'Roster'), ('test5', 'Single');
insert into strategy_version (strategy_id, version, params, code_ref, created_by)
    select st.id, 3, case when st.name = 'Roster' then '{"members": [{"name": "A"}, {"name": "B"}]}'::jsonb else '{}'::jsonb end,
           'test5 ' || st.name, a.id
      from strategy st, actor a where st.family = 'test5' and a.handle = 'test5';

create function pg_temp.version(n text) returns bigint language sql as $$ select id from strategy_version where code_ref = 'test5 ' || n $$;
-- The limits a new bucket is created with, as store.rosterLimits writes them ($4 the version, $5 the cap).
create function pg_temp.limits_for(ver bigint, cap_bps integer) returns jsonb language sql as $$
    select case when jsonb_typeof(v.params->'members') = 'array' and jsonb_array_length(v.params->'members') >= 2
                then jsonb_build_object('bet_cap_bps', cap_bps) else '{}'::jsonb end
      from strategy_version v where v.id = ver $$;

insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
select 'test5 roster', 'sim', va.id, la.id, pg_temp.version('Roster'), pg_temp.limits_for(pg_temp.version('Roster'), 500), 0
  from venue_account va, ledger_account la where va.name = 'test5 paper' and la.name = 'test5 roster cash';
insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
select 'test5 single', 'sim', va.id, la.id, pg_temp.version('Single'), pg_temp.limits_for(pg_temp.version('Single'), 500), 0
  from venue_account va, ledger_account la where va.name = 'test5 paper' and la.name = 'test5 single cash';

do $$
declare l jsonb; n bigint;
begin
    -- 1. A new bucket: a roster's is capped, a single shape's is not.
    select limits into l from bucket where name = 'test5 roster';
    assert l = '{"bet_cap_bps": 500}', 'the roster bucket was created with ' || l;
    select limits into l from bucket where name = 'test5 single';
    assert l = '{}', 'the single shape''s bucket was created with ' || l;
    raise notice 'ok 1: a roster version''s bucket is created with the cap, a single shape''s without';

    -- 2. A next life keeps what its last life's limits said, over the roster default.
    update bucket set limits = '{"bet_cap_bps": 300}' where name = 'test5 roster';
    insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
    select 'test5 roster life 2', 'sim', va.id, la.id, pg_temp.version('Roster'),
           pg_temp.limits_for(pg_temp.version('Roster'), 500) || coalesce((select limits from bucket where name = 'test5 roster'), '{}'), 0
      from venue_account va, ledger_account la where va.name = 'test5 paper' and la.name = 'test5 roster life 2 cash';
    select limits into l from bucket where name = 'test5 roster life 2';
    assert l = '{"bet_cap_bps": 300}', 'the next life has ' || l;
    raise notice 'ok 2: a next life keeps its last life''s limits';

    -- 3. The load reads the cap as store.HeldBuckets does; a limit that is not a number is none.
    update bucket set limits = '{"bet_cap_bps": "lots"}' where name = 'test5 single';
    select (case when jsonb_typeof(limits->'bet_cap_bps') = 'number' then (limits->>'bet_cap_bps')::numeric else 0 end)::bigint
      into n from bucket where name = 'test5 roster life 2';
    assert n = 300, 'read ' || n;
    select (case when jsonb_typeof(limits->'bet_cap_bps') = 'number' then (limits->>'bet_cap_bps')::numeric else 0 end)::bigint
      into n from bucket where name = 'test5 single';
    assert n = 0, 'a text limit read as ' || n;
    raise notice 'ok 3: the load reads the cap, and a limit that is not a number as none';

    -- 4. 0024's rule: a live roster bucket without a cap gets one, once, with the owner's event.
    update bucket set limits = '{}' where name = 'test5 roster life 2';
    for i in 1..2 loop
        with capped as (
            update bucket b set limits = b.limits || jsonb_build_object('bet_cap_bps', 500)
              from strategy_version v
             where v.id = b.strategy_version_id and b.mode = 'sim' and b.status <> 'frozen'
               and jsonb_typeof(v.params->'members') = 'array' and jsonb_array_length(v.params->'members') >= 2
               and not (b.limits ? 'bet_cap_bps') and b.name like 'test5 %'
            returning b.id)
        insert into bucket_event (bucket_id, kind, detail, actor_id)
        select c.id, 'limits_changed', jsonb_build_object('bet_cap_bps', 500, 'note', 'test5'), (select id from actor where handle = 'test5')
          from capped c;
    end loop;
    select limits into l from bucket where name = 'test5 roster life 2';
    assert l = '{"bet_cap_bps": 500}', 'after 0024''s rule: ' || l;
    select count(*) into n from bucket_event e join bucket b on b.id = e.bucket_id where b.name like 'test5 %' and e.kind = 'limits_changed';
    assert n = 1, n || ' events: the rule ran twice and should have capped once';
    select limits into l from bucket where name = 'test5 single';
    assert l = '{"bet_cap_bps": "lots"}', 'the rule touched a single shape''s bucket';
    raise notice 'ok 4: 0024''s rule caps a roster bucket once, with an event, and leaves a single shape alone';
end $$;

rollback;

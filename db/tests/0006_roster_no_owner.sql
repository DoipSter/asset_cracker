-- Checks the no_owner limit of migration 0025: the load reads it as store.HeldBuckets does (a list
-- of names; anything else is none), and 0025's rule sets it once, on a live roster bucket whose
-- version has the member, with a limits_changed event. Makes every row it looks at, under names
-- no one else uses. Rolls back. Run with:  db/test.sh
begin;

insert into actor (kind, handle) values ('system', 'test6');
insert into ledger_account (kind, mode, name) values ('bucket', 'sim', 'test6 roster cash'), ('bucket', 'sim', 'test6 other cash');
insert into source (code, name, has_market_data) values ('test6', 'Test venue 6', true);
insert into venue_account (source_id, mode, name) select id, 'sim', 'test6 paper' from source where code = 'test6';
insert into strategy (family, name) values ('test6', 'Dancer'), ('test6', 'Other');
insert into strategy_version (strategy_id, version, params, code_ref, created_by)
    select st.id, 3, case when st.name = 'Dancer' then '{"members": [{"name": "Favourite"}, {"name": "Late model"}]}'::jsonb
                          else '{"members": [{"name": "Favourite"}, {"name": "Value"}]}'::jsonb end,
           'test6 ' || st.name, a.id
      from strategy st, actor a where st.family = 'test6' and a.handle = 'test6';
insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
select 'test6 ' || st.name, 'sim', va.id, la.id, v.id, '{"bet_cap_bps": 500}', 0
  from strategy st join strategy_version v on v.strategy_id = st.id, venue_account va, ledger_account la
 where st.family = 'test6' and va.name = 'test6 paper' and la.name = 'test6 ' || lower(case when st.name = 'Dancer' then 'roster' else 'other' end) || ' cash';

do $$
declare n bigint; names text[]; l jsonb;
begin
    -- 1. 0025's rule, run twice: the bucket whose version has Late model gets it once; the other none.
    for i in 1..2 loop
        with target as (
            update bucket b set limits = b.limits || jsonb_build_object('no_owner', jsonb_build_array('Late model'))
              from strategy_version v join strategy st on st.id = v.strategy_id
             where v.id = b.strategy_version_id and b.mode = 'sim' and b.status <> 'frozen'
               and st.family = 'test6'
               and jsonb_path_exists(v.params, '$.members[*] ? (@.name == "Late model")')
               and not (b.limits ? 'no_owner')
            returning b.id)
        insert into bucket_event (bucket_id, kind, detail, actor_id)
        select t.id, 'limits_changed', jsonb_build_object('note', 'test6'), (select id from actor where handle = 'test6') from target t;
    end loop;
    select limits into l from bucket where name = 'test6 Dancer';
    assert l = '{"bet_cap_bps": 500, "no_owner": ["Late model"]}', 'the roster bucket has ' || l;
    select limits into l from bucket where name = 'test6 Other';
    assert l = '{"bet_cap_bps": 500}', 'the other bucket has ' || l;
    select count(*) into n from bucket_event e join bucket b on b.id = e.bucket_id where b.name like 'test6 %' and e.kind = 'limits_changed';
    assert n = 1, n || ' events';
    raise notice 'ok 1: the rule sets no_owner once, where the version has the member, and keeps the cap';

    -- 2. The load reads the list; anything but a list is none.
    select (case when jsonb_typeof(limits->'no_owner') = 'array' then array(select jsonb_array_elements_text(limits->'no_owner')) else '{}'::text[] end)
      into names from bucket where name = 'test6 Dancer';
    assert names = array['Late model'], 'read ' || names::text;
    update bucket set limits = limits || '{"no_owner": "Late model"}' where name = 'test6 Other';
    select (case when jsonb_typeof(limits->'no_owner') = 'array' then array(select jsonb_array_elements_text(limits->'no_owner')) else '{}'::text[] end)
      into names from bucket where name = 'test6 Other';
    assert names = '{}'::text[], 'a string read as ' || names::text;
    raise notice 'ok 2: the load reads the list, and anything else as none';
end $$;

rollback;

-- Checks roster_shadow's rules (migration 0023) and the insert the runner makes
-- (store.InsertRosterShadows): one row per bucket, member and market, a repeat passed over,
-- result and P&L together or neither, append-only, and emptied by reset_sim with the books.
-- Like 0003 it makes every row it looks at, under names no one else uses. Runs in one
-- transaction and rolls back. Run with:  db/test.sh
begin;

insert into actor (kind, handle) values ('system', 'test4');
insert into ledger_account (kind, mode, name) values ('bucket', 'sim', 'test4 bucket cash');
insert into source (code, name, has_market_data) values ('test4', 'Test venue 4', true);
insert into venue_account (source_id, mode, name) select id, 'sim', 'test4 paper' from source where code = 'test4';
insert into strategy (family, name) values ('test4', 'Dancer');
insert into strategy_version (strategy_id, version, params, code_ref, created_by)
    select st.id, 3, '{}', 'test4', a.id from strategy st, actor a where st.family = 'test4' and a.handle = 'test4';
insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
    select 'test4 bucket', 'sim', va.id, la.id, v.id, '{}', 0
      from venue_account va, ledger_account la, strategy_version v
     where va.name = 'test4 paper' and la.name = 'test4 bucket cash' and v.code_ref = 'test4';
insert into instrument (source_id, kind, symbol, underlying) select id, 'binary_contract', 'T4-15M', 'DOGE' from source where code = 'test4';
insert into market (instrument_id, ticker, strike) select id, 'T4-15M-1', 0.089 from instrument where symbol = 'T4-15M';

do $$
declare b bigint; m bigint; n bigint;
begin
    select id into b from bucket where name = 'test4 bucket';
    select id into m from market where ticker = 'T4-15M-1';

    -- 1. The runner's insert, twice: the second is passed over.
    for i in 1..2 loop
        insert into roster_shadow (bucket_id, strategy_version_id, member, market_id, side, seen_at, closes_at, owner, owner_how, cost_cents, result, pnl_cents)
        select x.id, x.strategy_version_id, 'Favourite', m, 'yes', now() - interval '5 minutes', now(), 'Late', 'window', 72, 'yes', 28
          from bucket x where x.id = b
        on conflict (bucket_id, member, market_id) do nothing;
    end loop;
    select count(*) into n from roster_shadow where bucket_id = b and member = 'Favourite' and market_id = m;
    assert n = 1, 'the repeat was not passed over: ' || n || ' rows';
    raise notice 'ok 1: one row per bucket, member and market; a repeat adds nothing';

    -- 2. A shadow dropped unscored has neither a result nor a P&L; one of them alone is refused.
    insert into roster_shadow (bucket_id, strategy_version_id, member, market_id, side, closes_at, cost_cents)
    select x.id, x.strategy_version_id, 'Value', m, 'no', now(), 40 from bucket x where x.id = b;
    begin
        insert into roster_shadow (bucket_id, strategy_version_id, member, market_id, side, closes_at, cost_cents, result)
        select x.id, x.strategy_version_id, 'Late', m, 'no', now(), 40, 'no' from bucket x where x.id = b;
        raise exception 'a result without a P&L was accepted';
    exception when check_violation then
        raise notice 'ok 2: a result and its P&L come together or not at all';
    end;

    -- 3. Append-only.
    begin
        update roster_shadow set pnl_cents = 0 where bucket_id = b and member = 'Favourite';
        raise exception 'an update was accepted';
    exception when others then
        if sqlerrm not like '%append-only%' then
            raise;
        end if;
        raise notice 'ok 3: a shadow cannot be changed once written';
    end;

    -- 4. reset_sim empties it with the simulated books, before the buckets its rows name.
    perform reset_sim('reset sim');
    select count(*) into n from roster_shadow s join bucket x on x.id = s.bucket_id where x.mode = 'sim';
    assert n = 0, 'reset_sim left ' || n || ' shadows on simulated buckets';
    select count(*) into n from roster_shadow where bucket_id = b;
    assert n = 0, 'reset_sim left the test bucket''s shadows';
    raise notice 'ok 4: reset_sim empties roster_shadow';
end $$;

rollback;

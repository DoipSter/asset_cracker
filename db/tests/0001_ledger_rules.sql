-- Checks that the database itself enforces the ledger's rules. Runs in one transaction and
-- rolls back, so it leaves nothing behind. Run with:  db/test.sh
--
-- Each "must fail" case runs in a sub-block: if the statement is wrongly accepted, the test
-- raises; if it is refused, the refusal is swallowed and the test goes on.
--
-- It has to pass on a dev database the service has already used, not only on a fresh one. So it
-- never assumes the tables are empty: it measures its own effect (a balance moves by so much,
-- its own rows are there) and only ever tries to change rows it made itself. The snapshot is
-- held for the whole transaction so that a service writing to the ledger at the same moment
-- cannot move a balance between the test's "before" and "after".
begin isolation level repeatable read;

insert into actor (kind, handle) values ('system', 'test');
insert into ledger_account (kind, mode, name) values
    ('external', 'sim', 'owners'), ('bucket', 'sim', 'bucket A cash'), ('bucket', 'sim', 'bucket B cash'),
    ('external', 'real', 'owners');
-- There is one common pool per mode, and the service makes the sim one the first time it runs
-- (EnsureSimSetup). Use the pool that is there; make one only where there is none.
insert into ledger_account (kind, mode, name) values
    ('common_pool', 'sim', 'test common pool'), ('common_pool', 'real', 'test common pool')
    on conflict do nothing;

create function pg_temp.acct(m run_mode, n text) returns bigint language sql as
    $$ select id from ledger_account where mode = m and name = n $$;
create function pg_temp.pool(m run_mode) returns bigint language sql as
    $$ select id from ledger_account where kind = 'common_pool' and mode = m and currency = 'USD' $$;
create function pg_temp.balance(a bigint) returns bigint language sql as
    $$ select balance_cents from ledger_balance where account_id = a $$;
create function pg_temp.me() returns bigint language sql as
    $$ select id from actor where handle = 'test' $$;
create function pg_temp.transfer(m run_mode, r text) returns bigint language sql as
    $$ insert into ledger_transfer (mode, reason, created_by) values (m, r, pg_temp.me()) returning id $$;

do $$
begin
    assert pg_temp.pool('sim') is not null and pg_temp.pool('real') is not null, 'a common pool in each mode';
end $$;

do $$
declare t bigint; pool_before bigint := pg_temp.balance(pg_temp.pool('sim'));
begin
    -- 1. A balanced transfer is accepted, and balances follow.
    t := pg_temp.transfer('sim', 'deposit');
    insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
        (t, pg_temp.acct('sim', 'owners'), 'sim', -100000), (t, pg_temp.pool('sim'), 'sim', 100000);
    t := pg_temp.transfer('sim', 'seed');
    insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
        (t, pg_temp.pool('sim'), 'sim', -15000), (t, pg_temp.acct('sim', 'bucket A cash'), 'sim', 15000);
    set constraints all immediate;
    set constraints all deferred;
    assert pg_temp.balance(pg_temp.pool('sim')) - pool_before = 85000, 'common pool balance';
    assert pg_temp.balance(pg_temp.acct('sim', 'owners')) = -100000, 'owners balance';
    assert pg_temp.balance(pg_temp.acct('sim', 'bucket A cash')) = 15000, 'bucket balance';
    assert (select sum(balance_cents) from ledger_balance where mode = 'sim') = 0, 'the sim ledger sums to zero';
    raise notice 'ok 1: balanced transfers accepted; balances correct; ledger sums to zero';
end $$;

do $$
declare t bigint;
begin
    -- 2. A transfer that does not balance is refused.
    begin
        t := pg_temp.transfer('sim', 'tax');
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
            (t, pg_temp.acct('sim', 'bucket A cash'), 'sim', -500), (t, pg_temp.pool('sim'), 'sim', 400);
        set constraints all immediate;
        raise exception 'TEST FAILED: an unbalanced transfer was accepted';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 2: unbalanced transfer refused (%)', sqlerrm;
    end;
    set constraints all deferred;
end $$;

do $$
declare t bigint;
begin
    -- 3. A transfer with no entries is refused.
    begin
        t := pg_temp.transfer('sim', 'adjustment');
        set constraints all immediate;
        raise exception 'TEST FAILED: an empty transfer was accepted';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 3: empty transfer refused (%)', sqlerrm;
    end;
    set constraints all deferred;
end $$;

do $$
declare t bigint;
begin
    -- 4. Sim and real cannot meet: a sim transfer cannot touch a real account.
    begin
        t := pg_temp.transfer('sim', 'seed');
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
            (t, pg_temp.pool('sim'), 'sim', -1000), (t, pg_temp.pool('real'), 'sim', 1000);
        raise exception 'TEST FAILED: a sim transfer reached a real account';
    exception when foreign_key_violation then
        raise notice 'ok 4: sim transfer into a real account refused';
    end;
    -- ...and an entry cannot claim a different mode from its transfer.
    begin
        t := pg_temp.transfer('sim', 'seed');
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
            (t, pg_temp.pool('real'), 'real', 1000);
        raise exception 'TEST FAILED: a real entry joined a sim transfer';
    exception when foreign_key_violation then
        raise notice 'ok 5: real entry on a sim transfer refused';
    end;
end $$;

do $$
begin
    -- 6. The ledger is append-only.
    begin
        update ledger_entry set amount_cents = amount_cents + 1
         where transfer_id in (select id from ledger_transfer where created_by = pg_temp.me());
        raise exception 'TEST FAILED: a ledger entry was edited';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 6: editing a ledger entry refused';
    end;
    begin
        delete from ledger_transfer where created_by = pg_temp.me();
        raise exception 'TEST FAILED: a ledger transfer was deleted';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 7: deleting a ledger transfer refused';
    end;
end $$;

do $$
declare v bigint; s bigint; va bigint; va_real bigint;
begin
    -- 8. A bucket is one virtual subdivision of one venue account, in the same mode.
    insert into source (code, name, has_market_data) values ('test', 'Test venue', true) returning id into s;
    insert into strategy (family, name) values ('test', 'T');
    insert into strategy_version (strategy_id, version, params, code_ref, created_by)
        select st.id, 1, '{}', 'test', a.id from strategy st, actor a where st.family = 'test' and a.handle = 'test'
        returning id into v;
    insert into venue_account (source_id, mode, name) values (s, 'sim', 'paper') returning id into va;
    insert into venue_account (source_id, mode, name) values (s, 'real', 'live') returning id into va_real;
    insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
        values ('A', 'sim', va, pg_temp.acct('sim', 'bucket A cash'), v, '{}', 1000);
    begin
        insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
            values ('B', 'sim', va_real, pg_temp.acct('sim', 'bucket B cash'), v, '{}', 1000);
        raise exception 'TEST FAILED: a sim bucket was placed in a real venue account';
    exception when foreign_key_violation then
        raise notice 'ok 8: sim bucket in a real venue account refused';
    end;
    begin
        insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
            values ('B', 'sim', va, pg_temp.acct('sim', 'bucket A cash'), v, '{}', 1000);
        raise exception 'TEST FAILED: two buckets shared one virtual account';
    exception when unique_violation then
        raise notice 'ok 8b: two buckets sharing one virtual account refused';
    end;
    begin
        insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
            values ('B', 'sim', va, pg_temp.pool('sim'), v, '{}', 1000);
        raise exception 'TEST FAILED: a bucket used the common pool as its cash';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 8c: bucket using a non-bucket ledger account refused';
    end;
    insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
        values ('B', 'sim', va, pg_temp.acct('sim', 'bucket B cash'), v, '{}', 1000);
    assert (select buckets from venue_account_virtual_cash where venue_account_id = va) = 2, 'two buckets in one venue account';
    assert (select bucket_cash_cents from venue_account_virtual_cash where venue_account_id = va) = 15000, 'virtual cash adds up';
    raise notice 'ok 8d: two buckets subdivide one venue account; their cash adds up to 15000';
    begin
        insert into ledger_account (kind, mode, name) values ('common_pool', 'sim', 'second pool');
        raise exception 'TEST FAILED: a second common pool was created';
    exception when unique_violation then
        raise notice 'ok 9: second common pool in the same mode refused';
    end;
end $$;

do $$
declare e bigint; m bigint; i bigint; b bigint;
begin
    -- 10. The journal takes rows in this month's partition, and weights resolve to the latest.
    insert into instrument (source_id, kind, symbol, underlying) select id, 'binary_contract', 'T15M', 'BTC' from source where code = 'test' returning id into i;
    insert into market (instrument_id, ticker, strike) values (i, 'T15M-1', 100) returning id into m;
    insert into evaluation (at, market_id, underlying_price, quotes) values (now(), m, 101, '{"yes_ask":0.6}') returning id into e;
    insert into decision (at, evaluation_id, bucket_id, strategy_version_id, model_prob, market_prob, side, edge, action, blocked_by, size_alone, human_weight, size_applied)
        select now(), e, b.id, b.strategy_version_id, 0.7, 0.6, 'yes', 0.04, 'none', 'cooling down', 10, 0.5, 5
          from bucket b where b.name = 'A'
        returning bucket_id into b;
    insert into human_weight (set_by, target_kind, target_id, weight, set_at) values (pg_temp.me(), 'bucket', b, 0.5, now() - interval '1 hour');
    insert into human_weight (set_by, target_kind, target_id, weight, set_at) values (pg_temp.me(), 'bucket', b, 2,   now());
    assert (select weight from current_human_weight where target_kind = 'bucket' and target_id = b) = 2, 'latest weight wins';
    assert (select count(*) from decision where evaluation_id = e) = 1, 'decision stored';
    raise notice 'ok 10: journal rows land in a partition; latest human weight wins';
end $$;

rollback;

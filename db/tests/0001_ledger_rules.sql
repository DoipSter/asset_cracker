-- Checks that the database itself enforces the ledger's rules. Runs in one transaction and
-- rolls back, so it leaves nothing behind. Run with:  db/test.sh
--
-- Each "must fail" case runs in a sub-block: if the statement is wrongly accepted, the test
-- raises; if it is refused, the refusal is swallowed and the test goes on.
begin;

insert into actor (kind, handle) values ('system', 'test');
insert into ledger_account (kind, mode, name) values
    ('external', 'sim', 'owners'), ('common_pool', 'sim', 'common pool'), ('bucket', 'sim', 'bucket A cash'), ('bucket', 'sim', 'bucket B cash'),
    ('external', 'real', 'owners'), ('common_pool', 'real', 'common pool');

create function pg_temp.acct(m run_mode, n text) returns bigint language sql as
    $$ select id from ledger_account where mode = m and name = n $$;
create function pg_temp.transfer(m run_mode, r text) returns bigint language sql as
    $$ insert into ledger_transfer (mode, reason, created_by) select m, r, id from actor where handle = 'test' returning id $$;

do $$
declare t bigint;
begin
    -- 1. A balanced transfer is accepted, and balances follow.
    t := pg_temp.transfer('sim', 'deposit');
    insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
        (t, pg_temp.acct('sim', 'owners'), 'sim', -100000), (t, pg_temp.acct('sim', 'common pool'), 'sim', 100000);
    t := pg_temp.transfer('sim', 'seed');
    insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
        (t, pg_temp.acct('sim', 'common pool'), 'sim', -15000), (t, pg_temp.acct('sim', 'bucket A cash'), 'sim', 15000);
    set constraints all immediate;
    set constraints all deferred;
    assert (select balance_cents from ledger_balance where mode = 'sim' and name = 'common pool') = 85000, 'common pool balance';
    assert (select balance_cents from ledger_balance where mode = 'sim' and name = 'bucket A cash') = 15000, 'bucket balance';
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
            (t, pg_temp.acct('sim', 'bucket A cash'), 'sim', -500), (t, pg_temp.acct('sim', 'common pool'), 'sim', 400);
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
            (t, pg_temp.acct('sim', 'common pool'), 'sim', -1000), (t, pg_temp.acct('real', 'common pool'), 'sim', 1000);
        raise exception 'TEST FAILED: a sim transfer reached a real account';
    exception when foreign_key_violation then
        raise notice 'ok 4: sim transfer into a real account refused';
    end;
    -- ...and an entry cannot claim a different mode from its transfer.
    begin
        t := pg_temp.transfer('sim', 'seed');
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values
            (t, pg_temp.acct('real', 'common pool'), 'real', 1000);
        raise exception 'TEST FAILED: a real entry joined a sim transfer';
    exception when foreign_key_violation then
        raise notice 'ok 5: real entry on a sim transfer refused';
    end;
end $$;

do $$
begin
    -- 6. The ledger is append-only.
    begin
        update ledger_entry set amount_cents = amount_cents + 1;
        raise exception 'TEST FAILED: a ledger entry was edited';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 6: editing a ledger entry refused';
    end;
    begin
        delete from ledger_transfer;
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
            values ('B', 'sim', va, pg_temp.acct('sim', 'common pool'), v, '{}', 1000);
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
declare e bigint; m bigint; i bigint;
begin
    -- 10. The journal takes rows in this month's partition, and weights resolve to the latest.
    insert into instrument (source_id, kind, symbol, underlying) select id, 'binary_contract', 'T15M', 'BTC' from source where code = 'test' returning id into i;
    insert into market (instrument_id, ticker, strike) values (i, 'T15M-1', 100) returning id into m;
    insert into evaluation (at, market_id, underlying_price, quotes) values (now(), m, 101, '{"yes_ask":0.6}') returning id into e;
    insert into decision (at, evaluation_id, bucket_id, strategy_version_id, model_prob, market_prob, side, edge, action, blocked_by, size_alone, human_weight, size_applied)
        select now(), e, b.id, b.strategy_version_id, 0.7, 0.6, 'yes', 0.04, 'none', 'cooling down', 10, 0.5, 5
          from bucket b where b.name = 'A';
    insert into human_weight (set_by, target_kind, target_id, weight, set_at) select id, 'bucket', 1, 0.5, now() - interval '1 hour' from actor where handle = 'test';
    insert into human_weight (set_by, target_kind, target_id, weight, set_at) select id, 'bucket', 1, 2,   now() from actor where handle = 'test';
    assert (select weight from current_human_weight where target_kind = 'bucket' and target_id = 1) = 2, 'latest weight wins';
    assert (select count(*) from decision) = 1, 'decision stored';
    raise notice 'ok 10: journal rows land in a partition; latest human weight wins';
end $$;

rollback;

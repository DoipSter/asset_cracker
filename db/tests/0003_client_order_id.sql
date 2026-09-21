-- Checks what migration 0012 adds, and the shapes the third engine's recorder
-- (service/internal/store/sim3.go) leans on: an order id made by the engine is used once, null may
-- repeat, an order may be partly filled or not filled at all, every fill has a balanced transfer
-- of its own, and the two reads that decide "what is still held" mean what sim3.go takes them to
-- mean. Runs in one transaction and rolls back. Run with:  db/test.sh
--
-- Like 0001 it has to pass on a dev database the service has already used: it makes every row it
-- looks at, under names no one else uses, and never counts a whole table.
--
-- WRITTEN WITHOUT A DATABASE TO RUN IT ON (2026-09-21). Until it has passed once, a failure
-- here may be this file's mistake and not the schema's.
--
-- The figures are the worked example of docs/honest-fills-v3.md section 2.4 (a real recorded
-- book): a sale of 348 YES that finds 43 at 0.1000 and 100 at 0.0990 in this test's two levels.
begin isolation level repeatable read;

insert into actor (kind, handle) values ('system', 'test3');
insert into ledger_account (kind, mode, name) values
    ('bucket', 'sim', 'test3 bucket cash'), ('venue', 'sim', 'test3 venue'), ('fees', 'sim', 'test3 fees');
insert into source (code, name, has_market_data) values ('test3', 'Test venue 3', true);
insert into venue_account (source_id, mode, name) select id, 'sim', 'test3 paper' from source where code = 'test3';
insert into strategy (family, name) values ('test3', 'T3');
insert into strategy_version (strategy_id, version, params, code_ref, created_by)
    select st.id, 3, '{}', 'test', a.id from strategy st, actor a where st.family = 'test3' and a.handle = 'test3';
insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
    select 'test3 bucket', 'sim', va.id, la.id, v.id, '{}', 0
      from venue_account va, ledger_account la, strategy_version v
     where va.name = 'test3 paper' and la.name = 'test3 bucket cash' and v.code_ref = 'test' and v.version = 3
       and v.strategy_id = (select id from strategy where family = 'test3');
insert into instrument (source_id, kind, symbol, underlying) select id, 'binary_contract', 'T3-15M', 'DOGE' from source where code = 'test3';
insert into market (instrument_id, ticker, strike) select id, 'T3-15M-1', 0.089 from instrument where symbol = 'T3-15M';

create function pg_temp.acct(n text) returns bigint language sql as
    $$ select id from ledger_account where mode = 'sim' and name = n $$;
create function pg_temp.bucket() returns bigint language sql as
    $$ select id from bucket where name = 'test3 bucket' $$;
create function pg_temp.market() returns bigint language sql as
    $$ select id from market where ticker = 'T3-15M-1' $$;
-- An order as sim3.go writes one. cid null is how the first two engines write theirs.
create function pg_temp.place(cid text, act text, sd text, q numeric, lim text, st text) returns bigint language sql as
    $$ insert into trade_order (bucket_id, market_id, broker, action, side, qty, limit_price, status, client_order_id)
       values (pg_temp.bucket(), pg_temp.market(), 'paper', act, sd, q, lim::numeric, st, cid) returning id $$;
-- One fill with its own transfer, as sim3.go writes one: zero amounts are left out.
create function pg_temp.fill(ord bigint, q numeric, px text, bucket_cents bigint, venue_cents bigint, fee bigint) returns bigint language plpgsql as $$
declare t bigint;
begin
    insert into ledger_transfer (mode, reason, memo, created_by)
        values ('sim', 'fill', 'test3', (select id from actor where handle = 'test3')) returning id into t;
    if bucket_cents <> 0 then
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values (t, pg_temp.acct('test3 bucket cash'), 'sim', bucket_cents);
    end if;
    if venue_cents <> 0 then
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values (t, pg_temp.acct('test3 venue'), 'sim', venue_cents);
    end if;
    if fee <> 0 then
        insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values (t, pg_temp.acct('test3 fees'), 'sim', fee);
    end if;
    insert into fill (order_id, at, qty, price, fee_cents, transfer_id) values (ord, now(), q, px::numeric, fee, t);
    return t;
end $$;

do $$
declare o bigint;
begin
    -- 1. The engine's own order id is accepted once and refused the second time.
    o := pg_temp.place('v3:test3:1:1', 'buy', 'yes', 200, '0.1300', 'filled');
    perform pg_temp.fill(o, 200, '0.1200', -2548, 2400, 148);
    perform pg_temp.place('v3:test3:1:2', 'sell', 'no', 5, '0.5000', 'rejected');
    begin
        perform pg_temp.place('v3:test3:1:1', 'buy', 'yes', 1, '0.1300', 'cancelled');
        raise exception 'TEST FAILED: a client order id was used twice';
    exception when unique_violation then
        raise notice 'ok 1: a second order with the same client order id refused';
    end;
end $$;

do $$
declare n bigint;
begin
    -- 2. Null is not an id: the first two engines' orders all leave it null.
    perform pg_temp.place(null, 'buy', 'no', 1, '0.92', 'filled');
    perform pg_temp.place(null, 'buy', 'no', 1, '0.92', 'filled');
    perform pg_temp.place(null, 'buy', 'no', 1, '0.92', 'filled');
    select count(*) into n from trade_order where bucket_id = pg_temp.bucket() and client_order_id is null;
    assert n = 3, 'orders with no client order id: ' || n;
    raise notice 'ok 2: null client order id allowed many times';
end $$;

do $$
declare o bigint; c bigint; filled numeric; asked numeric; n bigint; cents bigint;
begin
    -- 3. A partly filled order: qty is what was asked for, the fills add up to less, and each
    --    fill has a balanced transfer of its own.
    o := pg_temp.place('v3:test3:2:1', 'sell', 'yes', 348, '0.0900', 'partial');
    perform pg_temp.fill(o, 43,  '0.1000', 402, -430, 28);
    perform pg_temp.fill(o, 100, '0.0990', 928, -990, 62);
    set constraints all immediate;
    set constraints all deferred;
    select qty into asked from trade_order where id = o;
    select sum(qty), count(*), count(distinct transfer_id) into filled, n, c from fill where order_id = o;
    assert asked = 348 and filled = 143 and filled < asked, 'asked ' || asked || ', filled ' || filled;
    assert n = 2 and c = 2, 'fills ' || n || ', transfers ' || c;
    raise notice 'ok 3: a partial order keeps the requested qty; its fills sum below it; one balanced transfer per fill';

    -- 4. An order that found nothing: a row, no fill, no money.
    o := pg_temp.place('v3:test3:3:1', 'sell', 'yes', 9, '0.0900', 'cancelled');
    assert (select count(*) from fill where order_id = o) = 0, 'a cancelled order has no fill';
    raise notice 'ok 4: a cancelled order is a row with no fill';

    -- 4b. A broker rejection is journaled too. When its limit is not a price a contract can trade
    --     at, sim3.go writes limit_price as null (the same "$8::text::numeric", sent a null) and
    --     keeps the limit as it was sent, beside the broker's reason, in detail.
    insert into trade_order (bucket_id, market_id, broker, action, side, qty, limit_price, status, client_order_id, detail)
        values (pg_temp.bucket(), pg_temp.market(), 'paper', 'sell', 'yes', 9, null::text::numeric, 'rejected', 'v3:test3:3:2',
                '{"reason": "the limit is outside 0.0001..0.9999", "limit_not_stored": "0.0000"}')
        returning id into o;
    assert (select limit_price is null and detail->>'limit_not_stored' = '0.0000'
                   and detail->>'reason' = 'the limit is outside 0.0001..0.9999'
              from trade_order where id = o), 'the rejected order did not read back as written';
    assert (select count(*) from fill where order_id = o) = 0, 'a rejected order has no fill';
    raise notice 'ok 4b: a rejected order with no storable limit is a row: null limit_price, the reason and the limit in detail';

    -- 5. A sale whose premium equals its fee share has NO bucket entry (an entry may not be
    --    zero). Venue and fees still balance, and the rebuild's left join reads the missing
    --    entry as 0 cents.
    o := pg_temp.place('v3:test3:4:1', 'sell', 'yes', 1, '0.0001', 'filled');
    perform pg_temp.fill(o, 1, '0.0100', 0, -1, 1);
    set constraints all immediate;
    set constraints all deferred;
    select coalesce(e.amount_cents, 0) into cents
      from trade_order t
      join fill f              on f.order_id = t.id
      join bucket b            on b.id = t.bucket_id
      left join ledger_entry e on e.transfer_id = f.transfer_id and e.account_id = b.ledger_account_id
     where t.id = o;
    assert cents = 0, 'the missing bucket entry read as ' || cents;
    raise notice 'ok 5: a fill with no bucket entry balances and reads as 0';

    -- 6. A fill with no entries at all is refused: its transfer would be empty.
    begin
        o := pg_temp.place('v3:test3:5:1', 'sell', 'yes', 1, '0.0001', 'filled');
        perform pg_temp.fill(o, 1, '0.0001', 0, 0, 0);
        set constraints all immediate;
        raise exception 'TEST FAILED: a fill that moved nothing was accepted';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 6: a fill worth nothing refused (%)', sqlerrm;
    end;
    set constraints all deferred;
end $$;

do $$
declare n bigint; q int; cash bigint;
begin
    -- 7. OpenQty's statement (sim3.go): bought 200 yes, sold 143 + 1 yes, so 56 are still held.
    --    The three 'no' orders of step 2 say 'filled' and have no fill row, so they count for
    --    nothing: what is held is never read from trade_order.qty or from a status.
    select count(*) into n from (
        select o.bucket_id, o.side, sum(case when o.action = 'buy' then f.qty else -f.qty end)::int
          from trade_order o join fill f on f.order_id = o.id
         where o.market_id = pg_temp.market() and o.bucket_id = any(array[pg_temp.bucket()])
         group by o.bucket_id, o.side
        having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0) x;
    select sum(case when o.action = 'buy' then f.qty else -f.qty end)::int into q
      from trade_order o join fill f on f.order_id = o.id
     where o.market_id = pg_temp.market() and o.bucket_id = pg_temp.bucket() and o.side = 'yes';
    assert n = 1 and q = 56, 'open lots ' || n || ', yes held ' || q;
    raise notice 'ok 7: what is held is the fills added up, bought less sold: 56 yes';

    -- 8. BucketCash's statement: the bucket's cash is the sum of its entries, and only its own.
    select coalesce(sum(e.amount_cents), 0)::bigint into cash
      from bucket b left join ledger_entry e on e.account_id = b.ledger_account_id
     where b.id = any(array[pg_temp.bucket()]) group by b.id;
    assert cash = -2548 + 402 + 928, 'bucket cash ' || cash;
    raise notice 'ok 8: bucket cash is the ledger sum: % cents', cash;
end $$;

do $$
begin
    -- 9. A fill is append-only, so a partial order is never "topped up" by editing one.
    begin
        update fill set qty = qty + 1 where order_id in (select id from trade_order where bucket_id = pg_temp.bucket());
        raise exception 'TEST FAILED: a fill was edited';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 9: editing a fill refused';
    end;
end $$;

rollback;

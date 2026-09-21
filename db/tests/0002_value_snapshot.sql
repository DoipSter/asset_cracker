-- Checks the value history's rules, and that the two queries the home page leans on (the newest
-- snapshot at or before a moment, and the binned series) mean what service/internal/store/home.go
-- takes them to mean. Runs in one transaction and rolls back. Run with:  db/test.sh
--
-- WRITTEN WITHOUT A DATABASE TO RUN IT ON (2026-09-21). Until it has passed once, a failure
-- here may be this file's mistake and not the schema's.
begin;

-- Five hours of history, one total a minute, ending at a fixed moment. Value climbs a cent a minute.
insert into value_snapshot (at, mode, scope, key, value_cents, cash_cents, at_risk_cents, contributed_cents, unmarked)
select timestamptz '2026-09-21 12:00:00+00' - make_interval(mins => m), 'sim', 'total', 'test-all',
       1000000 + (300 - m), 1000000, 0, 1000000, 0
  from generate_series(0, 300) m;

do $$
declare n bigint; v bigint;
begin
    -- 1. The newest snapshot at or before a moment.
    select value_cents into v from value_snapshot
     where mode = 'sim' and scope = 'total' and key = 'test-all' and at <= timestamptz '2026-09-21 11:00:30+00'
     order by at desc limit 1;
    assert v = 1000000 + 240, 'at-or-before picked ' || v;
    raise notice 'ok 1: the snapshot at or before a moment is the newest one not after it';

    -- 2. Nothing at or before a moment older than the history: the API then falls back to the first.
    select count(*) into n from (select 1 from value_snapshot
     where mode = 'sim' and scope = 'total' and key = 'test-all' and at <= timestamptz '2026-09-20 12:00:00+00'
     order by at desc limit 1) x;
    assert n = 0, 'found a snapshot older than the history';
    raise notice 'ok 2: no snapshot before the history began';

    -- 3. The binned series: 301 rows in 60-second bins is every row; in 3600-second bins it is
    --    six points, each the LAST value recorded in its hour, oldest first.
    select count(*) into n from (
        select (array_agg(extract(epoch from at)::bigint order by at desc))[1]
          from value_snapshot where mode = 'sim' and scope = 'total' and key = 'test-all' and at >= timestamptz '2026-09-21 07:00:00+00'
         group by floor(extract(epoch from at) / 60::float8)) x;
    assert n = 301, 'minute bins gave ' || n;
    select count(*), max(v2) into n, v from (
        select (array_agg(extract(epoch from at)::bigint order by at desc))[1] as t,
               (array_agg(value_cents order by at desc))[1] as v2
          from value_snapshot where mode = 'sim' and scope = 'total' and key = 'test-all' and at >= timestamptz '2026-09-21 07:00:00+00'
         group by floor(extract(epoch from at) / 3600::float8) order by 1) x;
    assert n = 6, 'hour bins gave ' || n;
    assert v = 1000300, 'the last bin''s value is ' || v;
    raise notice 'ok 3: the series bins to the last value recorded in each bin';
end $$;

do $$
begin
    -- 4. Append-only, like the other history tables.
    begin
        update value_snapshot set value_cents = 0 where key = 'test-all';
        raise exception 'TEST FAILED: a value snapshot was edited';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 4: editing a value snapshot refused';
    end;
    begin
        delete from value_snapshot where key = 'test-all';
        raise exception 'TEST FAILED: a value snapshot was deleted';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 5: deleting a value snapshot refused';
    end;
    -- 6. Only the four scopes.
    begin
        insert into value_snapshot (at, mode, scope, key, value_cents, cash_cents, at_risk_cents, contributed_cents)
        values (now(), 'sim', 'planet', 'x', 0, 0, 0, 0);
        raise exception 'TEST FAILED: an unknown scope was accepted';
    exception when others then
        if sqlerrm like 'TEST FAILED%' then raise; end if;
        raise notice 'ok 6: an unknown scope refused';
    end;
end $$;

rollback;

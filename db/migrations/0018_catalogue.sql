-- The catalogue: what the venues list that the service could record, searchable on the assets page
-- (GET /assets, GET /api/catalogue), and the journal of what was switched on and off there
-- (POST /api/controls/asset). 0013 is reserved by the frozen v3 measurement protocol; 0017 is the
-- sim controls.
--
-- catalogue_item is REFERENCE DATA the service rewrites at start and once a day: upserted on
-- (source, code), not append-only. Kalshi: GET /series?category=<each of AC_CATALOGUE_CATEGORIES,
-- default Crypto> (274 series on 2026-09-21), and up to 100 open markets of each series to tell
-- what kind it is. Coinbase: GET /products, USD-quoted and online only (403 on 2026-09-21). An item
-- the venue stops listing keeps its row; last_seen says when it was last listed.
--
--   frequency  Kalshi's (fifteen_min, hourly, daily, weekly, monthly, annual, one_off, custom);
--              'continuous' for a Coinbase product.
--   recorder   the existing recorder that can follow it: 'round' (the 15-minute round poller, one
--              market a round with a floor strike), 'ladder' (the above/below ladder recorder,
--              every open market strike type "greater"), 'candles' (Coinbase daily, hourly and
--              minute candles), or '' when none can yet, and why_not says why.
--   checked_at when its open markets were last read. When a refresh cannot read them the row keeps
--              the recorder, what and why_not it had.
--   raw        the venue's listing entry, as received.
--
-- asset_selection is the journal of the switch: one row per change, append-only. Switching an item
-- on creates its instrument row, or sets an old one active again, with spec "selected": true;
-- switching it off sets instrument.active = false and keeps everything recorded. The instruments
-- the migrations seeded (0002, 0006, 0014) have no "selected" and are refused there: they run as
-- they always have. actor_id is the service actor, as for the other controls: the page has no
-- login, and is reached only through the SSH tunnel.
--
-- NOT YET RUN ANYWHERE: written without a database.

create table catalogue_item (
    id          bigint generated always as identity primary key,
    source      text not null references source (code),
    code        text not null,
    title       text not null default '',
    category    text not null default '',
    frequency   text not null default '',
    what        text not null default '',
    recorder    text not null default '' check (recorder in ('', 'round', 'ladder', 'candles')),
    recordable  boolean generated always as (recorder <> '') stored,
    why_not     text not null default '',
    checked_at  timestamptz,
    first_seen  timestamptz not null default now(),
    last_seen   timestamptz not null default now(),
    raw         jsonb not null default '{}',
    unique (source, code),
    check ((recorder = '') = (why_not <> ''))
);
comment on table catalogue_item is 'What Kalshi (configured categories) and Coinbase (USD, online) list, refreshed daily by the service. Reference data: upserted, not append-only.';
comment on column catalogue_item.recorder is 'round, ladder or candles: the existing recorder that can follow it. Empty when none can yet; why_not says why.';

create table asset_selection (
    id             bigint generated always as identity primary key,
    at             timestamptz not null default now(),
    actor_id       bigint not null references actor,
    via            text not null,
    source         text not null references source (code),
    code           text not null,
    instrument_id  bigint not null references instrument,
    record         boolean not null
);
create trigger append_only before update or delete on asset_selection for each row execute function forbid_change();
comment on table asset_selection is 'Every switch of recording on (record true) or off from the assets page. Append-only.';

-- The service inserts both and updates the catalogue's listing columns. db/grants.sql's blanket
-- grant covers select and insert in the real database; the column update is granted here.
do $grant$
begin
    if exists (select 1 from pg_roles where rolname = 'assetcracker') then
        grant select, insert on catalogue_item, asset_selection to assetcracker;
        grant update (title, category, frequency, what, recorder, why_not, checked_at, last_seen, raw)
            on catalogue_item to assetcracker;
    end if;
end
$grant$;

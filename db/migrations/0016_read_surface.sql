-- The read surface's derived layer (docs/mcp-read-surface.md): what agents and research read
-- through `assetcracker mcp` beside the recorded data. Two objects. Neither is money, and nothing
-- trades from either.
--
-- candle_return   one row per stored candle with its natural-log return over the previous candle
--                 of the same instrument and length: the convention every summary uses, written
--                 once. A VIEW, not a table: it is computed from candle when read, filtered by
--                 instrument and length through candle's unique index, and needs no refresh and
--                 no owner. Over a window it is as fast as the candles under it.
--
-- analysis_result where an analysis, once computed, is written down so it can be read back
--                 through the same surface as the data it came from, instead of living in /tmp or
--                 a JSON file: which analysis (key), by what (source, code_sha), over which window
--                 and parameters, when, and the result as JSON. APPEND-ONLY: a result is never
--                 edited; a rerun is a new row, and the two can be compared. Written by research
--                 tools and by SQL run by hand; the MCP surface only reads it.
--
--                 key      a dotted path naming the analysis: candle.momentum_grid.daily,
--                          candle.vol_profile.hour, spot.train ...
--                 source   what computed it: 'sql', 'cmd/spot', 'research/analyse_week.py', 'mcp'.
--                 code_sha the git sha of the code that computed it, when there is one.
--                 params   the inputs that make the result reproducible (lookbacks, fees, coins).
--                 result   the numbers. Shape is the analysis's own; the key says which.
--
-- Read by the read-only role in the real database (db/grants.sql). Written there by the service
-- role, and in dev by whoever owns the tables.

create view candle_return as
select c.instrument_id, c.granularity_s, c.at, c.close,
       ln(c.close / lag(c.close) over (partition by c.instrument_id, c.granularity_s order by c.at)) as log_return
  from candle c;

comment on view candle_return is 'Each stored candle with ln(close / previous close) of the same instrument and length. Computed on read. Not money.';

create table analysis_result (
    id           bigint generated always as identity primary key,
    key          text not null check (key <> '' and key !~ '\s'),
    source       text not null check (source <> ''),
    code_sha     text,
    params       jsonb not null default '{}'::jsonb,
    window_from  timestamptz,
    window_to    timestamptz,
    computed_at  timestamptz not null default now(),
    result       jsonb not null,
    check (window_from is null or window_to is null or window_to >= window_from)
);
create index on analysis_result (key, computed_at desc);
create trigger append_only before update or delete on analysis_result for each row execute function forbid_change();

comment on table analysis_result is 'Analyses written down once computed, for the read surface. Append-only: a rerun is a new row. Not money.';
comment on column analysis_result.key is 'Dotted path naming the analysis, e.g. candle.momentum_grid.daily.';
comment on column analysis_result.params is 'The inputs that make the result reproducible.';
comment on column analysis_result.result is 'The numbers, in the shape the analysis chooses; the key says which.';

-- The second version of doipster's strategy family (kalshi_trader.py on main at dc10fd4), ported to
-- Go in service/internal/kalshi15m2 and checked against the Python by replay.
--
-- What is different from version 1: one balance per strategy shared across every coin; a $1,000
-- bank with at most $250 at risk at once; a Scalper that trades often and banks gains; a strategy
-- that runs out is closed and a new bucket staked in its place; and every strategy has an
-- anti-world twin that takes the other side of each bet for the same stake and retires when it
-- runs out. Version 1 keeps running beside it: results attach to versions.
--
-- Params were generated from the Go definitions. These twelve rows take the trials registry from
-- six entries to eighteen.

with family (name, blurb, anti, params) as (values
    ('Value', 'Model + market blend, holds', false, '{"name":"Value","blurb":"Model + market blend, holds","shrink":0.5,"min_edge":0.03,"max_bets":1,"exit":"hold","tau_min":8,"tau_max":900,"band_min":0.05,"band_max":0.95,"min_gap":20,"max_stake":0.2,"min_hold":15}'::jsonb),
    ('Model', 'Trusts the model, adds bets', false, '{"name":"Model","blurb":"Trusts the model, adds bets","shrink":1,"min_edge":0.05,"max_bets":3,"exit":"hold","tau_min":8,"tau_max":900,"band_min":0.05,"band_max":0.95,"min_gap":20,"max_stake":0.2,"min_hold":15}'::jsonb),
    ('Late', 'Only bets the last 2.5 minutes', false, '{"name":"Late","blurb":"Only bets the last 2.5 minutes","shrink":1,"min_edge":0.03,"max_bets":2,"exit":"hold","tau_min":8,"tau_max":150,"band_min":0.05,"band_max":0.95,"min_gap":20,"max_stake":0.2,"min_hold":15}'::jsonb),
    ('Scalper', 'Trades often, banks small gains', false, '{"name":"Scalper","blurb":"Trades often, banks small gains","shrink":0.5,"min_edge":0.03,"max_bets":25,"exit":"ev","tau_min":25,"tau_max":900,"band_min":0.05,"band_max":0.95,"min_gap":8,"max_stake":0.015,"min_hold":5,"take_capture":0.8}'::jsonb),
    ('Favorite', 'Backs the favorite late', false, '{"name":"Favorite","blurb":"Backs the favorite late","shrink":1,"min_edge":0,"max_bets":1,"exit":"hold","tau_min":8,"tau_max":240,"band_min":0.62,"band_max":0.88,"min_gap":20,"max_stake":0.2,"min_hold":15}'::jsonb),
    ('Lottery', 'Cheap longshots after a vol spike', false, '{"name":"Lottery","blurb":"Cheap longshots after a vol spike","shrink":1,"min_edge":0,"max_bets":1,"exit":"hold","tau_min":180,"tau_max":900,"band_min":0,"band_max":0.15,"lottery":true,"min_gap":20,"max_stake":0.2,"min_hold":15}'::jsonb),
    ('Anti Value', 'Takes the other side of Value', true, '{"name":"Anti Value","blurb":"Takes the other side of Value","shrink":0.5,"min_edge":0.03,"max_bets":1,"exit":"hold","tau_min":8,"tau_max":900,"band_min":0.05,"band_max":0.95,"min_gap":20,"max_stake":0.2,"min_hold":15,"anti":true}'::jsonb),
    ('Anti Model', 'Takes the other side of Model', true, '{"name":"Anti Model","blurb":"Takes the other side of Model","shrink":1,"min_edge":0.05,"max_bets":3,"exit":"hold","tau_min":8,"tau_max":900,"band_min":0.05,"band_max":0.95,"min_gap":20,"max_stake":0.2,"min_hold":15,"anti":true}'::jsonb),
    ('Anti Late', 'Takes the other side of Late', true, '{"name":"Anti Late","blurb":"Takes the other side of Late","shrink":1,"min_edge":0.03,"max_bets":2,"exit":"hold","tau_min":8,"tau_max":150,"band_min":0.05,"band_max":0.95,"min_gap":20,"max_stake":0.2,"min_hold":15,"anti":true}'::jsonb),
    ('Anti Scalper', 'Takes the other side of Scalper', true, '{"name":"Anti Scalper","blurb":"Takes the other side of Scalper","shrink":0.5,"min_edge":0.03,"max_bets":25,"exit":"ev","tau_min":25,"tau_max":900,"band_min":0.05,"band_max":0.95,"min_gap":8,"max_stake":0.015,"min_hold":5,"take_capture":0.8,"anti":true}'::jsonb),
    ('Anti Favorite', 'Takes the other side of Favorite', true, '{"name":"Anti Favorite","blurb":"Takes the other side of Favorite","shrink":1,"min_edge":0,"max_bets":1,"exit":"hold","tau_min":8,"tau_max":240,"band_min":0.62,"band_max":0.88,"min_gap":20,"max_stake":0.2,"min_hold":15,"anti":true}'::jsonb),
    ('Anti Lottery', 'Takes the other side of Lottery', true, '{"name":"Anti Lottery","blurb":"Takes the other side of Lottery","shrink":1,"min_edge":0,"max_bets":1,"exit":"hold","tau_min":180,"tau_max":900,"band_min":0,"band_max":0.15,"lottery":true,"min_gap":20,"max_stake":0.2,"min_hold":15,"anti":true}'::jsonb)
), s as (
    insert into strategy (family, name, description)
    select 'kalshi15m', name, blurb from family
    on conflict (family, name) do update set description = strategy.description
    returning id, name
)
insert into strategy_version (strategy_id, version, params, code_ref, hypothesis, parent_version_id, created_by, status)
select s.id, 2, f.params,
       'kalshi_trader.py@dc10fd4, ported in service/internal/kalshi15m2',
       case when f.anti then 'A measurement, not a competitor: if a strategy and its twin BOTH lose, the losses are spread and fees rather than bad judgement; if the twin wins, the original is systematically wrong. Stake-matched, so not a hedge.'
            when f.name = 'Scalper' then 'Trades often (25 a round, 1.5% of cash each) and banks a gain once the bid covers 80% of the way to a dollar. Stop-losses were measured and found worse at every setting, so none is set.'
            when f.name = 'Lottery' then 'Kept as a documented negative result. Expected to lose.'
            else 'Version 1''s rules on a shared $1,000 balance across five coins, capped at $250 at risk.' end,
       (select v.id from strategy_version v where v.strategy_id = s.id and v.version = 1),
       (select id from actor where handle = 'doipster'),
       'probation'
  from s join family f using (name);

-- Which coins version 2 trades: all five. ("trade": false still keeps version 1 off the new three.)
update instrument set spec = spec || '{"v2": true}'::jsonb
 where symbol in ('KXBTC15M', 'KXETH15M', 'KXSOL15M', 'KXXRP15M', 'KXDOGE15M');

-- The first strategy family: doipster's six strategies for Kalshi's 15-minute crypto markets,
-- ported to Go in service/internal/kalshi15m and checked against the Python by replay.
-- These rows are the first six entries in the trials registry.
--
-- The params were generated from the Go definitions, so the JSON keys are the ones the service
-- reads. Changing a parameter means a NEW version row, never an edit: results attach to versions.

insert into actor (kind, handle) values ('person', 'brad'), ('person', 'doipster'), ('agent', 'claude');

with family (name, blurb, params) as (values
    ('Value', 'Model + market blend, holds', '{"name":"Value","blurb":"Model + market blend, holds","shrink":0.5,"min_edge":0.03,"max_bets":1,"exit":"hold","tau_min":8,"tau_max":900,"band_min":0.05,"band_max":0.95}'::jsonb),
    ('Model', 'Trusts the model, adds bets', '{"name":"Model","blurb":"Trusts the model, adds bets","shrink":1,"min_edge":0.05,"max_bets":3,"exit":"hold","tau_min":8,"tau_max":900,"band_min":0.05,"band_max":0.95}'::jsonb),
    ('Late', 'Only bets the last 2.5 minutes', '{"name":"Late","blurb":"Only bets the last 2.5 minutes","shrink":1,"min_edge":0.03,"max_bets":2,"exit":"hold","tau_min":8,"tau_max":150,"band_min":0.05,"band_max":0.95}'::jsonb),
    ('Scalper', 'In and out, cuts losers early', '{"name":"Scalper","blurb":"In and out, cuts losers early","shrink":0.5,"min_edge":0.03,"max_bets":3,"exit":"ev","tau_min":25,"tau_max":900,"band_min":0.05,"band_max":0.95}'::jsonb),
    ('Favorite', 'Backs the favorite late', '{"name":"Favorite","blurb":"Backs the favorite late","shrink":1,"min_edge":0,"max_bets":1,"exit":"hold","tau_min":8,"tau_max":240,"band_min":0.62,"band_max":0.88}'::jsonb),
    ('Lottery', 'Cheap longshots after a vol spike', '{"name":"Lottery","blurb":"Cheap longshots after a vol spike","shrink":1,"min_edge":0,"max_bets":1,"exit":"hold","tau_min":180,"tau_max":900,"band_min":0,"band_max":0.15,"lottery":true}'::jsonb)
), s as (
    insert into strategy (family, name, description)
    select 'kalshi15m', name, blurb from family
    returning id, name
)
insert into strategy_version (strategy_id, version, params, code_ref, hypothesis, created_by, status)
select s.id, 1, f.params,
       'kalshi_trader.py@ed05fc2, ported in service/internal/kalshi15m',
       case f.name
           when 'Lottery' then 'Kept as a documented negative result: its early backtest win came from a look-ahead bug. Expected to lose.'
           else 'Carried over unchanged from the Python. The Python''s one-month backtest (one-minute resolution, final minute untested) showed a loss; treated as inconclusive.'
       end,
       (select id from actor where handle = 'doipster'),
       'probation'
  from s join family f using (name);

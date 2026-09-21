-- tools/dev-v3-plumbing.sql
--
-- DEV ONLY. NEVER A MIGRATION, NEVER ON PROD. Step S5 of docs/honest-fills-v3.md.
--
-- What it does: registers two DEV PLUMBING strategies of the third engine on the DEV database,
-- "Scalper (dev plumbing)" v3 and "Value (dev plumbing)" v3, with TEST VALUES in place of the four
-- numbers that must be measured (lambda, the two staleness costs, the drift tolerance), so that
-- the runner's plumbing can be exercised end to end before the measurement protocol has produced
-- anything: orders, partial fills, holds, settlements, the rebuild, the sweep. Their results mean
-- NOTHING and every order they place carries "plumbing": true in its detail.
--
-- WHY THEIR OWN STRATEGY NAMES, not Scalper v3 and Value v3 (decided at S5; plan 7.4 and 11/S7):
-- strategy_version is unique on (strategy_id, version), and migration 0013 will register the
-- REAL Scalper v3 and Value v3 at that very key. Plan 7.4 lets 0013 overwrite a row marked DEV
-- PLUMBING in place, but the plumbing BUCKET would survive that: EnsureSimSetup finds a bucket by
-- its name ("kalshi15m3 Scalper v3"), so the real version would inherit the plumbing bucket's
-- cash and history, and if S5's checklist ran it out (item 18: reaped and frozen, never replaced)
-- the real version could never trade on dev at all. Under names of their own the plumbing rows
-- and buckets never meet 0013: its insert does not conflict, its "on conflict ... where
-- hypothesis like 'DEV PLUMBING%'" branch simply never fires, and the real versions get fresh
-- buckets. What S7 must do on dev instead of relying on the overwrite: retire both plumbing
-- versions (see "To stop" below) BEFORE approving a real one, so that no placeholder number ever
-- trades beside a measured one. (The params' own "name" stays "Scalper" / "Value", as the Go
-- constructors make it; it is only a label in logs and events.)
--
-- WHY VALUE IS A DRAFT: so that S5's checklist can approve a version WHILE the service runs and
-- see the fixed-bucket-set rule (no new bucket until the restart) and the "approved, waiting for
-- restart" note on /api/status. After this script only Scalper (dev plumbing) orders.
--
-- Why it is safe to leave in the repo:
--   * the first statement raises unless current_database() ends in '_dev', and the whole script
--     is one transaction, so on any other database it changes nothing;
--   * even if these rows reached another database, the runner would refuse them: it builds a
--     plumbing engine only when the version carries the mark below AND the database's name ends
--     in '_dev' (runner3.go, Options3.vet). Everything else goes through kalshi15m3.NewEngine,
--     which refuses any number labelled "placeholder".
--
-- The mark is written twice on purpose: in hypothesis, where a person reads it, and in params
-- under "dev_plumbing", where the runner reads it (the store package has no call that reads a
-- hypothesis, and it is frozen for this step).
--
-- The params are what kalshi15m3.PlumbingScalper / PlumbingValue make of these TEST VALUES, none
-- of which is a measurement: lambda 0.5, stale_cost 0.007 (the plan's own illustrative figure),
-- stale_cost_sell 0.007, drift_tol 0.02. The test TestDevPlumbingScriptMatchesTheConstructors
-- (service/internal/runner) fails if the JSON below drifts from the Go constructors.
--
-- NOTE ON TRIALS: every strategy_version row counts as a trial in the analysis page's corrected
-- t (store/analysis.go counts the table). That is a second reason this never goes to prod, and
-- it means the dev analysis page reads two trials more than prod's after this has run.
--
-- Run:   psql -v ON_ERROR_STOP=1 -d assetcracker_dev -f tools/dev-v3-plumbing.sql
-- Then:  AC_V3=on and restart the DEV service. Safe to run twice: existing rows are left alone.
-- To approve Value (dev plumbing) later:
--        update strategy_version set status = 'probation' where version = 3 and strategy_id =
--          (select id from strategy where family = 'kalshi15m' and name = 'Value (dev plumbing)');
-- To stop a plumbing version trading: update its status to 'retired' and restart (its open bets
-- are still settled), or restart with AC_V3 off.
--
-- NOT RUN ANYWHERE when it was written (2026-09-21: no database on the machine it was written on).

begin;

do $$
begin
    if current_database() !~ '_dev$' then
        raise exception 'dev-v3-plumbing.sql REFUSES to run on database "%": it is for a *_dev database only', current_database();
    end if;
end
$$;

with plumbing (base, name, status, params) as (values
    ('Scalper', 'Scalper (dev plumbing)', 'probation',
-- BEGIN Scalper
     '{"band_max":0.95,"band_min":0.05,"blurb":"Trades often, banks small gains; fills only what the book displayed","dev_plumbing":"DEV PLUMBING, not a trial","drift_tol":0.02,"exhausted_cents":100,"exit":"ev","fee_per_fill":false,"kappa":0.25,"lambda":0.5,"levels":5,"max_bets":25,"min_gap":8,"min_hold":5,"min_tau":8,"name":"Scalper","protocol_sha":"","provenance":{"band_max":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":0.95},"band_min":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":0.05},"drift_tol":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: drift_tol","value":0.02},"exhausted_cents":{"kind":"inherited","note":"v2''s BankruptAt","value":100},"kappa":{"kind":"convention","note":"quarter Kelly: a risk preference, not measurable","value":0.25},"lambda":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: lambda","value":0.5},"levels":{"kind":"fact","note":"a snapshot records five bid levels per side","value":5},"max_bets":{"kind":"inherited","note":"unmeasured; taken unchanged from Scalper v2; counts buy orders with a fill","value":25},"min_gap":{"kind":"inherited","note":"unmeasured; taken unchanged from Scalper v2","value":8},"min_hold":{"kind":"inherited","note":"unmeasured; taken unchanged from Scalper v2","value":5},"min_tau":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":8},"seed_cents":{"kind":"convention","note":"the same $1,000 as v2, so the lines compare","value":100000},"stale_cost":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: stale_cost","value":0.007},"stale_cost_sell":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: stale_cost_sell","value":0.007},"take_capture":{"kind":"inherited","note":"unmeasured; taken unchanged from Scalper v2; the review found its effect unresolved","value":0.8},"tau_max":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":900},"tau_min":{"kind":"inherited","note":"unmeasured; taken unchanged from Scalper v2","value":25},"window_cap_bps":{"kind":"limit","note":"the owner''s limit; v2''s TotalCap, on min(E_w, seed)","value":2500}},"result_sha":"","seed_cents":100000,"stale_cost":0.007,"stale_cost_sell":0.007,"take_capture":0.8,"tau_max":900,"tau_min":25,"window_cap_bps":2500}'::jsonb),
    ('Value', 'Value (dev plumbing)', 'draft',
-- BEGIN Value
     '{"band_max":0.95,"band_min":0.05,"blurb":"Model and market blended at the measured weight; holds to settlement","dev_plumbing":"DEV PLUMBING, not a trial","drift_tol":0.02,"exhausted_cents":100,"exit":"hold","fee_per_fill":false,"kappa":0.25,"lambda":0.5,"levels":5,"max_bets":1,"min_gap":20,"min_hold":0,"min_tau":8,"name":"Value","protocol_sha":"","provenance":{"band_max":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":0.95},"band_min":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":0.05},"drift_tol":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: drift_tol","value":0.02},"exhausted_cents":{"kind":"inherited","note":"v2''s BankruptAt","value":100},"kappa":{"kind":"convention","note":"quarter Kelly: a risk preference, not measurable","value":0.25},"lambda":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: lambda","value":0.5},"levels":{"kind":"fact","note":"a snapshot records five bid levels per side","value":5},"max_bets":{"kind":"inherited","note":"unmeasured; taken unchanged from Value v2; counts buy orders with a fill","value":1},"min_gap":{"kind":"inherited","note":"unmeasured; taken unchanged from Value v2","value":20},"min_tau":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":8},"seed_cents":{"kind":"convention","note":"the same $1,000 as v2, so the lines compare","value":100000},"stale_cost":{"kind":"placeholder","note":"TEST VALUE for dev plumbing, not a measurement: stale_cost","value":0.007},"tau_max":{"kind":"inherited","note":"unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief","value":900},"tau_min":{"kind":"inherited","note":"unmeasured; taken unchanged from Value v2","value":8},"window_cap_bps":{"kind":"limit","note":"the owner''s limit; v2''s TotalCap, on min(E_w, seed)","value":2500}},"result_sha":"","seed_cents":100000,"stale_cost":0.007,"stale_cost_sell":0,"take_capture":0,"tau_max":900,"tau_min":8,"window_cap_bps":2500}'::jsonb)
), made as (
    insert into strategy (family, name, description)
    select 'kalshi15m', p.name, 'DEV PLUMBING, not a trial: tools/dev-v3-plumbing.sql, dev database only'
      from plumbing p
    on conflict (family, name) do nothing
    returning id, name
), s as ( -- a row made just now is not visible to a plain read in the same statement, hence the union
    select id, name from made
    union
    select st.id, st.name from strategy st where st.family = 'kalshi15m' and st.name in (select name from plumbing)
)
insert into strategy_version (strategy_id, version, params, code_ref, hypothesis, parent_version_id, created_by, status)
select s.id, 3, p.params,
       'service/internal/kalshi15m3, dev plumbing (tools/dev-v3-plumbing.sql)',
       'DEV PLUMBING, not a trial',
       (select v.id from strategy_version v join strategy b on b.id = v.strategy_id
         where b.family = 'kalshi15m' and b.name = p.base and v.version = 2),
       (select id from actor where handle = 'claude'),
       p.status
  from s join plumbing p using (name)
on conflict (strategy_id, version) do nothing;

-- What is there now, for the person running this to read.
select st.name, v.version, v.status, v.hypothesis, v.params->>'dev_plumbing' as mark,
       v.params->'provenance'->'lambda'->>'kind' as lambda_kind
  from strategy_version v join strategy st on st.id = v.strategy_id
 where st.family = 'kalshi15m' and v.version = 3
 order by st.name;  -- expect Scalper (dev plumbing) probation and Value (dev plumbing) draft

commit;

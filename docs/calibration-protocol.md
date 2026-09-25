# Calibrated belief protocol

**Status: in force.** Brad said yes on 2026-09-24, in a chat session with the agent that
committed this file, and chose **Fold** (below). On 2026-09-25, before TRAIN's first close, he
amended it to fix what the first text left open ("Conventions fixed before TRAIN"). Nothing in it
had been run, and no outcome had been read for it, when either text was committed.

**`T_c`** is the committer time (UTC) of the last commit that changed this file, printed by
`git log -1 --format='%H %cI' -- docs/calibration-protocol.md`. Any later commit to this file is a
new protocol with a new `T_c`. Mechanical slips are corrected under the rule of section 0.1 of
`docs/v3-measurement-protocol.md`, applied to this file, in the append-only
`docs/calibration-protocol-errata.md`.

## Why this exists

Two days of the third engine on the 15-minute rounds (2026-09-22, about 160 rounds of BTC and
ETH with the model's view journaled) said three things at once:

1. The market's mid price beat the model's probability at every horizon but the last two minutes
   (Brier score). A fixed blend `p = mid + λ (p_model − mid)` with λ = 0.5 is therefore worse
   than the market alone for most of the round.
2. The market itself is miscalibrated in a structured way: the favourite priced 0.70–0.90 with
   300–600 s to the close won more often than its price, longshots less, and the pattern
   reversed inside 300 s.
3. The engine cannot act on (2): its belief is the blend, and the mid is below the ask by half the
   spread, so no side is bought unless the model pushes the belief over the ask. Under that gate
   the favourite cell fired in a quarter of the rounds and kept none of its edge.

The remedy that follows from the data is not a better λ but a **fitted belief**: a calibrator
`p_cal = f(mid, p_model, τ, …)` estimated on one span of rounds, frozen, and judged on a later
span it never saw. This document says how that would be done so that the number that comes out
is a measurement and not a look.

## What is measured

A logistic calibrator of the yes outcome `y ∈ {0, 1}` of a round on the features of one
snapshot of that round:

```
logit(p_cal) = a + b · logit(mid) + c · logit(p_model) + Σ_k d_k · 1[τ ∈ bin_k]
             + e · logit(mid) · 1[τ < 120 s] + g · vol_ratio + h · spread + coin effects
```

**[CONVENTION]** τ bins: (600, 900], (300, 600], (120, 300], (60, 120], (0, 60]. Coin effects:
one intercept per coin. `spread` is `yes_ask − yes_bid`. The interaction term is there because
the model's only advantage was inside two minutes. Nothing else is added after the first
result; a richer form is a new protocol.

The fit is by maximum likelihood with an L2 penalty **[CONVENTION: λ_ridge = 1 on standardised
features]**, chosen before any data is read. Monotonicity in `mid` and `p_model` is checked, not
imposed: a fitted `b < 0` or `c < 0` is reported and the version is not registered.

## Data and the split

- Population: settled rounds of `KXBTC15M` and `KXETH15M` (and any 15-minute series the engine
  evaluates by then) with `result in ('yes','no')` and at least one evaluation row carrying
  `model->'v3'->>'p_model'`, closing after `T_c`.
- Observations: one per round per τ bin: the LAST evaluation row inside the bin with a two-sided
  book `0 < yes_bid < yes_ask < 1`. Rounds closing at the same instant across coins share the
  crypto move; standard errors are clustered by close time.
- **TRAIN**: the first 14 complete UTC days after `T_c`. **TEST**: the next 14 complete UTC days.
  **[CONVENTION: two weeks each; about 1,300 rounds per coin per span]** Nothing is fitted on
  TEST. Nothing from TEST is looked at until TRAIN's fit is committed.
- One look each. The tool writes `research/calibration/train-attempt.json` (the round ids it will
  use) before reading any outcome, fits, writes `train-result.json` (coefficients, in-sample log
  loss and Brier by bin, against the mid's), and commits. It refuses to run TEST until TRAIN's
  result is committed; then the same for TEST. A rerun only for a mechanical failure, error quoted.

## What counts as a result

On TEST, by τ bin and overall: Brier and log loss of `p_cal`, of `mid`, and of the frozen
λ = 0.5 blend. The calibrator is **useful** if its log loss beats the mid's overall and in at
least three of the five bins, with a paired bootstrap over close times **[CONVENTION: 2,000
resamples]** giving the improvement a 90% interval above zero. "No" is an accepted result and
registers nothing.

## Conventions fixed before TRAIN

**[OWNER, amendment of 2026-09-25]** The first text left these open. They are fixed here before
TRAIN's first close (2026-09-26 00:00 UTC), so that the tool writes them down and decides nothing.
Brad chose the estimator's scale, the strict reading of the pass rule, and how H15's row is
computed; the rest follows the precedent of `cmd/measure3` and was put to him before he chose.

- **Rows.** An observation is the LAST evaluation row inside its τ bin that has a two-sided book
  (`0 < yes_bid < yes_ask < 1`, read from the row's `quotes`) and a v3 model view
  (`(model->'v3'->>'ok')::boolean`, with `model->'v3'->>'p_model'` present). τ is
  `closes_at − at` in seconds, and a bin (lo, hi] holds lo < τ ≤ hi. The interaction's
  `1[τ < 120 s]` is read as written.
- **Features.** `mid = (yes_bid + yes_ask) / 2` and `spread = yes_ask − yes_bid`, from the row's
  `quotes`; `p_model` and `vol_ratio` are the view's journaled figures (`vol_ratio` is the model's
  volatility over the coin's default, `sqrt(sigma2) / default_sigma`). `logit(p_model)` holds
  `p_model` at least 1e-6 from 0 and 1, as the scorecard's log loss does **[CONVENTION]**.
- **Coding.** An intercept `a`; one indicator for each τ bin except (600, 900], and for each coin
  except BTC. Every column but the intercept is standardised with TRAIN's mean and population
  standard deviation, frozen, and applied unchanged to TEST; a column that is constant on TRAIN
  is left out and reported.
- **Estimator [OWNER].** The coefficients maximise `Σ log L − ½ · λ_ridge · Σ β²` over the
  standardised columns, with λ_ridge = 1, the log-likelihood summed over TRAIN's observations,
  and the intercept not penalised (scikit-learn's `LogisticRegression(C=1)`). Newton's method from
  zero, until no coefficient moves by more than 1e-10, within 100 iterations; a fit that does not
  converge is a mechanical failure. `b` and `c` are reported on the unstandardised scale. Standard
  errors are sandwich estimates clustered by close time, and are reported only.
- **The TEST figures.** Brier and log loss are means over observations, overall and by τ bin, for
  `p_cal`, `mid` and the blend `mid + 0.5 · (p_model − mid)`; log loss holds every probability at
  least 1e-6 from 0 and 1. The improvement is the mid's log loss less `p_cal`'s.
- **The pass rule, read strictly [OWNER].** Useful when (i) the overall improvement is above zero
  and the 5th percentile of its value over 2,000 paired bootstrap resamples is above zero, and
  (ii) in at least three of the five bins, the bin's improvement is above zero and the 5th
  percentile of its value over the same resamples is above zero. A resample draws close times with
  replacement, each drawn close bringing all of its observations, from seed 20260925
  **[CONVENTION]**; a bin's percentile is taken over the resamples that hold at least one of its
  observations.
- **H15's row [OWNER].** Computed by the long-shot protocol's own rules (its sections 2 and 3) on
  the rounds of its five series closing in TEST: both sides of the book; the FIRST row with
  `closes_at − 300 s ≤ at < closes_at − 240 s`; its eligibility and size rules;
  `r = (1 − y) − c − f` with its fee; a mean per window; its block jackknife, with 6-hour blocks
  of close time aligned to 00:00 UTC **[CONVENTION]**; "profitable" when `t ≥ t_{0.9875, B−1}`,
  else "not shown"; and its secondary figures beside it.
- **When a span runs.** No earlier than one hour after its last close **[CONVENTION]**; a round
  with no `yes` or `no` result by then is left out and listed.
- **The files.** Under `research/calibration/`, the tool commits `train-attempt.json` (the rounds
  and evaluation rows it will read) before it reads any outcome, then `train-result.json`; the
  same for TEST, which it refuses to run until TRAIN's result is committed and held by the remote.
  Every run, complete or not, is appended to `runs.jsonl`.

## How a yes becomes a strategy

A "yes" permits ONE version 3 per shape: the engine gains a belief source `belief: "calibrated"`
whose coefficients are stored in `params` with provenance kind `measured`, `protocol_sha` this
file, `result_sha` the committed `test-result.json`. The entry rule is unchanged: a side is bought
only where `p_cal` beats the ask after fee and staleness. Sizing, the window cap, the side and
time filters are the builder's as before. Such a version is a trial like every other and is
judged at the corrected threshold on the leaderboard.

## What a no means for the project

**[OWNER, 2026-09-24]** Chosen before TRAIN, so that a no ends the search instead of starting
another variant: if TEST says no, strategy work is parked. No new strategy version is registered
or deployed, and no engine feature for trading is built, until the owner decides otherwise in
writing. The recorders keep running.

## Relation to the long-shot protocol

`docs/longshot-protocol.md` (H15) asks whether buying the other side of a long shot priced at
10 cents or less, five minutes before the close, pays after the fee: an outcome-minus-price
statistic by price band. Fitting a calibrator on the same rounds computes that relation and
more. Two honest ways to hold both were drafted, and the owner chose the first (2026-09-24):

- **Fold** (chosen): H15's question becomes one row of the calibrator's TEST report (the
  (0.90, 1.00] mid band at (240, 300] s, outcome minus price after fee), computed once with the
  rest, and that row is the answer to H15. H15 is not run on its own sample (the first 480
  windows after the long-shot protocol's `T_c`): the look disclosed below saw that population.
  The long-shot protocol's other horizons (Hh, Hd, Hw, on the ladders) are untouched.
- **Wait** (not chosen): `T_c` of this protocol set no earlier than the commit of H15's result.

The draft said the answer to H15, and the disclosure below, would be entered in
`docs/longshot-protocol-errata.md`. That file admits names and syntax only (its own header, and
section 0.1 of `docs/v3-measurement-protocol.md`), so neither is entered there: both are recorded
here, and H15's answer is reported with this protocol's TEST result.

Disclosed: on 2026-09-22 (21:30–22:00 PT) outcome-minus-price by mid band and τ bin WAS computed
by hand on the H15 population after the long-shot protocol's `T_c`, in a chat session, to choose
a builder shape (Mid-round Favourite). That look is what motivated this document. It is recorded
here so that any result from this protocol or from H15 is read knowing it happened.

## What this protocol does not do

It does not change the third engine's model while `docs/v3-measurement-protocol.md` is still
measuring it (TRAIN closes 2026-09-27); `p_model` here is that model's output, read from the
journal. It reads no ladder outcome. It moves no money.

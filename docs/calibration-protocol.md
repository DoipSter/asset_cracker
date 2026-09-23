# Calibrated belief protocol (DRAFT: not in force)

**Status: a draft for the owner's yes.** Nothing in it has been run. No outcome has been read
for it. It becomes a protocol, with a `T_c`, only when Brad says yes and the file is committed
without this heading. Until then it is a description of what would be measured and how.

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

## How a yes becomes a strategy

A "yes" permits ONE version 3 per shape: the engine gains a belief source `belief: "calibrated"`
whose coefficients are stored in `params` with provenance kind `measured`, `protocol_sha` this
file, `result_sha` the committed `test-result.json`. The entry rule is unchanged: a side is bought
only where `p_cal` beats the ask after fee and staleness. Sizing, the window cap, the side and
time filters are the builder's as before. Such a version is a trial like every other and is
judged at the corrected threshold on the leaderboard.

## Relation to the long-shot protocol

`docs/longshot-protocol.md` (H15) asks whether buying the other side of a long shot priced at
10 cents or less, five minutes before the close, pays after the fee: an outcome-minus-price
statistic by price band. Fitting a calibrator on the same rounds computes that relation and
more. Two honest ways to hold both:

- **Fold** (recommended): H15's question becomes one row of the calibrator's TEST report (the
  (0.90, 1.00] mid band at (240, 300] s, outcome minus price after fee), computed once with the
  rest and reported in `docs/longshot-protocol-errata.md` as the answer to H15. The long-shot
  protocol's other horizons (Hh, Hd, Hw, on the ladders) are untouched.
- **Wait**: `T_c` of this protocol is set no earlier than the commit of H15's result.

Disclosed: on 2026-09-22 (21:30–22:00 PT) outcome-minus-price by mid band and τ bin WAS computed
by hand on the H15 population after the long-shot protocol's `T_c`, in a chat session, to choose
a builder shape (Mid-round Favourite). That look is what motivated this document. It is recorded
here so that any result from this protocol or from H15 is read knowing it happened; it is also
owed to the long-shot errata, pending the owner's yes.

## What this protocol does not do

It does not change the third engine's model while `docs/v3-measurement-protocol.md` is still
measuring it (TRAIN closes 2026-09-27); `p_model` here is that model's output, read from the
journal. It reads no ladder outcome. It moves no money.

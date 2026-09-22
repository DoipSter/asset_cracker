# Draft: third amendment to the v3 measurement protocol

**Status: a proposal, not protocol.** This file changes nothing. `docs/v3-measurement-protocol.md`
is unchanged and `T_c` has not moved. If you say yes, the text under "Amendment" is pasted at the
top of the protocol file, under the second amendment, and committed; that commit becomes the new
`T_c`, `cmd/measure3` has its embedded sha updated to the new text, and `engine.Params.Validate`
gains the one exception named below. Before TRAIN's 480th window closes (about 2026-09-27 04:15
UTC) this costs no TEST window (section 0). After it, section 8 applies and this is not possible.

## Why

`drift_tol` is the fourth number `Params.Validate` requires to be of kind `measured`. The plan
measured it in step S5: the 99th percentile of `|p_model(v3) - p_model(v2)|` on the scratch
database with both engines running (`docs/honest-fills-v3.md`, S5). Two things have happened since:

- The second engine is stopped (release `000fa9d`, 2026-09-22 04:17 UTC). The live `View` is
  built with no reference, so `drift` is always false and the gate is open; the protocol's second
  amendment already records this (section 2, "With no live v2, `drift` is always false").
- `assetcracker_dev` is scratch and the service refuses to start against it (2026-09-22). S5's
  measurement has no place left to run.

So the number cannot be measured, and while `Validate` demands it, `measure3 emit-migration`
refuses to write the version rows however R1 and R2 come out. That is the correct behaviour under
the protocol as written; the tool does exactly that today. What is missing is the owner's decision.

## Amendment (proposed text)

> **Third amendment, <date> UTC, at the owner's instruction.** `drift_tol` is not measured.
> The reference it was to be measured against, the second engine, stopped at release `000fa9d`,
> and the scratch database S5 named is no longer a place an engine runs. With no reference the
> drift gate is open (section 2) and the number decides nothing. `drift_tol` is therefore
> registered as kind `fact`, value `0`, note "no reference engine runs beside the third; the drift
> gate is open and the number decides nothing", and `engine.Params.Validate` accepts kind `fact`
> for that one field and no other. The three remaining numbers, `lambda`, `stale_cost` and
> `stale_cost_sell`, stay `measured` under sections 3 and 4. Should a reference engine run again,
> a measured `drift_tol` is a new version, not a change to this one.
>
> Also recorded: the queries of sections 1 and 3.1 and the coverage query have now been executed
> against the record, by `cmd/measure3 train` on 2026-09-22 07:10 UTC, on the 10 windows then
> complete; nothing was frozen (section 6). Section 9 item 5 is superseded to that extent.

## What changes in code, only after the yes

1. `service/internal/engine/params.go`: in `validate`, the measured-fields check lets
   `drift_tol` be kind `fact` (value 0) as well as `measured`. Nothing else loosens.
2. `service/cmd/measure3/repo.go`: `protocolSHA` becomes the sha-256 of the amended protocol file.
3. A test in `engine` that a version with `drift_tol` of kind `fact` and the other three
   `measured` validates, and one with `drift_tol` of kind `convention` or `inherited` does not.

## What does not change

The four-number rule for the other three; the split, the estimators, the thresholds; the
"register nothing" outcome; who approves a version (you, on the buckets page).

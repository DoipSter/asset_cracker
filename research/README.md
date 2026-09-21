# Research: a week of Kalshi crypto markets

Tooling that scrapes Kalshi's public API and asks what the data says about how these
markets price — **assuming manipulation is possible rather than assuming it away.**

Data: 20 Sep 2026, seven days back. 3,324 settled 15-minute rounds across five coins,
plus the hourly bracket and above/below markets. Nothing here needs an account or key.

## Running it

```bash
python scrape_week.py     # ~25 min, writes week_data.json (~8 MB)
python analyse_week.py    # volume, settlement margins, late-money test
python confound.py        # controls the headline finding for round closeness
```

Backtesting the trading engine itself, against a month of recorded rounds:

```bash
python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json   # ~20 min, resumable, ~2.6 MB
python backtest_exits.py                                # compares exit rules
python backtest_exits.py 672                            # just the last seven days
```

`backtest_stops.py` asks whether the Scalper should cut its losses, and concludes it should
not — with the caveat that one-minute bars cannot fairly test a price stop, which it measures
rather than asserts.

`backtest_exits.py` drives the real `kalshi_trader` rather than a reimplementation of it,
so what it measures is what the app does. It produced the numbers in the commit that added
`take_capture`; they can be reproduced by running it.

The `.json` files are gitignored. They are caches of public APIs, rebuildable with the
scripts above, not source.

## What the week showed

### Volume is overwhelmingly Bitcoin, and overwhelmingly late

| Coin | Rounds | Total volume | Median/round |
|---|---|---|---|
| BTC | 664 | 1,638,913,278 | 2,517,066 |
| ETH | 665 | 71,849,642 | 102,605 |
| XRP | 665 | 37,320,390 | 53,121 |
| SOL | 665 | 32,743,287 | 44,848 |
| DOGE | 665 | 17,601,414 | 24,010 |

BTC carries ~23x ETH's volume and ~93x DOGE's. Outcomes are balanced everywhere
(UP won 49.9–51.7%), so there's no systematic directional bias to exploit.

**38–48% of a round's entire volume trades in its final minute** — which is exactly the
minute the settlement index is averaged over. BTC 38.3%, DOGE 48.2%.

### The headline: heavy late volume goes with the market being confidently wrong

Split each round by how much of its volume arrived in the settlement minute, then score how
well the price *three minutes out* predicted the result (Brier score, lower is better):

| | Calm finish | Heavy finish |
|---|---|---|
| BTC | 0.0205 | 0.1806 |
| ETH | 0.0207 | 0.1401 |
| SOL | 0.0176 | 0.1446 |
| XRP | 0.0196 | 0.1182 |
| DOGE | 0.0194 | 0.1428 |

A 6–9x degradation. The obvious innocent explanation is reverse causation: close rounds are
both harder to call *and* attract more late money.

**`confound.py` rules that out.** Conditioning on what the price actually said three minutes
out, the gap is *largest* where the market looked most certain:

| Band (price 3 min out) | BTC calm | BTC heavy | Gap |
|---|---|---|---|
| clear 10–25¢ | 0.031 | 0.292 | **+0.260** |
| clear 75–90¢ | 0.049 | 0.231 | +0.182 |
| leaning 60–75¢ | 0.150 | 0.271 | +0.121 |
| toss-up 40–60¢ | 0.231 | 0.255 | +0.025 |

The same shape repeats on ETH (+0.223 at 75–90¢), SOL (+0.205 at 10–25¢) and
DOGE (+0.154 at 75–90¢). If late volume merely followed uncertainty, these would vanish.
Instead: rounds that looked *decided* three minutes out, then saw a volume surge, finished
close to a coin flip.

**What this does and does not show.** It cannot separate late *information* (real news or a
spot move arriving) from late *pressure* (flow pushing the index across the line). Both
produce this signature. A directional test on toss-up rounds found no consistent tilt
(BTC 45.2% vs 43.8% UP; SOL 30.8% vs 44.4%; DOGE 50.0% vs 60.0%) — small samples, no clean
one-sided story. Cells hold 40–68 rounds, so treat magnitudes loosely.

Either way the trading implication is the same: **a confident price is least trustworthy
exactly when the settlement minute is busy.**

### Settlement margins: weak, mostly unremarkable

How often the final index landed within 0.05% of the strike, against a crude uniform baseline:

| Coin | Median margin | <0.05% | Baseline |
|---|---|---|---|
| BTC | 0.0877% | 34.8% | ~28.5% |
| ETH | 0.1298% | 17.1% | ~19.3% |
| SOL | 0.1557% | 19.1% | ~16.1% |
| XRP | 0.1955% | 12.3% | ~12.8% |
| DOGE | 0.1625% | 17.1% | ~15.4% |

BTC shows mild clustering near the strike; the others sit at or below baseline. The baseline
is rough, so this is suggestive at best — not evidence of pinning.

### Hourly brackets are thin

`KXBTC` runs hourly bracket markets: 188 brackets per event, $100 wide. Only ~10 trade at
all in a typical hour, and the top 3 hold **80.7%** of that hour's volume. Total hourly
bracket volume over the week was 818,082 against 1.64 **billion** in the 15-minute rounds —
about 0.05%. `KXBTCD` (hourly above/below) is healthier at 46M.

So per-bracket hourly volume is real but sparse; a display of it will be mostly empty cells
with a few live brackets around spot.

## Files

| | |
|---|---|
| `scrape_week.py` | Pulls 15-min rounds (with per-minute volume), hourly brackets, above/below, and Coinbase spot |
| `analyse_week.py` | Volume distribution, settlement margins, the late-money test |
| `confound.py` | Conditions the late-money test on round closeness |
| `fetch_rounds.py` | Downloads a month of one coin's settled rounds with per-minute quotes |
| `backtest_exits.py` | Compares exit rules, driving the real engine — produced the `take_capture` numbers |
| `backtest_stops.py` | Asks whether the Scalper should cut its losses (it should not) |

The `.json` caches are gitignored — regenerate them with `scrape_week.py` and
`fetch_rounds.py`.

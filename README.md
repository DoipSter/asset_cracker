# Asset Cracker

A phone-shaped desktop widget for Kalshi's 15-minute crypto prediction markets — the ones
Coinbase Predictions runs on. Live prices, the round's "price to beat", and six paper-trading
strategies competing on real market data.

**Everything is simulated.** No account, no API key, no real orders. It reads public data and
keeps imaginary balances.

<img src="docs/screenshot.png" width="320" alt="Asset Cracker tracking Bitcoin">

## What it does

- **Two pages, one window.** Bitcoin and Ethereum, switched by the buttons at the top. Both keep
  streaming and trading in the background whichever page is showing.
- **Live prices** from Coinbase's WebSocket trade feed (not polling — every trade, as it happens).
- **The 15-minute round**: the price to beat, a countdown, and a chart of the round from open to
  close with a marker for every bet placed, at the price and moment it went in.
- **Two side panels**, one per coin, that slide out from either edge and can both be open at once.
  Each has an Account view, a bet Log, and a Strategies leaderboard.
- **Six strategies**, each with its own $150, trading the same live market so you can see which
  approach actually works.

## Does it make money?

No. Over a month of real market data (≈2,900 rounds per coin, Aug–Sep 2026), **every strategy
lost money.** That's the honest headline, and it's why this is a simulator rather than a bot.

What the data shows:

- **Kalshi's prices are well calibrated.** The side that's ahead wins about as often as its price
  implies — 77.9% of the time at an average price of 76.8¢, 95.3% at 95.2¢. There's nothing
  systematically cheap to buy.
- **Fees are the whole story.** Kalshi's fee (7% × price × (1−price) per contract) plus a cent of
  slippage costs roughly 5% of every stake. Beating that needs a real edge.
- **Longshots are overpriced**, not underpriced. Contracts under 5¢ win 1.7% of the time against a
  2.5¢ price; 5–10¢ contracts win 6.1% against 7.4¢. Buying them loses 22–37% of what you stake.
- **The market doesn't lag spot.** Correlation between a model-vs-market disagreement and the
  market's next move is +0.02 — effectively nothing.

The `Lottery` strategy is included deliberately as a **negative result**. It looked like a big
winner in an early backtest until that backtest turned out to have a look-ahead bug: the
volatility window included the next minute's price, so the "volatility spike" was partly detecting
the move that was about to happen. Corrected, it loses like the rest. It's left in, and labelled,
because a documented failed idea is worth more than a quietly deleted one.

## The strategies

| Name | Approach |
|---|---|
| `Value` | Blends the model with the market's own odds, one bet, holds to settlement |
| `Model` | Trusts the model more, up to 3 bets per round |
| `Late` | Only bets in the last 2.5 minutes |
| `Scalper` | Up to 3 bets, sells early when the odds turn against it |
| `Favorite` | Backs the favorite late, at 62–88¢ |
| `Lottery` | Cheap longshots after a volatility spike (see above — it loses) |

All of them respect the real rules: Kalshi's fee on every buy and sell, a cent of slippage,
the order book's actual depth, and one net position per market (you can't hold UP and DOWN at
once — buying the other side offsets what you own).

## Running it

```bash
python asset_cracker.py
```

Python 3.8+ on Windows 10/11. No pip packages — standard library and tkinter only.

`make_icon.py` regenerates the app icons and needs Pillow, but you only need it if you want to
change the artwork; the `.ico` files are committed.

## How it works

A few things that took measuring to get right:

**Coinbase's trade feed, not its ticker.** The app subscribes to the `matches` channel over a
hand-rolled WebSocket client (stdlib only). The `ticker` channel carries the same data but
arrives with up to ~150 ms of extra, uneven delay.

**Kalshi's order book, not its market list.** The `/markets` list and even the single-market
endpoint are CDN-cached for several seconds, so their prices go stale — they'd show 54/55 while
the real book had moved to 61. Live quotes come from `/markets/{ticker}/orderbook`, which is real
time. The list is used only to find which round is open.

**Predicting the next round's ticker.** After a round closes, Kalshi's list takes ~30 seconds to
show the next one, but the ticker is derivable from the close time, so the app asks for it
directly and picks it up ~5 seconds after close instead of ~34.

**Self-calibrating the index gap.** Kalshi settles on CF Benchmarks' index (BRTI), which is built
from several exchanges' order books and sits slightly above any single exchange's last trade —
and that gap drifts by a few dollars within minutes, so a fixed constant goes stale. Every settled
round is a free measurement: the settled value *is* the index averaged over that round's final
minute, so comparing it with the app's own average over the same minute gives the gap exactly.
It keeps a median of the last 24 rounds (~6 hours). Measured live, the gap was +0.019% when a
month-old constant said +0.006% — about $10 at $80k.

That last part is also why the headline price won't exactly match any exchange: it's an estimate
of the index, and roughly $8 of round-to-round noise is irreducible.

## Files

| File | |
|---|---|
| `asset_cracker.py` | The app: feeds, window, charts, side panels |
| `kalshi_trader.py` | The trading engine: strategies, fees, accounting, settlement |
| `make_icon.py` | Regenerates `btc.ico` / `eth.ico` (needs Pillow) |

Runtime state (`kalshi_balance*.json`, `kalshi_trades*.csv`) is gitignored — it's personal
results, and the app recreates it at $150 per strategy on first launch.

## Caveats

- Fills assume you get the displayed price plus a cent. On a thin book you'd do worse, so treat
  the balances as a mildly optimistic upper bound.
- Backtests ran at one-minute resolution (Kalshi's historical candles), so they can't test the
  final minute of a round.
- One month of data is one month. It isn't proof of anything either way.

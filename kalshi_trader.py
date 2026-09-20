"""Paper trading on Kalshi's 15-minute crypto markets ("BTC price up in next 15 mins?", and the
same for ETH), the markets Coinbase Predictions shows. Simulated money only. Each coin gets
its own KalshiTrader (its own accounts, files and calibration); the strategies are shared.

How those markets work:
  * Every 15 minutes a new market opens with a "price to beat" (the strike).
  * You can buy UP (Yes) or DOWN (No) contracts at the market's prices, 1 cent to
    99 cents each. A contract pays $1.00 if you're right and $0 if you're wrong.
    Buying at 40 cents pays back 1/0.40 = 2.5x if you win.
  * You can also sell a contract before the window ends, at the current bid.
  * The result is decided by the average of CF Benchmarks' BRTI index over the last
    minute before the window closes, compared with the strike.
  * Kalshi charges a fee of 7% x price x (1 - price) per contract, rounded up to the
    cent, on every buy and every sell. We also assume one cent of slippage per fill.

Because nobody knows in advance which approach works, six strategies each trade their
own $150 account at the same time, on the same live prices. Each may bet several times
in a window (even both sides, if its view flips) and some sell early to cut losses.
The leaderboard shows which is actually ahead.

Shared method: once a second, estimate the chance BTC finishes at or above the strike
(live price, time left, recent volatility), optionally blend it with the market's own
odds, and compare with the price of each side after fees. This is a model, not a
crystal ball: the market is competitive and every strategy can lose money.

Saved next to the app after every event:
  kalshi_balance.json   - every account, updated as things change
  kalshi_trades.csv     - one row per bet, sale and settlement
  kalshi_exits.csv      - one row per early sale, with what holding would have paid
  kalshi_rounds.csv     - one row per coin per round, including rounds nobody bet on

The last two exist to be argued with. The trade log says what happened; those two say
whether it should have. Between them they answer the questions worth asking: did selling
early cost us (gave_up), did we skip rounds we should have traded (bets == 0), and is the
index estimate drifting (index_gap_pct).
"""

import csv
import json
import math
import os
import statistics
import time
from collections import deque
from datetime import datetime

START_BALANCE = 150.0

FEE_RATE = 0.07  # Kalshi's taker fee: 7% x price x (1 - price) per contract, rounded up
SLIPPAGE = 0.01  # we pay one cent worse than the displayed price, buying or selling
KELLY = 0.25  # bet this fraction of the Kelly-optimal stake
MAX_STAKE = 0.20  # default share of cash for one bet; a strategy may override
WINDOW_CAP = 0.35  # ...or more than this share of the account into one window
# One balance now funds every coin, so five coins betting at once could commit five times
# WINDOW_CAP. This caps what can be at risk across all open bets, whatever the coin --
# as a share of the STARTING balance, not the current one. A winning run would otherwise
# keep raising the ceiling on its own stakes, so a bad round costs more the better things
# have been going. Half of $150 is $75 at risk, and it stays $75 at any balance.
TOTAL_CAP = 0.50
MIN_GAP = 20  # default seconds between bets in the same round; a strategy may override
MIN_HOLD = 15  # default seconds before a bet may be sold again
EXIT_MARGIN = 0.02  # an early sale must beat our estimate of the hold value by this
MIN_TAU = 8  # stop trading when this few seconds remain (orders take time)

# Kalshi settles on CF Benchmarks' BTC index (BRTI), not on Coinbase's trades. Measured over
# 664 settled rounds (Sep 13-20 2026): the index averaged +0.0057% above Coinbase (~$4.6 at
# $81k) and the gap swung by +/-0.0144% (~$11) round to round, with occasional $30-130 spikes.
INDEX_OFFSET_PCT = 0.000057  # the index sits this far above Coinbase, on average
INDEX_SD_PCT = 0.000144  # ...and the gap is uncertain by this much (one standard deviation)
DEFAULT_SIGMA = 8e-5  # BTC's typical volatility, per sqrt(second), until we measure it
LOG_KEPT = 500

# The "Lottery" strategy: buy the cheap side when a model that respects a recent volatility
# spike says that side is much likelier than its price, risking a tiny stake per bet and holding
# to settlement (usually a small loss, occasionally a ~20x win).
#
# IMPORTANT: this is here to be watched, NOT because it works. It first looked like a winner in
# my backtest, but that was a look-ahead bug (the volatility window included the next minute's
# price). Corrected, it LOSES over a month of data (Aug 20 - Sep 20 2026): about -75% to -99% on
# BTC and ETH at 2% stakes, and -48% to -52% at the 1% stake used here, even with no slippage.
# Cheap contracts are simply overpriced (the "longshot bias"): those under 5c win 1.7% of the
# time against a 2.5c price.
LOTTERY = dict(
    max_ask=0.15,  # only cheap sides: 15 cents or less
    min_mult=3.0,  # the model must say it's at least this many times likelier than the price
    min_spike=1.3,  # volatility over the last ~5 minutes vs the last 45 minutes
    min_tau=180,  # only with 3+ minutes left, so a reversal has time to happen
    stake=0.01,  # 1% of cash per bet (2% doubled the drawdowns in testing)
    fatten=1.15,  # widen the volatility a little: real tails are fatter than a bell curve
    slippage=0.005,  # cheap contracts tick in tenths of a cent, so less slippage than mid prices
)

STRATEGIES = [
    {"name": "Value", "blurb": "Model + market blend, holds",
     "shrink": 0.5, "min_edge": 0.03, "max_bets": 1, "exit": "hold",
     "tau": (MIN_TAU, 900), "band": (0.05, 0.95)},
    {"name": "Model", "blurb": "Trusts the model, adds bets",
     "shrink": 1.0, "min_edge": 0.05, "max_bets": 3, "exit": "hold",
     "tau": (MIN_TAU, 900), "band": (0.05, 0.95)},
    {"name": "Late", "blurb": "Only bets the last 2.5 minutes",
     "shrink": 1.0, "min_edge": 0.03, "max_bets": 2, "exit": "hold",
     "tau": (MIN_TAU, 150), "band": (0.05, 0.95)},
    # Deliberately the busy one: up to 25 open bets in a round, a new one every 8 seconds,
    # and contracts as cheap as 5c. Each bet is capped at 1.5% of cash rather than the usual
    # 20%, because otherwise WINDOW_CAP is reached in a handful of trades and the higher bet
    # count never gets used. Selling early frees a slot, so the round's total can pass 25.
    # The money at risk is unchanged: WINDOW_CAP and TOTAL_CAP still bound it.
    {"name": "Scalper", "blurb": "Trades often, banks small gains",
     "shrink": 0.5, "min_edge": 0.03, "max_bets": 25, "exit": "ev", "min_gap": 8,
     "max_stake": 0.015, "take_capture": 0.80, "min_hold": 5,
     "tau": (25, 900), "band": (0.05, 0.95)},
    {"name": "Favorite", "blurb": "Backs the favorite late",
     "shrink": 1.0, "min_edge": 0.0, "max_bets": 1, "exit": "hold",
     "tau": (MIN_TAU, 240), "band": (0.62, 0.88)},
    {"name": "Lottery", "blurb": "Cheap longshots after a vol spike", "kind": "lottery",
     "shrink": 1.0, "min_edge": 0.0, "max_bets": 1, "exit": "hold",
     "tau": (LOTTERY["min_tau"], 900), "band": (0.0, LOTTERY["max_ask"])},
]

CSV_FIELDS = ["time", "strategy", "coin", "event", "ticker", "side", "contracts", "price",
              "multiplier", "fee", "cost", "payout", "pnl", "result", "strike",
              "btc_price", "final_value", "balance_after", "model_prob", "edge",
              # added for tuning: why a sale happened, how much of the round was left, and
              # the book we actually saw, so a decision can be replayed from the row alone
              "why", "tau", "yes_bid", "yes_ask", "mkt_prob"]

# One row per early sale, written when that round finally settles, so it can say what
# holding would have paid. This is the file that says whether take_capture is set right.
EXIT_FIELDS = ["time", "strategy", "coin", "ticker", "side", "contracts", "why",
               "entry", "exit", "cost", "sold_for", "booked", "held_would_pay",
               "gave_up", "tau_at_exit", "result"]

# One row per coin per round, written at settlement -- including rounds nobody bet on,
# which is where the answer to "why didn't it trade?" lives.
ROUND_FIELDS = ["time", "coin", "ticker", "close", "strike", "final_value", "result",
                "last_price", "index_est", "index_gap_pct", "sigma_pct", "spread",
                "ticks", "bets", "scalper_bets", "scalper_pnl"]


def parse_amount(value):
    """A number from Kalshi, which sometimes arrives as a string with thousands separators
    ("79,604.96"). Returns None if it isn't a usable number."""
    if isinstance(value, (int, float)):
        return float(value)
    try:
        return float(str(value).replace(",", "").strip())
    except (TypeError, ValueError):
        return None


def coin_from_ticker(ticker):
    """"KXETH15M-26SEP..." -> "ETH". Used to place bets saved before coins were tracked."""
    head = (ticker or "").split("-")[0]
    if head.startswith("KX") and head.endswith("15M"):
        return head[2:-3] or None
    return None


def kalshi_fee(contracts, price):
    """Kalshi's taker fee in dollars, rounded up to the next cent."""
    return math.ceil(FEE_RATE * contracts * price * (1 - price) * 100 - 1e-9) / 100


def norm_cdf(x):
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def prob_yes(price, strike, tau, sigma2, known_avg=None, offset_pct=INDEX_OFFSET_PCT,
             sd_pct=INDEX_SD_PCT):
    """Chance the final settlement value (the average of the last 60 seconds) ends at
    or above the strike. `price` and `known_avg` are Coinbase prices; the strike is on
    Kalshi's index, which runs slightly higher, so both are lifted by INDEX_OFFSET_PCT.
    `tau` is seconds left; `known_avg` is the average of our own prices over the part of
    that final minute that has already happened."""
    lift = 1 + offset_pct
    if tau >= 60:
        # Walk to the start of the final minute, then average a 60-second walk.
        mean = price * lift
        var = sigma2 * price ** 2 * ((tau - 60) + 60 / 3)
    else:
        seen = (60 - tau) / 60  # how much of the averaging window is already known
        mean = (price if known_avg is None else seen * known_avg + (1 - seen) * price) * lift
        var = (tau / 60) ** 2 * sigma2 * price ** 2 * tau / 3
    sd = math.sqrt(var + (sd_pct * price) ** 2)  # our price vs the index: uncertain
    return min(0.999, max(0.001, norm_cdf((mean - strike) / sd)))


def _iso(ts):
    return datetime.fromtimestamp(ts).isoformat(timespec="seconds")


class Account:
    """One strategy's paper account."""

    def __init__(self, params):
        self.params = params
        self.name = params["name"]
        self.cash = START_BALANCE
        self.log = []  # every bet placed, oldest first; each carries its own status
        self.bets = self.wins = self.losses = 0
        self.realized_pnl = 0.0
        self.next_id = 1
        self.views = {}  # coin -> what the model thinks right now, for the display

    # ---- persistence ------------------------------------------------------

    def load(self, d):
        self.cash = float(d["cash"])
        self.log = list(d.get("log", []))
        self.bets = int(d.get("bets", 0))
        self.wins = int(d.get("wins", 0))
        self.losses = int(d.get("losses", 0))
        self.realized_pnl = float(d.get("realized_pnl", 0.0))
        self.next_id = 1 + max([lot["id"] for lot in self.log], default=0)
        for lot in self.log:  # bets saved before coins were tracked: infer from the ticker
            lot.setdefault("coin", coin_from_ticker(lot.get("ticker", "")))

    def to_dict(self, equity):
        return {
            "strategy": self.params["blurb"],
            "balance": round(equity, 2),
            "profit": round(equity - START_BALANCE, 2),
            "return_pct": round((equity / START_BALANCE - 1) * 100, 3),
            "cash": round(self.cash, 2),
            "realized_pnl": round(self.realized_pnl, 2),
            "bets": self.bets, "wins": self.wins, "losses": self.losses,
            "log": self.log[-LOG_KEPT:],
        }

    # ---- helpers ----------------------------------------------------------

    def open_lots(self, ticker=None):
        return [lot for lot in self.log
                if lot["status"] == "open" and (ticker is None or lot["ticker"] == ticker)]

    def committed(self):
        """What every open bet cost, across all coins. One balance funds them all."""
        return sum(lot["cost"] for lot in self.open_lots())

    def equity(self, markets):
        """Cash plus what open bets could be sold for right now, after the selling fee
        (or their cost, if there's no bid to price them by). `markets` maps coin -> quotes."""
        total = self.cash
        for lot in self.open_lots():
            m = (markets or {}).get(lot.get("coin"))
            bid = 0.0
            if m and m.get("ticker") == lot["ticker"]:
                bid = m["yes_bid"] if lot["side"] == "UP" else m["no_bid"]
            if bid > 0:
                total += lot["contracts"] * bid - kalshi_fee(lot["contracts"], bid)
            else:
                total += lot["cost"]
        return total

    # ---- deciding ---------------------------------------------------------

    def step(self, market, price, now, p_model, paused, ctx=None):
        """Look at the open window: maybe sell, maybe bet. Returns a list of events."""
        prm = self.params
        tau = market["close"] - now
        ya, na, yb = market["yes_ask"], market["no_ask"], market["yes_bid"]
        if not price or ya <= 0 or na <= 0:
            self.views[market.get("coin")] = None
            return []
        if prm.get("kind") == "lottery":
            return self._lottery(market, price, now, p_model, paused, ctx or {})

        mid = (yb + ya) / 2 if yb > 0 else ya
        p_up = mid + prm["shrink"] * (p_model - mid)  # blend with what the market believes
        options = []
        for side, ask, p_side, size in (("UP", ya, p_up, market["yes_ask_size"]),
                                        ("DOWN", na, 1 - p_up, market["no_ask_size"])):
            c = min(0.99, ask + SLIPPAGE)
            options.append({"side": side, "cost": c, "p": p_side, "size": size,
                            "edge": p_side - c - FEE_RATE * c * (1 - c)})
        events = []
        if not paused and tau >= MIN_TAU and prm["exit"] == "ev":
            events += self._exits(market, now, p_up, price)
        # Kalshi keeps one net position per market: you can't hold UP and DOWN at once
        # (buying the other side just offsets what you own). So once a strategy holds a
        # side in this round it may only add to that side, or sell it if it's allowed to.
        held = {lot["side"] for lot in self.open_lots(market["ticker"])}
        best = max((o for o in options if not held or o["side"] in held),
                   key=lambda o: o["edge"])
        self.views[market.get("coin")] = {"p_up": p_up, "p_model": p_model, "mid": mid,
                                          "best": best, "tau": tau,
                     "signal": self._signal(market, now, best, tau, paused)}
        if paused or tau < MIN_TAU:
            return events
        return events + self._entries(market, now, price, best, tau)

    def _lottery(self, market, price, now, p_model, paused, ctx):
        """Buy the cheap side when it's much likelier than its price and volatility just
        spiked. One small bet per round, held to settlement. `ctx` carries the volatility-
        spike-aware chance of UP (p_tail) and the spike size, measured by the trader."""
        L = LOTTERY
        tau = market["close"] - now
        ya, na, yb = market["yes_ask"], market["no_ask"], market["yes_bid"]
        p_tail, spike = ctx.get("p_tail"), ctx.get("spike")
        p_up = p_model if p_tail is None else p_tail
        options = []
        for side, ask, p_side, size in (("UP", ya, p_up, market["yes_ask_size"]),
                                        ("DOWN", na, 1 - p_up, market["no_ask_size"])):
            c = min(0.999, ask + L["slippage"])
            options.append({"side": side, "ask": ask, "cost": c, "p": p_side, "size": size,
                            "mult": p_side / ask,
                            "edge": p_side - c - FEE_RATE * c * (1 - c)})
        cheap = min(options, key=lambda o: o["ask"])  # the side that's the longshot
        already = any(lot["ticker"] == market["ticker"] for lot in self.log)

        why = None
        if paused:
            why = "paused"
        elif p_tail is None:
            why = "collecting volatility history"
        elif tau < L["min_tau"]:
            why = f"only bets with {L['min_tau'] // 60}+ min left"
        elif cheap["ask"] > L["max_ask"]:
            why = f"no cheap side (needs {L['max_ask'] * 100:.0f}¢ or less)"
        elif already:
            why = "already bet this round"
        elif spike < L["min_spike"]:
            why = f"volatility calm ({spike:.1f}x, needs {L['min_spike']:.1f}x)"
        elif cheap["mult"] < L["min_mult"]:
            why = f"only {cheap['mult']:.1f}x likelier (needs {L['min_mult']:.0f}x)"
        mid = (yb + ya) / 2 if yb > 0 else ya
        self.views[market.get("coin")] = {"p_up": p_up, "p_model": p_model, "mid": mid,
                                          "best": cheap, "tau": tau,
                     "signal": {"side": cheap["side"], "conf": cheap["p"] * 100,
                                "edge": cheap["edge"] * 100, "need": 0.0, "bet": why is None,
                                "why": why or "will bet"}}
        if why is not None or tau < MIN_TAU:
            return []

        unit = cheap["cost"] + FEE_RATE * cheap["cost"] * (1 - cheap["cost"])
        n = min(int(L["stake"] * self.cash // unit), int(cheap["size"]))
        while n > 0 and n * cheap["cost"] + kalshi_fee(n, cheap["cost"]) > self.cash:
            n -= 1
        if n < 1:
            return []
        return [self._bet(market, cheap, n, price, now)]

    def _signal(self, market, now, best, tau, paused):
        """Would this strategy place a bet right now, and how confident is it? Mirrors
        the checks in _entries, so what the display says is what the bot will do."""
        prm = self.params
        here = self.open_lots(market["ticker"])
        conf = best["p"] * 100  # its chance of being right on the side it likes best
        sig = {"side": best["side"], "conf": conf, "edge": best["edge"] * 100,
               "need": prm["min_edge"] * 100, "bet": False}

        def clock(sec):
            return f"{int(sec) // 60}:{int(sec) % 60:02d}"

        if paused:
            sig["why"] = "paused"
        elif tau < MIN_TAU or tau < prm["tau"][0]:
            sig["why"] = "too late this round"
        elif tau > prm["tau"][1]:
            sig["why"] = f"waits for last {clock(prm['tau'][1])}"
        elif len(here) >= prm["max_bets"]:
            sig["why"] = "max bets this round"
        elif here and now - max(lot["t"] for lot in here) < MIN_GAP:
            sig["why"] = "cooling down"
        elif not prm["band"][0] <= best["cost"] <= prm["band"][1]:
            sig["why"] = f"{best['cost'] * 100:.0f}¢ outside its range"
        elif best["edge"] < prm["min_edge"]:
            sig["why"] = f"edge {best['edge'] * 100:+.1f}¢ of {prm['min_edge'] * 100:.0f}¢ needed"
        else:
            sig["bet"] = True
            sig["why"] = "will bet"
        return sig

    def participated(self):
        """How many 15-minute rounds this strategy has bet in. Betting on three coins in
        the same quarter hour is one round, not three -- they all settle together."""
        return len({lot["close"] for lot in self.log})

    def _entries(self, market, now, price, best, tau):
        prm = self.params
        here = self.open_lots(market["ticker"])
        c = best["cost"]
        if (not prm["tau"][0] <= tau <= prm["tau"][1]
                or best["edge"] < prm["min_edge"]
                or not prm["band"][0] <= c <= prm["band"][1]
                or len(here) >= prm["max_bets"]
                or (here and now - max(lot["t"] for lot in here) < prm.get("min_gap", MIN_GAP))):
            return []
        committed = sum(lot["cost"] for lot in here)
        room = WINDOW_CAP * (self.cash + committed) - committed
        # ...and with every coin drawing on the same balance, cap the overall exposure at a
        # fixed number of dollars rather than a share of a balance that grows as it wins
        at_risk = self.committed()
        room = min(room, TOTAL_CAP * START_BALANCE - at_risk)
        unit = c + FEE_RATE * c * (1 - c)  # cost of one contract, fee included
        kelly = (best["p"] - unit) / (1 - unit)
        cap = prm.get("max_stake", MAX_STAKE)
        stake = min(min(KELLY * kelly, cap) * self.cash, room)
        n = min(int(stake // unit), int(best["size"]))
        while n > 0 and n * c + kalshi_fee(n, c) > self.cash:
            n -= 1
        if n < 1:
            return []
        return [self._bet(market, best, n, price, now)]

    def _bet(self, market, option, n, price, now):
        c = option["cost"]
        fee = kalshi_fee(n, c)
        cost = round(n * c + fee, 2)
        self.cash -= cost
        lot = {"id": self.next_id, "t": now, "time": _iso(now), "ticker": market["ticker"],
               "coin": market.get("coin"),
               "side": option["side"], "contracts": n, "price": round(c, 2),
               "multiplier": round(1 / c, 2), "fee": fee, "cost": cost,
               "strike": market["strike"], "close": market["close"], "btc_price": price,
               "model_prob": round(option["p"], 3), "edge": round(option["edge"], 3),
               "status": "open"}
        self.next_id += 1
        self.bets += 1
        self.log.append(lot)
        return dict(lot, kind="bet", strategy=self.name)

    def _exits(self, market, now, p_up, price):
        """Sell early, for either of two reasons: the market is paying more than we think
        the bet is worth, or the position is simply up enough to bank. The second is what
        scalping mostly is, and a strategy opts into it with take_profit."""
        prm = self.params
        capture = prm.get("take_capture")  # None: only sell when the market overpays
        hold = prm.get("min_hold", MIN_HOLD)
        events = []
        for lot in self.open_lots(market["ticker"]):
            bid = market["yes_bid"] if lot["side"] == "UP" else market["no_bid"]
            if bid <= 0 or now - lot["t"] < hold:
                continue
            p_side = p_up if lot["side"] == "UP" else 1 - p_up
            sell_c = max(0.01, bid - SLIPPAGE)
            n = lot["contracts"]
            fee = kalshi_fee(n, sell_c)
            proceeds = round(n * sell_c - fee, 2)
            # what we would net now against what the model says holding is worth
            overpriced = sell_c - FEE_RATE * sell_c * (1 - sell_c) > p_side + EXIT_MARGIN
            # Banking a gain is measured against the upside, not as a flat percentage: a 7c
            # contract pays 100c, so "up 12%" is under a cent and throws away the other 14x.
            # Sell once the bid has covered this much of the way from entry to a dollar, and
            # only if proceeds genuinely beat cost -- both fees and slippage are already in
            # those two numbers, so this can never bank a loss.
            banking = (capture is not None and proceeds > lot["cost"]
                       and sell_c >= lot["price"] + capture * (1 - lot["price"]))
            if overpriced or banking:
                self._close(lot, "sold", proceeds, now, exit_price=round(sell_c, 2),
                            exit_btc=price,
                            # kept on the lot, not just the event: at settlement the exits
                            # log looks back at this sale and needs to know what drove it
                            why="capture" if banking and not overpriced else "value",
                            exit_tau=round(market["close"] - now))
                events.append(dict(lot, kind="sold", strategy=self.name, payout=proceeds))
        return events

    def _close(self, lot, status, payout, now, **extra):
        lot.update(status=status, payout=payout, pnl=round(payout - lot["cost"], 2),
                   exit_t=now, **extra)
        self.cash += payout
        self.realized_pnl += lot["pnl"]
        self.wins += lot["pnl"] > 0
        self.losses += lot["pnl"] <= 0

    def on_settled(self, ticker, result, final_value, now, price):
        events = []
        for lot in self.open_lots(ticker):
            won = (result == "yes") == (lot["side"] == "UP")
            self._close(lot, "won" if won else "lost", float(lot["contracts"]) if won else 0.0,
                        now, result=result, final_value=final_value, exit_btc=price)
            events.append(dict(lot, kind="settled", strategy=self.name, won=won))
        return events

class CoinState:
    """Everything specific to one coin: how its price behaves, how far Kalshi's settlement
    index sits above our exchange price, and the round currently open. No money lives here --
    the strategy accounts share a single balance across every coin."""

    def __init__(self, coin, offset_pct=INDEX_OFFSET_PCT, sd_pct=INDEX_SD_PCT,
                 default_sigma=DEFAULT_SIGMA, decimals=2):
        self.coin = coin
        self.base_offset_pct, self.sd_pct, self.default_sigma = offset_pct, sd_pct, default_sigma
        # How many decimals this coin's price is quoted to. DOGE moves in the sixth, so a
        # log rounded to a fixed four would throw away the whole move.
        self.decimals = decimals
        # (time, measured offset) for recent rounds: 24 rounds is about six hours
        self.offset_obs = deque(maxlen=24)
        self.rounds_monitored = 0  # 15-minute rounds watched for this coin
        self.round_ticker = None  # so each round is counted once
        self.reset_signals()

    def reset_signals(self):
        self.sigma2 = self.default_sigma ** 2
        self._ring = deque(maxlen=150)  # (second, price), for the settlement average
        self._vol_ref = None
        self._min_closes = deque(maxlen=60)  # the price at the end of each finished minute
        self._min_last = None  # (minute number, latest price in it), the minute in progress
        self.market = None  # the latest quotes for the open round
        self.price = None  # the latest exchange price
        self.hourly = None  # the hourly bracket market, when the feed supplies it

    # ---- how far the index sits above our exchange price -------------------

    @property
    def offset_pct(self):
        """Kalshi settles on an index built from several exchanges' order books, which sits a
        little above our exchange's last trade -- and that gap drifts within minutes, so a
        fixed number goes stale. Every settled round is a free measurement (see
        note_settlement); the median of the recent ones shrugs off the odd outlier."""
        cutoff = time.time() - 6 * 3600
        recent = [o for t, o in self.offset_obs if t >= cutoff]
        return statistics.median(recent) if len(recent) >= 3 else self.base_offset_pct

    def offset_status(self):
        """(offset now, how many recent measurements it rests on)."""
        cutoff = time.time() - 6 * 3600
        return self.offset_pct, sum(1 for t, _ in self.offset_obs if t >= cutoff)

    def add_offset(self, when, offset):
        if abs(offset) <= 0.002:  # anything wilder than 0.2% is bad data, not a real gap
            self.offset_obs.append((when, round(offset, 8)))
            return True
        return False

    def note_settlement(self, close, final_value):
        """Kalshi's settled value IS the index averaged over that round's final minute, so
        comparing it with our own average over the same minute measures the gap exactly."""
        index_avg = parse_amount(final_value)
        if index_avg is None:
            return False
        ours = [p for s, p in self._ring if close - 60 <= s < close]
        if len(ours) < 40 or index_avg <= 0:  # too few ticks that minute to average fairly
            return False
        return self.add_offset(close, index_avg / (sum(ours) / len(ours)) - 1)

    # ---- price history and volatility --------------------------------------

    @staticmethod
    def _clamp(sigma2):
        return min(max(sigma2, (2e-5) ** 2), (3e-4) ** 2)

    @staticmethod
    def _var(closes):
        """Per-second variance of the price, from a run of one-minute closes."""
        rets = [math.log(b / a) for a, b in zip(closes, closes[1:]) if a > 0 and b > 0]
        return sum(r * r for r in rets) / len(rets) / 60 if rets else None

    def seed_vol(self, closes):
        """Start from real recent volatility using 1-minute closes (oldest first)."""
        rets = [math.log(b / a) for a, b in zip(closes, closes[1:]) if a > 0 and b > 0]
        if len(rets) >= 5:
            self.sigma2 = self._clamp(sum(r * r for r in rets) / len(rets) / 60)
        self._min_closes.extendleft(reversed(closes))  # older minutes go in front

    def observe(self, price, ts):
        """Feed every live price. Keeps a per-second record and updates volatility."""
        self.price = price
        sec = int(ts)
        minute = sec // 60  # keep the last price of each minute, for the volatility windows
        if self._min_last is not None and minute != self._min_last[0]:
            self._min_closes.append(self._min_last[1])
        self._min_last = (minute, price)
        if self._ring and self._ring[-1][0] == sec:
            self._ring[-1] = (sec, price)
        else:
            self._ring.append((sec, price))
        if self._vol_ref is None:
            self._vol_ref = (sec, price)
        elif sec - self._vol_ref[0] >= 5:  # sample every 5s to dodge bid/ask bounce
            dt = sec - self._vol_ref[0]
            inst = math.log(price / self._vol_ref[1]) ** 2 / dt
            weight = 1 - 0.5 ** (dt / 300)  # ~5 minute half-life
            self.sigma2 = self._clamp(self.sigma2 + weight * (inst - self.sigma2))
            self._vol_ref = (sec, price)

    def known_avg(self, close):
        vals = [p for s, p in self._ring if s >= close - 60]
        return sum(vals) / len(vals) if vals else None

    def late_volume_share(self):
        """How much of the open round's volume has landed in its final minute. A week of data
        says a confident price is least trustworthy when this runs high -- see research/.
        None until the feed supplies per-minute volume."""
        m = self.market or {}
        bars = m.get("bars")
        if not bars:
            return None
        total = sum(b.get("volume", 0) for b in bars)
        if total <= 0:
            return None
        late = sum(b.get("volume", 0) for b in bars if m["close"] - b.get("t", 0) <= 60)
        return late / total

    def tail_context(self, market, price, tau):
        """What the Lottery strategy needs: how much volatility has just spiked (last ~5
        minutes vs the last 45), and the chance of UP from a model that respects the spike."""
        closes = list(self._min_closes)
        c45, c6 = closes[-46:], closes[-7:]
        if len(c45) < 15 or len(c6) < 4:
            return {"p_tail": None, "spike": None}  # not enough history yet
        s45, s5 = self._var(c45), self._var(c6)
        if not s45 or s5 is None:
            return {"p_tail": None, "spike": None}
        s45 = self._clamp(s45)
        s2 = max(s45, s5) * LOTTERY["fatten"] ** 2  # widen the tails a little
        p_tail = prob_yes(price, market["strike"], tau, s2,
                          self.known_avg(market["close"]) if tau < 60 else None,
                          self.offset_pct, self.sd_pct)
        return {"p_tail": p_tail, "spike": math.sqrt(s5 / s45)}


class KalshiTrader:
    """The strategy accounts -- one balance each, shared across every coin -- plus a CoinState
    per coin holding that coin's prices, volatility and index calibration.

    A strategy that bets on Bitcoin and Ethereum in the same quarter hour spends the same
    balance twice, so TOTAL_CAP limits how much of an account can be committed at once."""

    def __init__(self, folder, coins):
        """`coins` maps a coin name to its calibration, e.g.
        {"BTC": {"index_offset_pct": ..., "index_sd_pct": ..., "default_sigma": ...}, ...}"""
        self.json_path = os.path.join(folder, "kalshi_balance.json")
        self.csv_path = os.path.join(folder, "kalshi_trades.csv")
        self.exit_path = os.path.join(folder, "kalshi_exits.csv")
        self.round_path = os.path.join(folder, "kalshi_rounds.csv")
        # What each live round looked like while it ran, so the round log can be written
        # when it settles -- including the rounds no strategy touched.
        self._rounds = {}
        self.coins = {
            name: CoinState(name, cfg.get("index_offset_pct", INDEX_OFFSET_PCT),
                            cfg.get("index_sd_pct", INDEX_SD_PCT),
                            cfg.get("default_sigma", DEFAULT_SIGMA),
                            cfg.get("decimals", 2))
            for name, cfg in coins.items()
        }
        self.accounts = {p["name"]: Account(p) for p in STRATEGIES}
        self.selected = STRATEGIES[0]["name"]
        self.selected_coin = next(iter(self.coins))
        self.paused = False
        self.started_at = time.time()
        # A round is one 15-minute window across every coin, so it is counted once no
        # matter how many coins are trading it.
        self.rounds_monitored = 0
        self._round_close = None
        self._load()
        self._last_save = 0.0

    # ---- lookups ----------------------------------------------------------

    def coin(self, name=None):
        return self.coins[name or self.selected_coin]

    def account(self, name=None):
        return self.accounts[name or self.selected]

    def markets(self):
        """Every coin's live quotes, keyed by coin, for valuing open bets."""
        return {name: c.market for name, c in self.coins.items() if c.market}

    def coin_rounds(self):
        """Rounds watched per coin, which is what each CoinState counts."""
        return {name: c.rounds_monitored for name, c in self.coins.items()}

    def select(self, name):
        if name in self.accounts:
            self.selected = name
            self.save(force=True)

    def select_coin(self, name):
        if name in self.coins:
            self.selected_coin = name
            self.save(force=True)

    def pending_tickers(self, coin=None):
        """Open bets still awaiting settlement, optionally just one coin's."""
        seen = {}
        for acct in self.accounts.values():
            for lot in acct.open_lots():
                if coin is None or lot.get("coin") == coin:
                    seen[lot["ticker"]] = lot["close"]
        return list(seen.items())

    # ---- per-coin work, delegated to that coin's state ---------------------

    def seed_vol(self, coin, closes):
        self.coins[coin].seed_vol(closes)

    def observe(self, coin, price, ts):
        self.coins[coin].observe(price, ts)

    def note_settlement(self, coin, close, final_value):
        if self.coins[coin].note_settlement(close, final_value):
            self.save(force=True)

    def seed_offsets(self, coin, measurements):
        """Prime the index-gap estimate from rounds that settled before we started."""
        c = self.coins[coin]
        for when, offset in sorted(measurements):
            c.add_offset(when, offset)
        self.save(force=True)

    def set_hourly(self, coin, brackets):
        """The hourly bracket market for this coin, as the side panel shows it."""
        self.coins[coin].hourly = brackets

    # ---- trading ----------------------------------------------------------

    def step(self, coin, market, price, now):
        """Give every strategy a look at this coin's open round. The accounts are shared, so
        a bet here spends the same balance as a bet on any other coin."""
        c = self.coins[coin]
        c.market = dict(market, coin=coin)
        if not price:
            return []
        if market["ticker"] != c.round_ticker:  # this coin moved to a new round
            c.round_ticker = market["ticker"]
            c.rounds_monitored += 1
        if market["close"] != self._round_close:  # a new 15-minute window, for all coins
            self._round_close = market["close"]
            self.rounds_monitored += 1
        self._note_round(coin, c, market, price, now)
        tau = market["close"] - now
        p_model = prob_yes(price, market["strike"], tau, c.sigma2,
                           c.known_avg(market["close"]) if tau < 60 else None,
                           c.offset_pct, c.sd_pct)
        ctx = c.tail_context(market, price, tau)
        events = []
        for acct in self.accounts.values():
            events += acct.step(c.market, price, now, p_model, self.paused, ctx)
        return self._record(events, now)

    def on_settled(self, ticker, result, final_value, now, price):
        """A round closed and Kalshi reported the real result. Pay out or write off.
        Tickers are unique across coins, so the ticker alone identifies the bets."""
        if result not in ("yes", "no"):
            return []
        events = []
        for acct in self.accounts.values():
            events += acct.on_settled(ticker, result, final_value, now, price)
        # Only now do we know what the round was worth, so this is the moment to grade the
        # early sales and to write down the round itself.
        self._log_exits(ticker, result, now)
        self._log_round(ticker, result, final_value, now)
        return self._record(events, now)

    # ---- the debugging trail ----------------------------------------------

    def _note_round(self, coin, c, market, price, now):
        """Keep the shape of each live round so it can be written down at settlement."""
        r = self._rounds.get(market["ticker"])
        if r is None:
            if len(self._rounds) > 200:  # rounds we never saw settle, e.g. app restarted
                cutoff = market["close"] - 7200
                self._rounds = {k: v for k, v in self._rounds.items() if v["close"] > cutoff}
            r = self._rounds[market["ticker"]] = {"coin": coin, "close": market["close"],
                                                  "strike": market["strike"], "ticks": 0,
                                                  "yes_bid": market["yes_bid"],
                                                  "yes_ask": market["yes_ask"]}
        r["ticks"] += 1
        # The price we want is the last one, because the index gap is measured at close.
        # The book is the opposite: in the final seconds the asks vanish and the bid runs to
        # a dollar, so a spread taken from the closing book is nonsense. Freeze it a minute
        # out, while the book is still a real two-sided market.
        r.update(last_price=price, sigma2=c.sigma2, offset_pct=c.offset_pct)
        if market["close"] - now > 60:
            r.update(yes_bid=market["yes_bid"], yes_ask=market["yes_ask"])

    def _log_exits(self, ticker, result, now):
        """Grade every early sale in this round against holding it to the end. `gave_up` is
        the whole point: positive means selling cost us, negative means it saved us."""
        for acct in self.accounts.values():
            for lot in acct.log:
                if (lot["ticker"] != ticker or lot["status"] != "sold"
                        or lot.get("graded")):
                    continue
                lot["graded"] = True  # survives a save/reload, so it is never graded twice
                won = (result == "yes") == (lot["side"] == "UP")
                held = float(lot["contracts"]) if won else 0.0
                self._append(self.exit_path, EXIT_FIELDS, {
                    "time": _iso(now), "strategy": acct.name, "coin": lot.get("coin", ""),
                    "ticker": ticker, "side": lot["side"], "contracts": lot["contracts"],
                    "why": lot.get("why", ""), "entry": lot["price"],
                    "exit": lot.get("exit_price", ""), "cost": lot["cost"],
                    "sold_for": lot["payout"], "booked": lot["pnl"],
                    "held_would_pay": round(held, 2),
                    "gave_up": round(held - lot["payout"], 2),
                    "tau_at_exit": lot.get("exit_tau", ""), "result": result,
                })

    def _log_round(self, ticker, result, final_value, now):
        """One row per round, whether or not anyone bet. A round with bets == 0 is a round
        the model passed on, and those are as worth reading as the ones it traded."""
        r = self._rounds.pop(ticker, None)
        if r is None:
            return
        # Kalshi settles on its own index; ours is the exchange price lifted by the offset
        # we have calibrated. Comparing the two each round is how we see the offset drift.
        est = r["last_price"] * (1 + r["offset_pct"])
        final = parse_amount(final_value)
        dec = self.coins[r["coin"]].decimals if r["coin"] in self.coins else 2
        bets = [l for a in self.accounts.values() for l in a.log if l["ticker"] == ticker]
        scalp = [l for l in self.accounts["Scalper"].log if l["ticker"] == ticker]
        self._append(self.round_path, ROUND_FIELDS, {
            "time": _iso(now), "coin": r["coin"], "ticker": ticker, "close": _iso(r["close"]),
            "strike": r["strike"], "final_value": final if final is not None else "",
            "result": result, "last_price": round(r["last_price"], dec),
            # two digits finer than the coin is quoted to: the gap we are measuring is
            # around 0.01%, which for DOGE lands past the sixth decimal
            "index_est": round(est, dec + 2),
            "index_gap_pct": round((final / est - 1) * 100, 5) if final and est else "",
            # the move a full 15-minute round is expected to make, as a percentage
            "sigma_pct": round(math.sqrt(max(0.0, r["sigma2"]) * 900) * 100, 4),
            "spread": round(r["yes_ask"] - r["yes_bid"], 3),
            "ticks": r["ticks"], "bets": len(bets), "scalper_bets": len(scalp),
            "scalper_pnl": round(sum(l.get("pnl", 0.0) or 0.0 for l in scalp), 2),
        })

    def _append(self, path, fields, row):
        """Append one row, writing the header for a new file. If an older file has a
        different header -- because these columns grew -- move it aside rather than write
        rows that no longer line up with it."""
        try:
            if os.path.exists(path):
                with open(path, newline="") as f:
                    head = (f.readline() or "").strip()
                if head and head.split(",") != fields:
                    os.replace(path, f"{os.path.splitext(path)[0]}"
                                     f"_cols_{datetime.now():%Y%m%d_%H%M%S}.csv")
            is_new = not os.path.exists(path)
            with open(path, "a", newline="") as f:
                w = csv.DictWriter(f, fieldnames=fields)
                if is_new:
                    w.writeheader()
                w.writerow(row)
        except OSError:
            pass  # a locked file (say, open in Excel) shouldn't stop the simulation

    def _record(self, events, now):
        for e in events:
            self._append_csv(e, now)
        if events:
            self.save(force=True)
        return events

    # ---- reporting --------------------------------------------------------

    def standings(self):
        """Every strategy, best balance first."""
        mk = self.markets()
        rows = []
        for a in self.accounts.values():
            eq = a.equity(mk)
            rows.append({"name": a.name, "blurb": a.params["blurb"], "equity": eq,
                         "pnl_pct": (eq / START_BALANCE - 1) * 100, "bets": a.bets,
                         "wins": a.wins, "losses": a.losses, "joined": a.participated()})
        return sorted(rows, key=lambda r: -r["equity"])

    def snapshot(self, name=None, coin=None):
        """One strategy's state. `coin` scopes the live-market view and its open bets to a
        single coin; the balance is always the shared one."""
        a = self.account(name)
        c = self.coin(coin)
        mk = self.markets()
        eq = a.equity(mk)
        return {
            "name": a.name, "blurb": a.params["blurb"], "equity": eq,
            "pnl": eq - START_BALANCE, "pnl_pct": (eq / START_BALANCE - 1) * 100,
            "cash": a.cash, "open": a.open_lots(), "log": a.log,
            "coin": c.coin, "market": c.market, "hourly": c.hourly,
            "late_share": c.late_volume_share(),
            "coin_open": [l for l in a.open_lots() if l.get("coin") == c.coin],
            "coin_log": [l for l in a.log if l.get("coin") == c.coin],
            "view": a.views.get(c.coin), "bets": a.bets, "wins": a.wins, "losses": a.losses,
            "paused": self.paused, "rounds_monitored": self.rounds_monitored,
            "rounds_joined": a.participated(),
        }

    # ---- files ------------------------------------------------------------

    def _append_csv(self, e, now):
        acct = self.accounts[e["strategy"]]
        # the book as it stood when this happened, so a row can be read on its own later
        mkt = (self.coins[e["coin"]].market or {}) if e.get("coin") in self.coins else {}
        yb, ya = mkt.get("yes_bid", ""), mkt.get("yes_ask", "")
        row = {
            "time": _iso(now), "strategy": e["strategy"], "coin": e.get("coin", ""),
            "event": {"bet": "BET", "sold": "SOLD", "settled": "SETTLED"}[e["kind"]],
            "ticker": e["ticker"], "side": e["side"], "contracts": e["contracts"],
            "price": e["exit_price"] if e["kind"] == "sold" else e["price"],
            "multiplier": e["multiplier"], "fee": e["fee"], "cost": e["cost"],
            "payout": e.get("payout", ""), "pnl": e.get("pnl", ""),
            "result": e.get("result", ""), "strike": e["strike"],
            "btc_price": e.get("exit_btc", e["btc_price"]),
            "final_value": e.get("final_value", ""),
            "balance_after": round(acct.equity(self.markets()), 2),
            "model_prob": e.get("model_prob", ""), "edge": e.get("edge", ""),
            "why": e.get("why") or ("entry" if e["kind"] == "bet" else ""),
            "tau": round(e["close"] - now),
            "yes_bid": yb, "yes_ask": ya,
            "mkt_prob": round((yb + ya) / 2, 3) if yb != "" and ya != "" else "",
        }
        self._append(self.csv_path, CSV_FIELDS, row)

    def _load(self):
        try:
            with open(self.json_path) as f:
                d = json.load(f)
            for name, acct in d["accounts"].items():
                if name in self.accounts:
                    self.accounts[name].load(acct)
            self.selected = d.get("selected", self.selected)
            self.selected_coin = d.get("selected_coin", self.selected_coin)
            self.paused = bool(d.get("paused", False))
            self.started_at = float(d.get("started_at", self.started_at))
            saved_rounds = int(d.get("rounds_monitored", 0))
            self._round_close = d.get("last_round_close")
            if self._round_close is None and saved_rounds:
                # Written before a round meant one window: it counted each coin separately,
                # so scale it back down to windows.
                saved_rounds = round(saved_rounds / max(1, len(self.coins)))
            self.rounds_monitored = saved_rounds
            # Rounds that were still open when the app last closed. Without these, every
            # restart would punch a hole in the round log exactly where a round was live.
            self._rounds = {k: v for k, v in (d.get("live_rounds") or {}).items()
                            if v.get("close", 0) > time.time() - 7200}
            for name, saved in (d.get("coins") or {}).items():
                c = self.coins.get(name)
                if not c:
                    continue
                c.rounds_monitored = int(saved.get("rounds_monitored", 0))
                c.round_ticker = saved.get("last_round_ticker")
                for row in saved.get("index_offsets", []):
                    c.offset_obs.append((float(row[0]), float(row[1])))
        except Exception:
            # No file yet, or a format we don't recognise: start every account fresh.
            self.accounts = {p["name"]: Account(p) for p in STRATEGIES}

    def save(self, force=False):
        """Write kalshi_balance.json. Throttled to every few seconds unless forced."""
        if not force and time.monotonic() - self._last_save < 5:
            return
        self._last_save = time.monotonic()
        standings = self.standings()
        mk = self.markets()
        data = {
            "note": "Simulation only. No real money. One balance per strategy, shared across coins.",
            "updated": datetime.now().isoformat(timespec="seconds"),
            "starting_balance_each": START_BALANCE,
            "selected": self.selected,
            "selected_coin": self.selected_coin,
            "leader": standings[0]["name"],
            "paused": self.paused,
            "started_at": self.started_at,
            "rounds_monitored": self.rounds_monitored,
            "rounds_note": "a round is one 15-minute window across every coin, counted once",
            "live_rounds": self._rounds,  # open rounds, so the round log survives a restart
            "last_round_close": self._round_close,
            "rounds_monitored_per_coin": self.coin_rounds(),
            "leaderboard": [{"strategy": r["name"], "balance": round(r["equity"], 2),
                             "return_pct": round(r["pnl_pct"], 2), "bets": r["bets"],
                             "rounds_joined": r["joined"]}
                            for r in standings],
            "coins": {
                name: {
                    "rounds_monitored": c.rounds_monitored,
                    "last_round_ticker": c.round_ticker,
                    "index_offset_pct": round(c.offset_pct, 8),
                    "index_offsets": [[round(t, 3), o] for t, o in c.offset_obs],
                    "open_bets": sum(1 for a in self.accounts.values()
                                     for l in a.open_lots() if l.get("coin") == name),
                }
                for name, c in self.coins.items()
            },
            "index_offset_note": "how far Kalshi's index sits above our exchange price, "
                                 "learned from recent settlements, per coin",
            "accounts": {n: a.to_dict(a.equity(mk)) | {"cash": round(a.cash, 2)}
                         for n, a in self.accounts.items()},
        }
        tmp = self.json_path + ".tmp"
        try:
            with open(tmp, "w") as f:
                json.dump(data, f, indent=2)
            os.replace(tmp, self.json_path)  # swap in one step so it's never half-written
        except OSError:
            pass

    def set_paused(self, paused):
        self.paused = paused
        self.save(force=True)

    def reset(self):
        """Start every account over with the starting balance. The old files are kept, renamed."""
        stamp = time.strftime("%Y%m%d_%H%M%S")
        for path in (self.json_path, self.csv_path, self.exit_path, self.round_path):
            if os.path.exists(path):
                root, ext = os.path.splitext(path)
                try:
                    os.replace(path, f"{root}_old_{stamp}{ext}")
                except OSError:
                    pass
        self.accounts = {p["name"]: Account(p) for p in STRATEGIES}
        self.started_at = time.time()
        self.rounds_monitored = 0
        self._round_close = None
        self._rounds = {}
        for c in self.coins.values():
            c.rounds_monitored = 0
            c.round_ticker = None
        self.save(force=True)  # the learned index offsets are about the market, so they stay

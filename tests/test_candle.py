"""The three candle strategies.

They buy the way the last minute moved and let the exits do the rest -- take_capture to bank
a gain, stop_loss to cut a loser. The three are identical apart from when they may open a
position, which is the whole point: whatever the three-way result turns out to be, it is
about timing rather than about tuning.
"""

import unittest

import helpers as h
import kalshi_trader as kt

CANDLES = ("Candle", "Candle Open", "Candle Step")


class TheThree(unittest.TestCase):

    def setUp(self):
        self.params = {p["name"]: p for p in kt.STRATEGIES if p.get("kind") == "candle"}

    def test_all_three_exist(self):
        self.assertEqual(sorted(self.params), sorted(CANDLES))

    def test_they_differ_only_in_when_they_may_open(self):
        """If they differed in stake or thresholds too, comparing them would measure a
        muddle rather than the timing."""
        timing = {"name", "blurb", "window", "cutoff", "every", "burst"}
        shapes = {name: {k: v for k, v in p.items() if k not in timing}
                  for name, p in self.params.items()}
        first = shapes["Candle"]
        for name in CANDLES[1:]:
            with self.subTest(strategy=name):
                self.assertEqual(shapes[name], first)

    def test_each_both_banks_and_cuts(self):
        """'Accepting wins and selling for losses' is both exits, not one."""
        for name, p in self.params.items():
            with self.subTest(strategy=name):
                self.assertIn("take_capture", p)
                self.assertIn("stop_loss", p)
                self.assertEqual(p["exit"], "ev")

    def test_they_hold_briefly_and_buy_often(self):
        for name, p in self.params.items():
            with self.subTest(strategy=name):
                self.assertLessEqual(p["min_hold"], 5)
                self.assertLessEqual(p["min_gap"], 10)

    def test_each_gets_an_anti_twin(self):
        twins = {p["name"] for p in kt.ANTI_STRATEGIES}
        for name in CANDLES:
            self.assertIn(f"Anti {name}", twins)


class WhenTheyMayOpen(unittest.TestCase):
    """Windows are described from the start of the round, so they are checked that way."""

    def opens_at(self, name, elapsed):
        params = next(p for p in kt.STRATEGIES if p["name"] == name)
        return kt.Account(dict(params))._candle_open(params, kt.ROUND_SECONDS - elapsed)

    def test_the_continuous_one_is_open_all_round(self):
        for elapsed in (0, 150, 449, 750, 899):
            with self.subTest(elapsed=elapsed):
                self.assertTrue(self.opens_at("Candle", elapsed))

    def test_the_early_one_shuts_after_two_and_a_half_minutes(self):
        for elapsed in (0, 60, 149, 150):
            self.assertTrue(self.opens_at("Candle Open", elapsed), elapsed)
        for elapsed in (151, 300, 600, 890):
            self.assertFalse(self.opens_at("Candle Open", elapsed), elapsed)

    def test_the_stepped_one_opens_six_times(self):
        bursts = [e for e in range(0, kt.ROUND_SECONDS) if self.opens_at("Candle Step", e)]
        starts = [e for e in bursts if e - 1 not in bursts]
        self.assertEqual(starts, [0, 150, 300, 450, 600, 750])
        self.assertEqual(len(bursts), 6 * 30)

    def test_the_stepped_one_is_shut_between_bursts(self):
        for elapsed in (31, 100, 149, 181, 299):
            with self.subTest(elapsed=elapsed):
                self.assertFalse(self.opens_at("Candle Step", elapsed))


class ItFollowsTheMove(unittest.TestCase):
    """Momentum, not mispricing: whichever way the last minute went, that is the side."""

    CLOSE = 1_800_000_900.0

    def run_round(self, name, direction, elapsed_at=30):
        """Drive one account directly, with a price path that moves `direction`."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts[name]
        now = self.CLOSE - kt.ROUND_SECONDS + elapsed_at
        for k in range(90):  # a minute and a half of ticks, trending
            t.observe("BTC", 80000.0 * (1 + direction * k * 2e-5), now - 90 + k)
        ctx = {"candle": direction * 0.001, "p_tail": None, "spike": None}
        mkt = h.market("BTC", "KXBTC15M-CANDLE", 80000.0, self.CLOSE,
                       yes_bid=0.49, yes_ask=0.50)
        acct.step(mkt, 80000.0, now, 0.5, False, ctx)
        return acct

    def test_a_rising_minute_buys_up(self):
        acct = self.run_round("Candle", +1)
        self.assertTrue(acct.log, "it passed on a clear move")
        self.assertEqual(acct.log[0]["side"], "UP")

    def test_a_falling_minute_buys_down(self):
        acct = self.run_round("Candle", -1)
        self.assertTrue(acct.log)
        self.assertEqual(acct.log[0]["side"], "DOWN")

    def test_a_flat_minute_is_left_alone(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts["Candle"]
        now = self.CLOSE - 800
        for k in range(90):
            t.observe("BTC", 80000.0, now - 90 + k)
        acct.step(h.market("BTC", "KXBTC15M-CANDLE", 80000.0, self.CLOSE, 0.49, 0.50),
                  80000.0, now, 0.5, False,
                  {"candle": 0.00001, "p_tail": None, "spike": None})
        self.assertEqual(acct.log, [], "it traded noise smaller than its own threshold")

    def test_the_early_one_passes_once_its_window_has_gone(self):
        early = self.run_round("Candle Open", +1, elapsed_at=400)
        self.assertEqual(early.log, [], "it opened well past two and a half minutes")
        inside = self.run_round("Candle Open", +1, elapsed_at=60)
        self.assertTrue(inside.log, "it passed inside its own window")

    def test_it_never_holds_both_sides_of_a_market(self):
        """Kalshi nets a position, so a second side would just offset the first."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts["Candle"]
        now = self.CLOSE - 800
        for k in range(90):
            t.observe("BTC", 80000.0 * (1 + k * 2e-5), now - 90 + k)
        mkt = h.market("BTC", "KXBTC15M-CANDLE", 80000.0, self.CLOSE, 0.49, 0.50)
        for direction, when in ((+1, 0), (-1, 60), (+1, 120)):
            acct.step(mkt, 80000.0, now + when, 0.5, False,
                      {"candle": direction * 0.001, "p_tail": None, "spike": None})
        sides = {lot["side"] for lot in acct.open_lots()}
        self.assertLessEqual(len(sides), 1, f"held both sides at once: {sides}")

    def test_cash_reconciles(self):
        acct = self.run_round("Candle", +1)
        self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)

    def test_it_respects_the_exposure_cap(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts["Candle"]
        acct.cash = kt.START_BALANCE * 9
        now = self.CLOSE - 800
        for k in range(90):
            t.observe("BTC", 80000.0 * (1 + k * 2e-5), now - 90 + k)
        mkt = h.market("BTC", "KXBTC15M-CANDLE", 80000.0, self.CLOSE, 0.49, 0.50)
        for step in range(0, 400, 6):
            acct.step(mkt, 80000.0, now + step, 0.5, False,
                      {"candle": 0.001, "p_tail": None, "spike": None})
        self.assertLessEqual(acct.committed(), kt.TOTAL_CAP * kt.START_BALANCE + 1e-6)


class TheSignal(unittest.TestCase):

    def test_it_measures_the_last_minute_from_the_per_second_ring(self):
        """Minute closes would make a strategy that trades in seconds wait for a boundary."""
        coin = kt.CoinState("BTC")
        for k in range(120):
            coin.observe(80000.0 * (1 + k * 1e-5), 1_800_000_000 + k)
        move = coin.candle_move(1_800_000_119)
        self.assertIsNotNone(move)
        self.assertGreater(move, 0)

    def test_it_says_nothing_until_there_are_enough_ticks(self):
        coin = kt.CoinState("BTC")
        for k in range(4):
            coin.observe(80000.0, 1_800_000_000 + k)
        self.assertIsNone(coin.candle_move(1_800_000_003))

    def test_a_fall_reads_negative(self):
        coin = kt.CoinState("BTC")
        for k in range(120):
            coin.observe(80000.0 * (1 - k * 1e-5), 1_800_000_000 + k)
        self.assertLess(coin.candle_move(1_800_000_119), 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)

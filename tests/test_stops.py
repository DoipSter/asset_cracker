"""Cutting losses.

The Scalper does not cut losses, and that is deliberate rather than an oversight: both ways
of doing it lose money over a month of recorded rounds. These tests pin the two mechanisms so
the knobs keep working if anyone wants to retry the idea on better data, and pin the fact
that they are off, so nobody turns one on by accident and wonders why the balance moved.

The measurement behind the decision is in research/backtest_stops.py.
"""

import unittest

import helpers as h
import kalshi_trader as kt


CLOSE = 1_800_000_900.0
TICKER = "KXBTC15M-STOP"
STRIKE = 80000.0


def dying_position(case, flat_bid=None, **params):
    """A Scalper position opened early in the round, then left to rot.

    Entry is forced by showing a book that prices UP far cheaper than the model does. After
    that the bid either decays (a position going to zero) or sits flat and underwater
    (a position with nowhere left to go), depending on `flat_bid`.

    take_capture is off throughout: it banks winners and would leave nothing open to cut,
    and these tests are about the stop rather than the interaction between the two.
    """
    t = h.new_trader()
    t._append_csv = lambda *a, **k: None
    acct = t.accounts["Scalper"]
    acct.params = dict(acct.params, take_capture=None, **params)

    for k in range(200):  # prime the volatility estimate
        t.observe("BTC", 80400.0 + (k % 5), CLOSE - 1200 + k)

    # well above the strike, so the model likes UP, but the book sells it at 20c
    for step in range(0, 120, 10):
        now = CLOSE - 880 + step
        t.observe("BTC", 80400.0, now)
        t.step("BTC", h.market("BTC", TICKER, STRIKE, CLOSE, 0.19, 0.20), 80400.0, now)
    case.assertTrue(acct.open_lots(), "nothing was opened, so there is nothing to cut")

    # ...and now the book goes against it, falling well below the 20c it paid. The observed
    # price is held above the strike throughout, so the model still likes UP: that keeps the
    # value exit quiet and leaves the stop as the only rule that can act, which is the point.
    steps = ([(flat_bid, tau) for tau in (600, 400, 240, 120, 70, 40, 20)] if flat_bid
             else list(zip((0.14, 0.09, 0.05, 0.02, 0.01),
                           (600, 480, 360, 240, 120))))
    for bid, tau in steps:
        now = CLOSE - tau
        t.observe("BTC", 80400.0, now)
        t.step("BTC", h.market("BTC", TICKER, STRIKE, CLOSE, bid,
                               round(bid + 0.01, 2)), 80400.0, now)
    return acct


class OffByDefault(unittest.TestCase):

    def test_no_shipped_strategy_cuts_losses(self):
        """If this starts failing, someone enabled a stop. That may be right -- but it needs
        the numbers in research/backtest_stops.py to have changed first."""
        for s in kt.STRATEGIES:
            with self.subTest(strategy=s["name"]):
                self.assertNotIn("stop_loss", s)
                self.assertNotIn("stop_tau", s)

    def test_a_losing_position_is_held_to_settlement(self):
        """The behaviour the default produces, stated plainly: a position that dies is held
        all the way down rather than sold."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts["Scalper"]
        h.lagging_book_round(t)
        held = [l for l in acct.log if l["status"] == "open"]
        self.assertTrue(acct.log, "nothing traded, so this proves nothing")
        # nothing was sold at a loss: every sale is a gain
        for lot in acct.log:
            if lot["status"] == "sold":
                self.assertGreater(lot["payout"], lot["cost"], lot)


class PriceStop(unittest.TestCase):
    """stop_loss: sell once this much of the stake is gone."""

    def _dying_position(self, **params):
        """Open a position early in the round, then let its bid decay across the rest of it.

        Built here rather than from lagging_book_round because the decay has to happen after
        the entry: the minimum-hold check compares `now` against the lot's timestamp, so
        stepping at an earlier time skips the lot silently and the stop is never evaluated.
        """
        return dying_position(self, **params)

    def test_without_it_the_position_is_still_open(self):
        acct = self._dying_position()
        self.assertTrue(acct.open_lots(), "something sold without a stop being set")

    def test_with_it_the_position_is_cut(self):
        acct = self._dying_position(stop_loss=0.50)
        cut = [l for l in acct.log if l.get("why") == "stop"]
        self.assertTrue(cut, "the stop never fired on a position that lost 96% of its bid")
        for lot in cut:
            self.assertLess(lot["payout"], lot["cost"], "a stop banked a gain")

    def test_cash_still_reconciles_when_stops_fire(self):
        acct = self._dying_position(stop_loss=0.50)
        self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)


class TimeStop(unittest.TestCase):
    """stop_tau: sell a position that is still behind this close to the bell. Unlike a price
    floor it cannot be gapped through, because it triggers on the clock."""

    def _late_loser(self, **params):
        """Underwater and walking into the close, with the bid flat so only the clock can
        trigger a sale."""
        return dying_position(self, flat_bid=0.12, **params)

    def test_it_fires_only_inside_the_window(self):
        acct = self._late_loser(stop_tau=60)
        cut = [l for l in acct.log if l.get("why") == "stop"]
        self.assertTrue(cut, "the time stop never fired")
        for lot in cut:
            self.assertLessEqual(lot["close"] - lot["exit_t"], 60,
                                 "a time stop fired outside its window")

    def test_it_leaves_a_winning_position_alone(self):
        """It cuts losers, not positions that happen to be late."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts["Scalper"]
        acct.params = dict(acct.params, stop_tau=300, take_capture=None)
        h.lagging_book_round(t)
        for lot in acct.log:
            if lot.get("why") == "stop":
                self.assertLess(lot["payout"], lot["cost"],
                                "the time stop sold a position that was ahead")


if __name__ == "__main__":
    unittest.main(verbosity=2)

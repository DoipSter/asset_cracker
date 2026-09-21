"""Money and signals must not leak between the two worlds.

Twelve accounts trade at once and six of them are mirrors of the other six, placed in the
same instant on the opposite side. That is exactly the shape of mistake that produces a
balance quietly built from someone else's trades, so this file checks the separation rather
than assuming it.
"""

import unittest

import helpers as h
import kalshi_trader as kt


def both_worlds(seed_rich=False):
    t = h.new_trader()
    t._append_csv = lambda *a, **k: None
    if seed_rich:
        for acct in t.accounts.values():
            acct.cash = kt.START_BALANCE * 3
    h.lagging_book_round(t)
    return t


class BalancesAreSeparate(unittest.TestCase):

    def setUp(self):
        self.t = both_worlds()
        self.traded = [n for n, a in self.t.accounts.items() if a.log]
        self.assertTrue(self.traded, "nothing traded, so none of this proves anything")

    def test_every_balance_comes_only_from_its_own_trades(self):
        for name, acct in self.t.accounts.items():
            with self.subTest(strategy=name):
                self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)

    def test_no_account_holds_another_accounts_lot(self):
        """Lot ids are per account. A lot appearing in two logs would mean one account's
        money was being moved by another's trade."""
        seen = {}
        for name, acct in self.t.accounts.items():
            for lot in acct.log:
                key = (id(lot),)
                self.assertNotIn(key, seen,
                                 f"{name} and {seen.get(key)} share the same lot object")
                seen[key] = name

    def test_a_twins_lots_are_all_on_the_opposite_side(self):
        for real in kt.STRATEGIES:
            a = self.t.accounts[real["name"]]
            b = self.t.accounts[f"Anti {real['name']}"]
            if not a.log:
                continue
            by_id = {l["id"]: l for l in a.log}
            for lot in b.log:
                with self.subTest(strategy=real["name"], lot=lot["id"]):
                    origin = by_id[lot["mirror_of"]]
                    self.assertEqual(lot["side"], kt.OPPOSITE[origin["side"]])

    def test_settling_pays_each_account_on_its_own_side(self):
        self.t.on_settled("KXBTC15M-LAG", "yes", "80,200.00", 1_800_000_900.0, 80200.0)
        for name, acct in self.t.accounts.items():
            for lot in acct.log:
                if lot["status"] not in ("won", "lost"):
                    continue
                with self.subTest(strategy=name, lot=lot["id"]):
                    should_win = lot["side"] == "UP"  # the round settled "yes"
                    self.assertEqual(lot["status"], "won" if should_win else "lost")
                    self.assertEqual(lot["payout"],
                                     float(lot["contracts"]) if should_win else 0.0)
            with self.subTest(strategy=name):
                self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)

    def test_changing_one_balance_moves_no_other(self):
        before = {n: a.cash for n, a in self.t.accounts.items()}
        target = self.traded[0]
        self.t.accounts[target].cash += 500.0
        for name, acct in self.t.accounts.items():
            if name == target:
                continue
            with self.subTest(strategy=name):
                self.assertEqual(acct.cash, before[name])


class EquityPricesTheRightSide(unittest.TestCase):
    """An open DOWN position is worth the no side's bid, not the yes side's. Getting this
    backwards would show a twin's losses as gains and vice versa."""

    def _account_holding(self, side, contracts=10, price=0.40):
        acct = kt.Account(dict(kt.STRATEGIES[0]))
        acct.log = [{"id": 1, "t": 0.0, "time": "x", "ticker": "KXBTC15M-TEST",
                     "coin": "BTC", "side": side, "contracts": contracts,
                     "price": price, "multiplier": 1 / price, "fee": 0.0,
                     "cost": contracts * price, "strike": 80000.0,
                     "close": 1_800_000_900.0, "btc_price": 80000.0,
                     "model_prob": 0.5, "edge": 0.0, "status": "open"}]
        acct.cash = 0.0
        return acct

    def test_an_up_lot_is_priced_off_the_yes_bid(self):
        mkt = {"BTC": h.market(yes_bid=0.70, yes_ask=0.72)}
        acct = self._account_holding("UP")
        expected = 10 * 0.70 - kt.kalshi_fee(10, 0.70)
        self.assertAlmostEqual(acct.equity(mkt), expected, places=6)

    def test_a_down_lot_is_priced_off_the_no_bid(self):
        mkt = {"BTC": h.market(yes_bid=0.70, yes_ask=0.72)}  # no_bid = 1 - 0.72 = 0.28
        acct = self._account_holding("DOWN")
        expected = 10 * 0.28 - kt.kalshi_fee(10, 0.28)
        self.assertAlmostEqual(acct.equity(mkt), expected, places=6)

    def test_the_two_sides_are_not_valued_the_same(self):
        mkt = {"BTC": h.market(yes_bid=0.70, yes_ask=0.72)}
        up = self._account_holding("UP").equity(mkt)
        down = self._account_holding("DOWN").equity(mkt)
        self.assertNotAlmostEqual(up, down, places=2)

    def test_a_lot_from_another_round_falls_back_to_its_cost(self):
        """The live book belongs to whatever round is open now. Pricing last round's lot
        against it would value a settled position at today's odds."""
        acct = self._account_holding("UP")
        acct.log[0]["ticker"] = "KXBTC15M-OLDROUND"
        mkt = {"BTC": h.market(yes_bid=0.70, yes_ask=0.72)}
        self.assertAlmostEqual(acct.equity(mkt), acct.log[0]["cost"], places=6)


class StandingsAddUp(unittest.TestCase):

    def test_each_world_totals_only_its_own_accounts(self):
        t = both_worlds()
        mk = t.markets()
        for anti in (False, True):
            rows = t.standings(anti=anti)
            names = {p["name"] for p in kt.strategies(anti)}
            with self.subTest(world="anti" if anti else "real"):
                self.assertEqual({r["name"] for r in rows}, names)
                for row in rows:
                    self.assertAlmostEqual(row["equity"],
                                           t.accounts[row["name"]].equity(mk), places=6)

    def test_a_twins_equity_never_reads_its_originals(self):
        t = both_worlds()
        mk = t.markets()
        for real in kt.STRATEGIES:
            a, b = t.accounts[real["name"]], t.accounts[f"Anti {real['name']}"]
            if not a.log:
                continue
            before = b.equity(mk)
            a.cash += 1000.0  # move the original's money
            with self.subTest(strategy=real["name"]):
                self.assertAlmostEqual(b.equity(mk), before, places=6)
            a.cash -= 1000.0

    def test_the_snapshot_reports_the_account_it_was_asked_for(self):
        t = both_worlds()
        for name, acct in t.accounts.items():
            s = t.snapshot(name=name, coin="BTC")
            with self.subTest(strategy=name):
                self.assertEqual(s["name"], name)
                self.assertEqual(s["cash"], acct.cash)
                self.assertEqual(s["bets"], acct.bets)


class SignalsDoNotBleed(unittest.TestCase):
    """The glow and the toast are per strategy. A mirror fires in the same instant as its
    original, so anything that reads across worlds lights up twice for one decision."""

    def setUp(self):
        self.src = h.app_source()

    def test_the_tab_dot_only_watches_its_own_world(self):
        body = self.src[self.src.index("    def _header(self, s):"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertNotIn("for n in self.app.flash)", body,
                         "the tab dot glows for every strategy in both worlds")
        self.assertIn("self.world_names", body,
                      "the tab dot should only consider its own world's strategies")

    def test_a_panel_knows_which_names_belong_to_it(self):
        self.assertIn("def world_names(self)", self.src)

    def test_only_one_world_is_notified(self):
        body = self.src[self.src.index("    def _on_trade(self, e):"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertNotIn("self.trader.tracked(False), self.trader.tracked(True)", body,
                         "both worlds are notified, so one bet pings twice")
        self.assertIn("self.trader.tracked(self.hub.chart_anti)", body,
                      "notifications should follow the world the globe selects")


if __name__ == "__main__":
    unittest.main(verbosity=2)

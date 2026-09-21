"""The anti-world: for every strategy, a twin that takes the other side of its trades.

The twin has no opinions. It mirrors -- same moment, same number of contracts, opposite
side -- and closes when the original closes. That is what makes the pair worth having: with
n contracts on each side exactly one of them pays $n, so whatever the pair loses is precisely
what the two fills and their fees cost. If a strategy and its mirror both lose, the losses
are costs rather than bad judgement, and no amount of tuning the model will fix that.
"""

import os
import unittest

import helpers as h
import kalshi_trader as kt


class Pairing(unittest.TestCase):

    def test_every_strategy_has_exactly_one_twin(self):
        self.assertEqual(len(kt.ANTI_STRATEGIES), len(kt.STRATEGIES))
        self.assertEqual(len(kt.ALL_STRATEGIES),
                         len(kt.STRATEGIES) + len(kt.ANTI_STRATEGIES))
        for real, anti in zip(kt.STRATEGIES, kt.ANTI_STRATEGIES):
            with self.subTest(strategy=real["name"]):
                self.assertEqual(anti["name"], f"Anti {real['name']}")
                self.assertTrue(anti["anti"])

    def test_a_twin_has_no_opinion_to_invert(self):
        """An earlier version gave twins an inverted belief and let them trade on it, which
        made them different strategies rather than mirrors -- Anti Scalper took 32 bets
        against the Scalper's 5."""
        for anti in kt.ANTI_STRATEGIES:
            with self.subTest(strategy=anti["name"]):
                self.assertNotIn("invert", anti)

    def test_the_real_strategies_are_untouched(self):
        for real in kt.STRATEGIES:
            with self.subTest(strategy=real["name"]):
                self.assertNotIn("anti", real)


class OwnMoney(unittest.TestCase):

    def test_twelve_separate_banks(self):
        t = h.new_trader()
        self.assertEqual(len(t.accounts), 12)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertEqual(acct.cash, kt.START_BALANCE)

    def test_a_twin_spending_does_not_touch_its_original(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        h.lagging_book_round(t)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)


class ItIsAMirror(unittest.TestCase):

    def setUp(self):
        self.t = h.new_trader()
        self.t._append_csv = lambda *a, **k: None
        h.lagging_book_round(self.t)
        self.traded = [p["name"] for p in kt.STRATEGIES if self.t.accounts[p["name"]].log]
        self.assertTrue(self.traded, "nothing traded, so none of this proves anything")

    def test_same_number_of_bets(self):
        for name in self.traded:
            with self.subTest(strategy=name):
                self.assertEqual(len(self.t.accounts[f"Anti {name}"].log),
                                 len(self.t.accounts[name].log))

    def test_opposite_side_same_stake_same_moment(self):
        """Matched by money, not by contract count: the two sides of a market are different
        prices, so equal contracts would mean very unequal stakes."""
        for name in self.traded:
            real = self.t.accounts[name].log
            anti = self.t.accounts[f"Anti {name}"].log
            for a, b in zip(real, anti):
                with self.subTest(strategy=name, bet=a["id"]):
                    self.assertEqual(b["side"], kt.OPPOSITE[a["side"]])
                    self.assertEqual(b["t"], a["t"])
                    self.assertEqual(b["ticker"], a["ticker"])
                    self.assertEqual(b["mirror_of"], a["id"])
                    self.assertLessEqual(b["cost"], a["cost"] + 0.01,
                                         "the twin staked more than its original")
                    # within one contract of the stake: it cannot buy a fraction of one
                    self.assertGreater(b["cost"], a["cost"] - b["price"] - 0.05)

    def test_a_twin_never_exceeds_the_exposure_cap(self):
        """Matching contracts instead of stake put a twin $524 into a $250 cap, because the
        opposite side costs more per contract."""
        for name in self.traded:
            twin = self.t.accounts[f"Anti {name}"]
            with self.subTest(strategy=name):
                self.assertLessEqual(twin.committed(),
                                     kt.TOTAL_CAP * kt.START_BALANCE + 1e-6)

    def test_a_twin_never_trades_on_its_own(self):
        """Every twin bet points at an original bet. One that did not would mean the twin had
        started having opinions again."""
        for name in self.traded:
            ids = {l["id"] for l in self.t.accounts[name].log}
            for lot in self.t.accounts[f"Anti {name}"].log:
                with self.subTest(strategy=name):
                    self.assertIn(lot.get("mirror_of"), ids)


class ClosingTogether(unittest.TestCase):

    def _round(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        h.lagging_book_round(t)
        return t

    def test_a_twin_closes_when_its_original_does(self):
        t = self._round()
        checked = 0
        for real in kt.STRATEGIES:
            a, b = t.accounts[real["name"]], t.accounts[f"Anti {real['name']}"]
            sold = {l["id"] for l in a.log if l["status"] == "sold"}
            if not sold:
                continue
            checked += 1
            mirrored = {l["mirror_of"] for l in b.log if l["status"] == "sold"}
            with self.subTest(strategy=real["name"]):
                self.assertEqual(mirrored, sold,
                                 "the twin did not close what its original closed")
        self.assertTrue(checked, "nothing sold early, so this scenario proves nothing")

    def test_a_mirrored_sale_is_marked_as_one(self):
        t = self._round()
        for acct in t.accounts.values():
            if not acct.params.get("anti"):
                continue
            for lot in acct.log:
                if lot["status"] == "sold":
                    with self.subTest(strategy=acct.name):
                        self.assertEqual(lot.get("why"), "mirror")

    def test_at_settlement_exactly_one_side_of_a_pair_is_paid(self):
        t = self._round()
        t.on_settled("KXBTC15M-LAG", "yes", "80,200.00", 1_800_000_900.0, 80200.0)
        checked = 0
        for real in kt.STRATEGIES:
            a, b = t.accounts[real["name"]], t.accounts[f"Anti {real['name']}"]
            twins = {l["mirror_of"]: l for l in b.log if l["status"] in ("won", "lost")}
            for lot in a.log:
                if lot["status"] not in ("won", "lost"):
                    continue
                twin = twins.get(lot["id"])
                if twin is None:
                    continue  # sold early, or the twin could not afford it
                checked += 1
                with self.subTest(strategy=real["name"], bet=lot["id"]):
                    self.assertNotEqual(lot["status"], twin["status"],
                                        "both sides of a pair had the same result")
        self.assertTrue(checked, "no pair reached settlement together")

    def test_cash_reconciles_after_settlement(self):
        t = self._round()
        t.on_settled("KXBTC15M-LAG", "yes", "80,200.00", 1_800_000_900.0, 80200.0)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)


class WhenItCannotFollow(unittest.TestCase):
    """A twin can be too poor to mirror. Revival is switched off in the first two, because
    they are about what the mirror does with an empty pocket, not about bankruptcy -- with it
    on, a starved twin is restaked mid-round and the balance under test disappears. The third
    covers that interaction deliberately.
    """

    def _starved(self, cash, revive=False):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        if not revive:
            t._check_broke = lambda *a, **k: []
        for acct in t.accounts.values():
            if acct.params.get("anti"):
                acct.cash = cash
        h.lagging_book_round(t)
        return t

    def test_a_twin_with_nothing_skips_rather_than_going_negative(self):
        """A cent buys no contract at all: the cheapest is a cent plus slippage plus fee."""
        t = self._starved(0.01)
        for acct in t.accounts.values():
            if acct.params.get("anti"):
                with self.subTest(strategy=acct.name):
                    self.assertEqual(acct.log, [])
                    self.assertAlmostEqual(acct.cash, 0.01, places=2)

    def test_a_twin_buys_what_it_can_afford(self):
        """Following only partway is worth seeing, not worth crashing over."""
        stake = 12.00
        t = self._starved(stake)
        checked = 0
        for real in kt.STRATEGIES:
            a, b = t.accounts[real["name"]], t.accounts[f"Anti {real['name']}"]
            if not a.log:
                continue
            checked += 1
            with self.subTest(strategy=real["name"]):
                self.assertLessEqual(len(b.log), len(a.log))
                self.assertGreaterEqual(b.cash, 0.0)
                self.assertAlmostEqual(b.cash, h.cash_balances(b, start=stake), places=2)
                for lot in b.log:
                    self.assertLessEqual(lot["cost"], stake + 0.01)
        self.assertTrue(checked, "nothing traded, so this proves nothing")

    def test_a_starved_twin_retires_rather_than_being_restaked(self):
        """A twin that runs out stops for good and its original carries on without it. Not
        restaked, because a twin measures its original rather than competing with it -- a
        fresh stake each time it failed would say nothing except that it failed again.
        Covered in full in test_retirement.py; checked here so this file's picture is whole.
        """
        t = self._starved(0.01, revive=True)
        twins = [a for a in t.accounts.values() if a.params.get("anti")]
        self.assertTrue(any(a.retired for a in twins),
                        "a twin with a cent left was never declared broke")
        for acct in twins:
            if acct.retired:
                with self.subTest(strategy=acct.name):
                    self.assertEqual(acct.bankruptcies, 0)
                    self.assertEqual(acct.log, [])


class Separation(unittest.TestCase):
    """The two worlds are shown in their own panels, so they are never ranked together."""

    def test_standings_never_mix_the_worlds(self):
        t = h.new_trader()
        real = {r["name"] for r in t.standings(anti=False)}
        anti = {r["name"] for r in t.standings(anti=True)}
        self.assertEqual(real, {p["name"] for p in kt.STRATEGIES})
        self.assertEqual(anti, {p["name"] for p in kt.ANTI_STRATEGIES})
        self.assertFalse(real & anti)

    def test_each_world_tracks_its_own_strategy(self):
        t = h.new_trader(quiet=False)
        t.select("Anti Late")
        self.assertEqual(t.tracked(anti=True), "Anti Late")
        self.assertEqual(t.tracked(anti=False), kt.STRATEGIES[0]["name"],
                         "selecting a twin moved the other world's selection")
        t.select("Favorite")
        self.assertEqual(t.tracked(anti=False), "Favorite")
        self.assertEqual(t.tracked(anti=True), "Anti Late")

    def test_both_selections_survive_a_restart(self):
        t = h.new_trader(quiet=False)
        t.select("Anti Scalper")
        t.select("Late")
        t.save(force=True)
        reloaded = kt.KalshiTrader(os.path.dirname(t.json_path), {"BTC": {}})
        self.assertEqual(reloaded.tracked(anti=True), "Anti Scalper")
        self.assertEqual(reloaded.tracked(anti=False), "Late")

    def test_a_twin_can_be_told_apart_in_the_logs(self):
        t = h.new_trader()
        rows = []
        t._append_csv = lambda e, now: rows.append(e["strategy"])
        h.lagging_book_round(t)
        self.assertTrue(any(r.startswith("Anti ") for r in rows),
                        "nothing in the trade log identifies a twin")


if __name__ == "__main__":
    unittest.main(verbosity=2)

"""What happens when a twin runs out of money.

A strategy that runs out is staked again, because it exists to be compared and stops
producing evidence at zero. A twin is not: it is a measurement of its original rather than a
competitor, so handing it a fresh stake every time it fails would say nothing except that it
failed again. It retires, and -- the part that matters -- its original carries on alone.
"""

import os
import unittest

import helpers as h
import kalshi_trader as kt


def starved_twins(cash=0.01, rounds=2):
    """A trader whose twins cannot afford to mirror anything."""
    t = h.new_trader(quiet=False)
    t._append_csv = lambda *a, **k: None
    for acct in t.accounts.values():
        if acct.params.get("anti"):
            acct.cash = cash
    for i in range(rounds):
        h.lagging_book_round(t, ticker=f"KXBTC15M-R{i}")
    return t


class ATwinThatRunsOutStops(unittest.TestCase):

    def setUp(self):
        self.t = starved_twins()
        self.twins = [a for a in self.t.accounts.values() if a.params.get("anti")]

    def test_it_places_no_bets(self):
        for twin in self.twins:
            with self.subTest(strategy=twin.name):
                self.assertEqual(twin.log, [])

    def test_it_is_marked_retired(self):
        for twin in self.twins:
            with self.subTest(strategy=twin.name):
                self.assertTrue(twin.retired)

    def test_it_is_not_staked_again(self):
        """The whole difference from a strategy. A fresh $1,000 would erase the result."""
        for twin in self.twins:
            with self.subTest(strategy=twin.name):
                self.assertEqual(twin.bankruptcies, 0)
                self.assertLess(twin.cash, kt.BANKRUPT_AT)

    def test_it_is_written_up_once_and_not_once_per_tick(self):
        """`broke` answers yes forever at zero, so without the retired check this would
        append a line and write a postmortem on every single tick."""
        path = self.t.bankrupt_path
        lines = [l for l in open(path, encoding="utf-8").read().splitlines() if l]
        self.assertEqual(len(lines), len(self.twins))
        names = [l.split("\t")[1] for l in lines]
        self.assertEqual(sorted(names), sorted(a.name for a in self.twins))
        for line in lines:
            self.assertIn("retired", line)

    def test_its_postmortem_says_it_retired(self):
        reports = [f for f in os.listdir(self.t.folder)
                   if f.startswith("kalshi_bankrupt_Anti ") and f.endswith(".md")]
        self.assertTrue(reports)
        text = open(os.path.join(self.t.folder, reports[0]), encoding="utf-8").read()
        self.assertIn("retired", text)
        self.assertIn("its original trades on alone", text)

    def test_a_retired_twin_stays_retired_across_a_restart(self):
        self.t.save(force=True)
        reopened = kt.KalshiTrader(self.t.folder, {"BTC": {}})
        for twin in self.twins:
            with self.subTest(strategy=twin.name):
                self.assertTrue(reopened.accounts[twin.name].retired)

    def test_a_reopened_retired_twin_still_does_not_bet(self):
        self.t.save(force=True)
        reopened = kt.KalshiTrader(self.t.folder, {"BTC": {}})
        reopened._append_csv = lambda *a, **k: None
        h.lagging_book_round(reopened, ticker="KXBTC15M-AFTER")
        for twin in self.twins:
            with self.subTest(strategy=twin.name):
                self.assertEqual(reopened.accounts[twin.name].log, [])


class TheOriginalCarriesOn(unittest.TestCase):
    """The requirement this was built for."""

    def test_originals_keep_betting_while_their_twins_are_dead(self):
        t = starved_twins()
        traded = [a for a in t.accounts.values()
                  if not a.params.get("anti") and a.log]
        self.assertTrue(traded, "no original placed a bet, so this proves nothing")

    def test_a_dead_twin_changes_nothing_about_its_original(self):
        """Run the same rounds twice, once with solvent twins and once with broke ones. The
        originals' trades must be identical: a twin is a passenger."""
        def trades(cash):
            t = starved_twins(cash=cash)
            return {name: [(l["t"], l["side"], l["contracts"], l["price"])
                           for l in acct.log]
                    for name, acct in t.accounts.items()
                    if not acct.params.get("anti")}

        with_twins = trades(kt.START_BALANCE)
        without = trades(0.01)
        self.assertTrue(any(v for v in with_twins.values()))
        for name in with_twins:
            with self.subTest(strategy=name):
                self.assertEqual(with_twins[name], without[name])

    def test_an_original_is_never_retired_by_its_twin(self):
        t = starved_twins()
        for acct in t.accounts.values():
            if not acct.params.get("anti"):
                with self.subTest(strategy=acct.name):
                    self.assertFalse(acct.retired)


class AStrategyIsStillStakedAgain(unittest.TestCase):
    """Unchanged behaviour for the six originals, checked so that the twin change cannot
    have quietly altered it."""

    def test_a_broke_strategy_is_revived_not_retired(self):
        t = h.new_trader(quiet=False)
        t._append_csv = lambda *a, **k: None
        for acct in t.accounts.values():
            if not acct.params.get("anti"):
                acct.cash = 0.01
        h.lagging_book_round(t)
        revived = [a for a in t.accounts.values()
                   if not a.params.get("anti") and a.bankruptcies]
        self.assertTrue(revived, "a strategy at a cent was never declared broke")
        for acct in revived:
            with self.subTest(strategy=acct.name):
                self.assertFalse(acct.retired)
                self.assertGreater(acct.cash, kt.BANKRUPT_AT)

    def test_reset_clears_retirement(self):
        t = starved_twins()
        self.assertTrue(any(a.retired for a in t.accounts.values()))
        t.reset()
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertFalse(acct.retired)
                self.assertEqual(acct.cash, kt.START_BALANCE)


class TheDisplayCanTellYou(unittest.TestCase):

    def test_standings_carry_the_retired_flag(self):
        t = starved_twins()
        for row in t.standings(anti=True):
            with self.subTest(strategy=row["name"]):
                self.assertTrue(row["retired"])
        for row in t.standings(anti=False):
            with self.subTest(strategy=row["name"]):
                self.assertFalse(row["retired"])

    def test_the_panel_labels_a_retired_row(self):
        """At zero and never moving again, it would otherwise look like a stuck row."""
        src = h.app_source()
        body = src[src.index("    def _strategies(self, s):"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertIn('dead = r.get("retired")', body)
        self.assertIn("retired", body)
        self.assertIn("out of money", body)

    def test_the_event_says_which_happened(self):
        t = h.new_trader(quiet=False)
        t._append_csv = lambda *a, **k: None
        for acct in t.accounts.values():
            acct.cash = 0.01
        h.lagging_book_round(t)
        # every account was broke, so both outcomes should appear
        retired = {a.name for a in t.accounts.values() if a.retired}
        revived = {a.name for a in t.accounts.values() if a.bankruptcies}
        self.assertTrue(retired, "no twin retired")
        self.assertTrue(revived, "no strategy was staked again")
        self.assertFalse(retired & revived, "an account was both retired and revived")


if __name__ == "__main__":
    unittest.main(verbosity=2)

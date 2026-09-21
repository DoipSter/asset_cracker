"""What happens when a strategy runs out of money.

The point of this file is that going broke is not silent. It produces a postmortem while the
evidence still exists, a line in a log that can be watched from outside the app, and an event
the app can notify on -- and then the strategy is staked again so the comparison continues.
"""

import csv
import io
import json
import os
import unittest

import helpers as h
import kalshi_trader as kt


class Thresholds(unittest.TestCase):
    """The one place the dollar figures are written down. Everything else derives them, so
    changing the bank or the cap fails here and nowhere else."""

    def test_bank_and_round_cap(self):
        self.assertEqual(kt.START_BALANCE, 1000.0)
        self.assertAlmostEqual(kt.TOTAL_CAP * kt.START_BALANCE, 250.0, places=9)


class Detection(unittest.TestCase):
    """`broke` deliberately ignores accounts with live bets: while a bet is open the strategy
    still holds something that might pay, and declaring it dead then would flap."""

    def setUp(self):
        self.acct = kt.Account(dict(kt.STRATEGIES[0]))

    def test_a_full_account_is_not_broke(self):
        self.assertFalse(self.acct.broke())

    def test_no_money_and_nothing_open_is_broke(self):
        self.acct.cash = 0.40
        self.assertTrue(self.acct.broke())

    def test_no_money_but_a_live_bet_is_not_broke_yet(self):
        self.acct.cash = 0.40
        self.acct.log = [{"id": 1, "status": "open", "cost": 5.0, "ticker": "X",
                          "side": "UP", "contracts": 5, "price": 0.5, "close": 1.0,
                          "coin": "BTC"}]
        self.assertFalse(self.acct.broke())

    def test_the_floor_is_a_cent_of_headroom_not_zero(self):
        """A single contract costs a cent plus fee, so an account at $0.00 and one at $0.60
        are equally unable to trade."""
        self.acct.cash = kt.BANKRUPT_AT - 0.01
        self.assertTrue(self.acct.broke())
        self.acct.cash = kt.BANKRUPT_AT
        self.assertFalse(self.acct.broke())


class Reporting(unittest.TestCase):
    """Drive a strategy to zero through the real engine, then read what it left behind."""

    @classmethod
    def setUpClass(cls):
        cls.trader = t = h.new_trader(quiet=False)
        acct = t.accounts["Value"]
        # A round it will lose, entered with almost nothing left, so it ends at zero. The
        # engine does the rest: bet, settle against it, notice, report, restake.
        h.run_round(t, "BTC", [80000.0 * (1 + i * 2e-5) for i in range(200)],
                    h.market(strike=80000.0, yes_bid=0.30, yes_ask=0.32))
        for a in t.accounts.values():  # strip everyone to the bone mid-round
            a.cash = 0.50
        # who had actually put money on before the settlement; revive() clears the logs, so
        # this cannot be worked out afterwards
        cls.staked = {name: sum(l["cost"] for l in a.log) for name, a in t.accounts.items()}
        cls.events = t.on_settled("KXBTC15M-TEST", "no", "79,900.00", 1_800_000_900.0,
                                  79900.0)
        cls.folder = os.path.dirname(t.json_path)
        cls.acct = acct

    def test_an_event_is_raised_for_the_app_to_notify_on(self):
        broke = [e for e in self.events if e["kind"] == "bankrupt"]
        self.assertTrue(broke, "going broke produced no event")
        for e in broke:
            self.assertIn("report", e)
            self.assertIn("cash", e)
            # a strategy is staked again and starts its second life; a twin retires instead
            if e["retired"]:
                self.assertEqual(e["life"], 0)
            else:
                self.assertEqual(e["life"], 1)

    def test_the_bankruptcy_log_is_appended_and_parsable(self):
        path = self.trader.bankrupt_path
        self.assertTrue(os.path.exists(path))
        lines = [l for l in io.open(path, encoding="utf-8").read().splitlines() if l]
        self.assertTrue(lines)
        for line in lines:
            fields = line.split("\t")
            self.assertEqual(len(fields), 6, line)
            self.assertTrue(fields[5].startswith("kalshi_bankrupt_"))
            self.assertTrue(fields[5].endswith(".md"))

    def _report_for(self, strategy):
        names = [f for f in os.listdir(self.folder)
                 if f.startswith(f"kalshi_bankrupt_{strategy}_") and f.endswith(".md")]
        self.assertTrue(names, f"no postmortem was written for {strategy}")
        return io.open(os.path.join(self.folder, names[0]), encoding="utf-8").read()

    def test_a_postmortem_is_written_with_the_findings(self):
        """Checked against a strategy that actually traded -- the tables are built from its
        settled bets, so a strategy with none has nothing to tabulate."""
        traded = [name for name, a in self.trader.accounts.items() if a.bankruptcies
                  and self.staked.get(name)]
        self.assertTrue(traded, "no strategy both traded and went broke in this scenario")
        text = self._report_for(traded[0])
        for heading in ("ran out of money", "## What happened", "By coin", "By outcome",
                        "### The worst ", "## What it was running"):
            with self.subTest(section=heading):
                self.assertIn(heading, text)
        # the settings that produced the failure have to be in it, or it cannot be acted on
        self.assertIn("`TOTAL_CAP`", text)
        self.assertIn("`START_BALANCE`", text)
        self.assertIn("Simulated money", text)

    def test_a_strategy_that_never_bet_still_gets_a_readable_report(self):
        """Some strategies never find a trade they like. Their report has no tables, and that
        is the right answer rather than a crash or an empty file."""
        idle = [name for name, a in self.trader.accounts.items()
                if a.bankruptcies and not self.staked.get(name)]
        if not idle:
            self.skipTest("every strategy traded in this scenario")
        text = self._report_for(idle[0])
        self.assertIn("nothing was ever staked", text)
        self.assertIn("## What it was running", text)
        self.assertNotIn("| coin |", text)

    def test_a_bankrupt_event_writes_no_trade_row(self):
        """It has no ticker or side, so a CSV row for it would be malformed."""
        rows = list(csv.DictReader(io.open(self.trader.csv_path, newline="",
                                          encoding="utf-8")))
        self.assertTrue(rows)
        self.assertNotIn("BANKRUPT", {r["event"] for r in rows})


class Revival(unittest.TestCase):

    def test_it_is_staked_again_with_a_clean_slate(self):
        acct = kt.Account(dict(kt.STRATEGIES[0]))
        acct.cash = 0.10
        acct.bets, acct.wins, acct.losses = 40, 12, 28
        acct.realized_pnl = -999.9
        acct.log = [{"id": 1, "status": "lost", "cost": 5.0, "ticker": "X", "side": "UP",
                     "contracts": 5, "price": 0.5, "close": 1.0, "coin": "BTC"}]
        acct.revive()
        self.assertEqual(acct.cash, kt.START_BALANCE)
        self.assertEqual(acct.log, [])
        self.assertEqual((acct.bets, acct.wins, acct.losses), (0, 0, 0))
        self.assertEqual(acct.realized_pnl, 0.0)
        self.assertEqual(acct.participated(), 0)
        self.assertEqual(acct.bankruptcies, 1, "the death was not counted")

    def test_lives_survive_a_save_and_reload(self):
        """Otherwise a restart would hide that a strategy has already died twice."""
        t = h.new_trader(quiet=False)
        t.accounts["Late"].bankruptcies = 3
        t.save(force=True)
        reloaded = kt.KalshiTrader(os.path.dirname(t.json_path), {"BTC": {}})
        self.assertEqual(reloaded.accounts["Late"].bankruptcies, 3)

    def test_a_revived_strategy_keeps_trading(self):
        t = h.new_trader(quiet=False)
        acct = t.accounts["Value"]
        acct.revive()
        h.run_round(t, "BTC", [80000.0 * (1 + i * 3e-5) for i in range(300)],
                    h.market(strike=80000.0, yes_bid=0.30, yes_ask=0.32))
        self.assertGreater(acct.bets, 0, "a restaked strategy stopped betting")
        self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)


class ResetToStart(unittest.TestCase):

    def test_reset_returns_every_account_to_the_new_bank(self):
        t = h.new_trader(quiet=False)
        for a in t.accounts.values():
            a.cash = 12.34
        t.rounds_monitored = 99
        t.reset()
        for name, a in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertEqual(a.cash, 1000.0)
                self.assertEqual(a.log, [])
        self.assertEqual(t.rounds_monitored, 0)

    def test_reset_keeps_the_old_files_rather_than_deleting_them(self):
        t = h.new_trader(quiet=False)
        folder = os.path.dirname(t.json_path)
        io.open(t.bankrupt_path, "w", encoding="utf-8").write("old line\n")
        t.save(force=True)
        t.reset()
        kept = [f for f in os.listdir(folder) if "_old_" in f]
        self.assertTrue(any("bankruptcies" in f for f in kept),
                        "the bankruptcy log was lost on reset instead of being kept")


if __name__ == "__main__":
    unittest.main(verbosity=2)

"""The money rules: exits, position limits, and the invariant that cash always adds up."""

import math
import unittest

import helpers as h
import kalshi_trader as kt


class CashInvariant(unittest.TestCase):
    """Whatever else happens, cash equals start - costs + payouts. If this breaks, every
    balance the app has ever shown is wrong, so it is checked after every scenario."""

    def test_five_coins_one_balance(self):
        coins = {c: {} for c in ("BTC", "ETH", "SOL", "XRP", "DOGE")}
        t = h.new_trader(coins)
        t._append_csv = lambda *a, **k: None
        levels = {"BTC": 80000.0, "ETH": 2600.0, "SOL": 200.0, "XRP": 1.4, "DOGE": 0.09}
        for coin, px in levels.items():
            prices = [px * (1 + (k % 7) * 3e-5) for k in range(300)]
            h.run_round(t, coin, prices, h.market(coin, strike=px))
        for coin, px in levels.items():
            t.on_settled(f"KX{coin}15M-TEST", "yes", str(px * 1.001), 1_800_000_900.0, px)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertAlmostEqual(acct.cash, h.cash_balances(acct), places=2)

    def test_a_sale_never_pays_out_negative(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        prices = [80000.0 * (1 + i * 2e-5) for i in range(600)]
        h.run_round(t, "BTC", prices, h.market(strike=80000.0))
        for acct in t.accounts.values():
            for lot in acct.log:
                if lot["status"] == "sold":
                    self.assertGreaterEqual(lot["payout"], 0, lot)


class ProfitTaking(unittest.TestCase):
    """The Scalper's take_capture rule: bank a gain once the bid has covered enough of the
    way from entry to a dollar. Added because the value-based exit would hold a position
    that was well up, on the grounds that the model still thought it cheap."""

    def _run(self, capture):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        acct = t.accounts["Scalper"]
        acct.params = dict(acct.params)
        if capture is None:  # how it behaved before take_capture existed
            acct.params.pop("take_capture", None)
            acct.params.pop("min_hold", None)
        else:
            acct.params["take_capture"] = capture
        h.lagging_book_round(t)
        return acct

    def test_banks_gains_the_value_rule_would_have_held(self):
        before = self._run(None)
        after = self._run(0.80)
        gains_before = [l for l in before.log if l["status"] == "sold" and l["pnl"] > 0]
        gains_after = [l for l in after.log if l["status"] == "sold" and l["pnl"] > 0]
        self.assertEqual(len(gains_before), 0,
                         "the value rule sold on its own; this scenario no longer isolates "
                         "take_capture")
        self.assertGreater(len(gains_after), 10)
        self.assertAlmostEqual(after.cash, h.cash_balances(after), places=2)

    def test_cannot_bank_a_loss(self):
        """proceeds is net of the selling fee and slippage and cost is net of the buying
        fee, so a capture exit is a real gain rather than a paper one."""
        acct = self._run(0.80)
        for lot in acct.log:
            if lot.get("why") == "capture":
                self.assertGreater(lot["payout"], lot["cost"], lot)

    def test_other_strategies_are_untouched(self):
        opted_in = [s["name"] for s in kt.STRATEGIES if "take_capture" in s]
        self.assertEqual(opted_in, ["Scalper"])


class ExposureCap(unittest.TestCase):
    """Total exposure is capped in dollars, at half the STARTING balance. As a share of
    current equity it would raise the ceiling on its own stakes during a winning run, so a
    bad round costs more the better things have been going."""

    def test_ceiling_does_not_move_with_the_balance(self):
        acct = kt.Account(dict(kt.STRATEGIES[0]))
        for balance in (150.0, 300.0, 600.0, 1500.0, 10000.0):
            acct.cash, acct.log = balance, []
            room = kt.TOTAL_CAP * kt.START_BALANCE - acct.committed()
            with self.subTest(balance=balance):
                self.assertAlmostEqual(room, 75.0, places=9)

    def test_binds_with_a_rich_account_and_every_coin_live(self):
        coins = {c: {} for c in ("BTC", "ETH", "SOL", "XRP", "DOGE")}
        t = h.new_trader(coins)
        t._append_csv = lambda *a, **k: None
        for acct in t.accounts.values():
            acct.cash = 2000.0  # a very good week
        peak = {}
        levels = {"BTC": 81000.0, "ETH": 2637.0, "SOL": 110.36, "XRP": 1.4096,
                  "DOGE": 0.0875}
        for coin, px in levels.items():
            mkt = h.market(coin, f"KX{coin}15M-CAP", px * 0.9993, yes_bid=0.30,
                           yes_ask=0.32)
            h.run_round(t, coin, [px * (1 + (k % 5) * 2e-5) for k in range(400)], mkt)
            for name, acct in t.accounts.items():
                peak[name] = max(peak.get(name, 0.0), acct.committed())
        self.assertTrue(any(v > 50 for v in peak.values()),
                        "nothing came near the cap, so this proves nothing")
        for name, committed in peak.items():
            with self.subTest(strategy=name):
                self.assertLessEqual(committed, kt.TOTAL_CAP * kt.START_BALANCE + 1e-6)


class RoundCounting(unittest.TestCase):
    """A round is one 15-minute window across every coin. Betting on three coins in the
    same quarter hour is one round, not three: they all settle together."""

    def test_five_coins_two_windows_counts_two_rounds(self):
        coins = {c: {} for c in ("BTC", "ETH", "SOL", "XRP", "DOGE")}
        t = h.new_trader(coins)
        t._append_csv = lambda *a, **k: None
        base = 1_800_000_000.0
        levels = {"BTC": 80000.0, "ETH": 2600.0, "SOL": 200.0, "XRP": 1.4, "DOGE": 0.09}
        for window in range(2):
            close = base + (window + 1) * 900
            for coin, px in levels.items():
                for k in range(150):
                    t.observe(coin, px * (1 + (k % 3) * 1e-5), close - 1050 + k)
            for step in range(0, 600, 30):
                for coin, px in levels.items():
                    t.step(coin, h.market(coin, f"KX{coin}15M-W{window}", px, close,
                                          0.06, 0.07), px, close - 900 + step)
        self.assertEqual(t.rounds_monitored, 2)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertEqual(acct.participated(),
                                 len({lot["close"] for lot in acct.log}))
                self.assertLessEqual(acct.participated(), 2)

    def test_old_save_files_are_migrated(self):
        """Files written before the fix counted each coin separately. They have no
        last_round_close, which is how they are recognised."""
        coins = {c: {} for c in ("BTC", "ETH", "SOL", "XRP", "DOGE")}
        t = h.new_trader(coins)
        t.rounds_monitored = 50  # ten windows, counted five times over
        t._round_close = None
        t.save = kt.KalshiTrader.save.__get__(t)
        t.save(force=True)
        import json
        with open(t.json_path) as f:
            d = json.load(f)
        d.pop("last_round_close", None)
        with open(t.json_path, "w") as f:
            json.dump(d, f)
        reloaded = kt.KalshiTrader(t.json_path.rsplit("\\", 1)[0].rsplit("/", 1)[0], coins)
        self.assertEqual(reloaded.rounds_monitored, 10)


if __name__ == "__main__":
    unittest.main(verbosity=2)

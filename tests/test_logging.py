"""The four files the engine writes. These exist so the logs can be trusted as evidence:
a column that silently goes wrong is worse than one that is missing, because it gets used.
"""

import csv
import json
import os
import unittest

import helpers as h
import kalshi_trader as kt


def rows(path):
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


class TradeLog(unittest.TestCase):

    def setUp(self):
        self.t = h.new_trader()
        prices = [80000.0 * (1 + i * 3e-5) for i in range(600)]
        h.run_round(self.t, "BTC", prices, h.market(strike=80000.0, yes_bid=0.30,
                                                   yes_ask=0.32))
        self.t.on_settled("KXBTC15M-TEST", "yes", "80,018.00", 1_800_000_900.0, 80018.0)

    def test_header_matches_the_field_list(self):
        self.assertEqual(list(rows(self.t.csv_path)[0]), kt.CSV_FIELDS)

    def test_every_row_says_why_it_happened(self):
        for row in rows(self.t.csv_path):
            with self.subTest(event=row["event"]):
                if row["event"] == "BET":
                    self.assertEqual(row["why"], "entry")
                elif row["event"] == "SOLD":
                    self.assertIn(row["why"], ("value", "capture"))

    def test_rows_carry_the_book_and_the_clock(self):
        """A decision should be replayable from its own row, without cross-referencing."""
        for row in rows(self.t.csv_path):
            if row["event"] in ("BET", "SOLD"):
                self.assertNotEqual(row["tau"], "")
                self.assertNotEqual(row["yes_bid"], "")
                self.assertNotEqual(row["yes_ask"], "")

    def test_a_grown_header_moves_the_old_file_aside(self):
        """Appending 25-column rows under a 20-column header would corrupt the file for
        anything that reads it."""
        stale = self.t.csv_path
        with open(stale, "w", newline="") as f:
            f.write("time,strategy,coin,event\n2026-01-01T00:00:00,Value,BTC,BET\n")
        self.t._append(stale, kt.CSV_FIELDS, {k: "" for k in kt.CSV_FIELDS})
        self.assertEqual(list(rows(stale)[0]), kt.CSV_FIELDS)
        moved = [f for f in os.listdir(os.path.dirname(stale)) if "_cols_" in f]
        self.assertEqual(len(moved), 1, "the old file was overwritten instead of kept")


class ExitLog(unittest.TestCase):
    """One row per early sale, written at settlement -- the only moment the counterfactual
    exists, since a sold lot never reaches settlement itself."""

    def _traded_round(self):
        t = h.lagging_book_round(h.new_trader())
        t.on_settled("KXBTC15M-LAG", "yes", "80,200.00", 1_800_000_900.0, 80200.0)
        self.assertTrue(os.path.exists(t.exit_path),
                        "no early sales happened, so this scenario tests nothing")
        return t

    def test_gave_up_is_what_holding_would_have_paid(self):
        t = self._traded_round()
        self.assertGreater(len(rows(t.exit_path)), 5)
        for row in rows(t.exit_path):
            with self.subTest(ticker=row["ticker"]):
                won = (row["result"] == "yes") == (row["side"] == "UP")
                expected = float(row["contracts"]) if won else 0.0
                self.assertAlmostEqual(float(row["held_would_pay"]), expected, places=2)
                self.assertAlmostEqual(float(row["gave_up"]),
                                       expected - float(row["sold_for"]), places=2)

    def test_a_sale_is_graded_exactly_once(self):
        """Settlement can be seen more than once; double grading would double-count the
        evidence the exit rule is tuned on."""
        t = self._traded_round()
        before = len(rows(t.exit_path))
        for _ in range(3):
            t.on_settled("KXBTC15M-LAG", "yes", "80,200.00", 1_800_000_900.0, 80200.0)
        self.assertEqual(len(rows(t.exit_path)), before)


class RoundLog(unittest.TestCase):

    CLOSE = 1_800_000_900.0

    def test_rounds_nobody_bet_on_are_still_written(self):
        """A round with no bets is where "why didn't it trade?" is answered, so it has to
        be in the file."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        # A book a metre wide: both sides ask 99c, outside every strategy's price band, so
        # none of them will touch it. Thin books really do look like this.
        h.run_round(t, "BTC", [80000.0] * 200,
                    h.market(strike=80000.0, yes_bid=0.01, yes_ask=0.99))
        t.on_settled("KXBTC15M-TEST", "yes", "80,000.50", self.CLOSE, 80000.5)
        row = rows(t.round_path)[0]
        self.assertEqual(list(row), kt.ROUND_FIELDS)
        self.assertEqual(row["bets"], "0")
        self.assertNotEqual(row["ticks"], "0")

    def test_spread_comes_from_the_live_book_not_the_bell(self):
        """In the final seconds the asks vanish and the bid runs to a dollar, so a spread
        read off the closing book is negative nonsense."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        for k in range(890):
            ts = self.CLOSE - 890 + k
            px = 80010.0 + (k % 11)
            live = self.CLOSE - ts > 60
            mkt = h.market("BTC", "KXBTC15M-BELL", 80000.0, self.CLOSE,
                           0.58 if live else 0.999, 0.60 if live else 0.0)
            t.observe("BTC", px, ts)
            t.step("BTC", mkt, px, ts)
        t.on_settled("KXBTC15M-BELL", "yes", "80,015.00", self.CLOSE, 80015.0)
        row = rows(t.round_path)[0]
        self.assertAlmostEqual(float(row["spread"]), 0.02, places=6)
        self.assertGreater(float(row["last_price"]), 80000,
                           "the closing price must keep updating even as the book freezes")

    def test_cheap_coins_keep_their_precision(self):
        """Rounding every coin to four decimals flattened DOGE's whole move."""
        assets = h.app_value("ASSETS")
        t = h.new_trader(assets)
        t._append_csv = lambda *a, **k: None
        prices = [0.087431 + (k % 5) * 0.000001 for k in range(400)]
        h.run_round(t, "DOGE", prices,
                    h.market("DOGE", "KXDOGE15M-DEC", 0.087416, self.CLOSE, 0.55, 0.57))
        t.on_settled("KXDOGE15M-DEC", "yes", "0.0874578", self.CLOSE, 0.087435)
        row = rows(t.round_path)[0]
        last = float(row["last_price"])
        self.assertNotEqual(last, round(last, 4),
                            f"last_price {last} was flattened to four decimals")
        self.assertLess(abs(float(row["index_gap_pct"])), 1.0)


class RestartSurvival(unittest.TestCase):

    CLOSE = 1_800_000_900.0

    def test_a_round_open_at_shutdown_still_gets_logged(self):
        t = h.new_trader(quiet=False)
        folder = os.path.dirname(t.json_path)
        for k in range(300):
            ts = self.CLOSE - 600 + k
            t.observe("BTC", 79995.0 + (k % 7), ts)
            t.step("BTC", h.market("BTC", "KXBTC15M-RESTART", 80000.0, self.CLOSE),
                   79995.0 + (k % 7), ts)
        t.save(force=True)
        self.assertIn("KXBTC15M-RESTART", t._rounds)

        reopened = kt.KalshiTrader(folder, {"BTC": {}})
        self.assertIn("KXBTC15M-RESTART", reopened._rounds,
                      "the open round was lost across the restart")
        reopened.on_settled("KXBTC15M-RESTART", "no", "79,988.10", self.CLOSE, 79990.0)
        row = rows(reopened.round_path)[0]
        self.assertGreater(int(row["ticks"]), 0)
        self.assertNotEqual(row["index_gap_pct"], "")

    def test_a_stale_round_is_dropped_rather_than_carried_forward(self):
        t = h.new_trader(quiet=False)
        folder = os.path.dirname(t.json_path)
        t.save(force=True)
        with open(t.json_path) as f:
            d = json.load(f)
        d["live_rounds"] = {"KXBTC15M-ANCIENT": {"coin": "BTC", "close": 1.0,
                                                 "strike": 1.0, "ticks": 1,
                                                 "last_price": 1.0, "sigma2": 1e-9,
                                                 "offset_pct": 0.0, "yes_bid": 0.1,
                                                 "yes_ask": 0.2}}
        with open(t.json_path, "w") as f:
            json.dump(d, f)
        self.assertFalse(kt.KalshiTrader(folder, {"BTC": {}})._rounds)


if __name__ == "__main__":
    unittest.main(verbosity=2)

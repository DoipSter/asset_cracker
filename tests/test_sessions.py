"""Recording which run each row came from.

Without this the logs are one stream: close the app, reopen it, change the starting bank,
and the rows continue with nothing marking where one era ended. These check that the mark is
there, that it is stable within a run, and that the index of runs stays truthful.
"""

import csv
import os
import unittest

import helpers as h
import kalshi_trader as kt


def rows(path):
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


class EveryRowIsStamped(unittest.TestCase):

    def setUp(self):
        self.t = h.new_trader(quiet=False)
        h.lagging_book_round(self.t)
        self.t.on_settled("KXBTC15M-LAG", "yes", "80,200.00", 1_800_000_900.0, 80200.0)

    def test_the_session_column_comes_first_in_every_file(self):
        for fields in (kt.CSV_FIELDS, kt.EXIT_FIELDS, kt.ROUND_FIELDS):
            with self.subTest(file=fields[1]):
                self.assertEqual(fields[0], "session")

    def test_trades_carry_the_run(self):
        written = rows(self.t.csv_path)
        self.assertTrue(written)
        for r in written:
            self.assertEqual(r["session"], self.t.session)

    def test_rounds_carry_the_run(self):
        written = rows(self.t.round_path)
        self.assertTrue(written)
        for r in written:
            self.assertEqual(r["session"], self.t.session)

    def test_exits_carry_the_run(self):
        if not os.path.exists(self.t.exit_path):
            self.skipTest("no early sales to grade in this scenario")
        for r in rows(self.t.exit_path):
            self.assertEqual(r["session"], self.t.session)

    def test_the_stamp_does_not_drift_during_a_run(self):
        first = self.t.session
        h.lagging_book_round(self.t, ticker="KXBTC15M-LAG2")
        self.assertEqual(self.t.session, first)
        self.assertEqual({r["session"] for r in rows(self.t.csv_path)}, {first})


class TheIndexOfRuns(unittest.TestCase):

    def test_a_run_appears_once_and_is_kept_current(self):
        """Rewritten rather than appended: a run's length is only known as it goes, so an
        appended row would be stale from the moment it was written."""
        t = h.new_trader(quiet=False)
        t.save(force=True)
        first = rows(t.sessions_path)
        self.assertEqual(len(first), 1)
        self.assertEqual(first[0]["session"], t.session)

        h.lagging_book_round(t)
        t.save(force=True)
        second = rows(t.sessions_path)
        self.assertEqual(len(second), 1, "the run was appended again instead of updated")
        self.assertGreater(int(second[0]["bets"]), 0)

    def test_a_second_run_in_the_same_folder_adds_a_line(self):
        t = h.new_trader(quiet=False)
        t.save(force=True)
        folder = os.path.dirname(t.json_path)

        again = kt.KalshiTrader(folder, {"BTC": {}})
        again.session = "29990101_000000"  # a distinct run, deterministically
        again.save(force=True)
        listed = {r["session"] for r in rows(again.sessions_path)}
        self.assertEqual(listed, {t.session, "29990101_000000"})

    def test_it_records_what_the_run_was_configured_with(self):
        """The bank and the cap changed mid-project. Without them on the row, an old
        session's numbers cannot be read correctly."""
        t = h.new_trader(quiet=False)
        t.save(force=True)
        row = rows(t.sessions_path)[0]
        self.assertEqual(float(row["bank"]), kt.START_BALANCE)
        self.assertAlmostEqual(float(row["round_cap"]),
                               kt.TOTAL_CAP * kt.START_BALANCE, places=2)
        self.assertEqual(int(row["strategies"]), len(kt.ALL_STRATEGIES))
        self.assertIn("BTC", row["coins"])

    def test_the_summary_matches_the_accounts(self):
        t = h.new_trader(quiet=False)
        h.lagging_book_round(t)
        t.save(force=True)
        row = rows(t.sessions_path)[0]
        self.assertEqual(int(row["bets"]), t.session_bets)
        self.assertEqual(int(row["rounds"]), t.session_rounds)
        # on a first run the two agree, which is what makes the next test meaningful
        self.assertEqual(t.session_bets, sum(a.bets for a in t.accounts.values()))
        everyone = t.standings() + t.standings(anti=True)
        best = max(everyone, key=lambda r: r["equity"])
        self.assertEqual(row["best"], best["name"])
        self.assertAlmostEqual(float(row["best_balance"]), best["equity"], places=2)

    def test_the_counts_are_for_this_run_not_all_time(self):
        """Accounts persist across restarts. Reading their lifetime totals credited a
        one-minute session with 125 bets it had nothing to do with."""
        t = h.new_trader(quiet=False)
        h.lagging_book_round(t)
        t.save(force=True)
        folder = os.path.dirname(t.json_path)
        lifetime = sum(a.bets for a in t.accounts.values())
        self.assertGreater(lifetime, 0, "nothing traded, so this proves nothing")

        reopened = kt.KalshiTrader(folder, {"BTC": {}})
        reopened.session = "29990101_000000"
        reopened.save(force=True)
        fresh = [r for r in rows(reopened.sessions_path)
                 if r["session"] == "29990101_000000"][0]
        self.assertEqual(sum(a.bets for a in reopened.accounts.values()), lifetime,
                         "the accounts did not carry over, so this tests nothing")
        self.assertEqual(int(fresh["bets"]), 0,
                         "a fresh run inherited the previous run's bet count")
        self.assertEqual(int(fresh["rounds"]), 0)

    def test_the_index_covers_both_worlds(self):
        t = h.new_trader(quiet=False)
        t.save(force=True)
        self.assertEqual(int(rows(t.sessions_path)[0]["strategies"]),
                         len(kt.ALL_STRATEGIES))


class ResetStartsANewRun(unittest.TestCase):

    def test_two_resets_in_one_second_still_get_different_ids(self):
        """The id is a timestamp to the second, and a reset can easily land inside the same
        second as the run it replaces."""
        ids = [kt.session_id()]
        for _ in range(4):
            ids.append(kt.session_id(ids[-1]))
        self.assertEqual(len(set(ids)), len(ids), f"ids collided: {ids}")

    def test_resetting_begins_a_new_session(self):
        """Rows already written keep the old id, so they stay findable rather than blurring
        into what comes after."""
        t = h.new_trader(quiet=False)
        h.lagging_book_round(t)
        before = t.session
        written = {r["session"] for r in rows(t.csv_path)}
        self.assertEqual(written, {before})

        t.reset()
        self.assertNotEqual(t.session, before, "reset reused the finished run's id")

        kept = [f for f in os.listdir(os.path.dirname(t.json_path))
                if "kalshi_trades_old_" in f]
        self.assertTrue(kept, "the finished run's trades were deleted rather than kept")


if __name__ == "__main__":
    unittest.main(verbosity=2)

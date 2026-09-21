"""Surviving an unexpected shutdown.

This machine crashes. After one such crash kalshi_sessions.csv came back as 826 bytes of NUL:
NTFS had recorded the file's new size but its contents never reached the disk. Anything
rewritten whole is exposed to that, and the balances are the ones that would actually hurt.

Append-only files are not at risk in the same way, since appending cannot damage rows already
written -- at worst the last line is short.
"""

import csv
import json
import os
import unittest

import helpers as h
import kalshi_trader as kt


class WholeFileWritesAreAtomic(unittest.TestCase):
    """Write beside the target, flush it to the platter, then rename over. A reader then sees
    either the old file or the new one, never a half-written one."""

    def setUp(self):
        self.t = h.new_trader(quiet=False)

    def test_the_balances_are_not_truncated_in_place(self):
        """open(path, "w") truncates first, so a crash between the truncate and the flush
        leaves nothing at all."""
        src = open(kt.__file__, encoding="utf-8").read()
        body = src[src.index("    def save(self, force=False):"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertIn("_atomic_write", body)
        self.assertNotIn('open(self.json_path, "w"', body)

    def test_the_session_index_is_not_truncated_in_place(self):
        src = open(kt.__file__, encoding="utf-8").read()
        body = src[src.index("    def _write_session_row(self):"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertIn("_atomic_write", body)
        self.assertNotIn('open(self.sessions_path, "w"', body)

    def test_the_helper_flushes_before_renaming(self):
        """Without the fsync the rename can reach the disk before the data it renames, which
        is the whole failure being guarded against."""
        src = open(kt.__file__, encoding="utf-8").read()
        body = src[src.index("    def _atomic_write(self, path, write):"):]
        body = body[:body.index("\n    def ", 1)]
        # the docstring names both calls while explaining them, so read the code alone
        code = body[body.index('"""', body.index('"""') + 3) + 3:]
        self.assertIn("os.fsync", code)
        self.assertIn("os.replace", code)
        self.assertLess(code.index("os.fsync"), code.index("os.replace"),
                        "the rename happens before the data is flushed")

    def test_it_writes_what_it_was_asked_to(self):
        path = os.path.join(self.t.folder, "probe.txt")
        self.t._atomic_write(path, lambda f: f.write("hello"))
        self.assertEqual(open(path, encoding="utf-8").read(), "hello")

    def test_it_leaves_no_temporary_file_behind(self):
        self.t.save(force=True)
        leftovers = [f for f in os.listdir(self.t.folder) if f.endswith(".tmp")]
        self.assertEqual(leftovers, [])

    def test_a_failed_write_leaves_the_previous_file_untouched(self):
        """If the new content cannot be produced, the old file is still the good one."""
        path = os.path.join(self.t.folder, "probe.txt")
        self.t._atomic_write(path, lambda f: f.write("first"))

        def explode(f):
            f.write("partial")
            raise OSError("disk full")

        self.t._atomic_write(path, explode)
        self.assertEqual(open(path, encoding="utf-8").read(), "first")
        self.assertEqual([f for f in os.listdir(self.t.folder) if f.endswith(".tmp")], [])

    def test_the_balances_survive_a_rewrite(self):
        h.lagging_book_round(self.t)
        self.t.save(force=True)
        saved = json.load(open(self.t.json_path, encoding="utf-8"))
        self.assertEqual(len(saved["accounts"]), len(self.t.accounts))
        for name, acct in self.t.accounts.items():
            with self.subTest(strategy=name):
                self.assertAlmostEqual(saved["accounts"][name]["cash"], acct.cash, places=2)


class CorruptFilesAreSetAsideNotAppendedTo(unittest.TestCase):
    """What actually happened: the index came back as NUL bytes, and the app noticed rather
    than writing good rows into a broken file."""

    def test_a_nulled_index_is_rotated_and_rebuilt(self):
        t = h.new_trader(quiet=False)
        t.save(force=True)
        self.assertTrue(os.path.exists(t.sessions_path))

        with open(t.sessions_path, "wb") as f:  # exactly how the crash left it
            f.write(b"\x00" * 826)

        again = kt.KalshiTrader(t.folder, {"BTC": {}})
        again.save(force=True)
        rows = list(csv.DictReader(open(again.sessions_path, newline="", encoding="utf-8")))
        self.assertTrue(rows, "the index was not rebuilt")
        self.assertEqual(list(rows[0]), kt.SESSION_FIELDS)
        rotated = [f for f in os.listdir(t.folder) if "kalshi_sessions_cols_" in f]
        self.assertTrue(rotated, "the corrupt file was overwritten instead of kept")

    def test_a_nulled_trade_log_does_not_swallow_new_rows(self):
        t = h.new_trader(quiet=False)
        h.lagging_book_round(t)
        self.assertTrue(os.path.exists(t.csv_path))
        with open(t.csv_path, "wb") as f:
            f.write(b"\x00" * 400)

        again = kt.KalshiTrader(t.folder, {"BTC": {}})
        h.lagging_book_round(again, ticker="KXBTC15M-AFTER")
        rows = list(csv.DictReader(open(again.csv_path, newline="", encoding="utf-8")))
        self.assertTrue(rows, "no rows were written after the corruption")
        self.assertEqual(list(rows[0]), kt.CSV_FIELDS)


class AppendOnlyFilesKeepTheirHistory(unittest.TestCase):
    """The trade, exit and round logs are appended to, which is why five hours of overnight
    trading survived the crash that emptied the index."""

    def test_appending_never_rewrites_what_is_already_there(self):
        src = open(kt.__file__, encoding="utf-8").read()
        body = src[src.index("    def _append(self, path, fields, row):"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertIn('open(path, "a"', body)
        self.assertNotIn('open(path, "w"', body)

    def test_earlier_rows_are_still_there_after_a_restart(self):
        t = h.new_trader(quiet=False)
        h.lagging_book_round(t)
        first = len(list(csv.DictReader(open(t.csv_path, newline="", encoding="utf-8"))))
        self.assertGreater(first, 0)

        again = kt.KalshiTrader(t.folder, {"BTC": {}})
        h.lagging_book_round(again, ticker="KXBTC15M-SECOND")
        rows = list(csv.DictReader(open(again.csv_path, newline="", encoding="utf-8")))
        self.assertGreater(len(rows), first, "the restart did not add rows")
        self.assertEqual(len({r["session"] for r in rows}), 2,
                         "both runs should be present and distinguishable")


if __name__ == "__main__":
    unittest.main(verbosity=2)

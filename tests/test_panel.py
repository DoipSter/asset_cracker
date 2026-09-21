"""The side panels' geometry.

They were square at 360 until nine strategies a world had to fit into a leaderboard that
shares its height between the rows: 234px over nine gave each 26 and the rows became a smear.
The panel is taller now, and what these check is that adding another strategy cannot quietly
squash the rows again without failing here first.
"""

import unittest

import helpers as h
import kalshi_trader as kt


def panel_geometry(src):
    ns = {}
    for line in src.splitlines():
        if line.startswith(("SIDE = ", "SIDE_H = ", "LEDGER_T, LEDGER_B")):
            exec(line.split("#")[0].rstrip(), ns)
    return ns


class Size(unittest.TestCase):

    def setUp(self):
        self.src = h.app_source()
        self.g = panel_geometry(self.src)

    def test_height_is_separate_from_width(self):
        """Only the five places that used SIDE as a height moved; the rest are horizontal."""
        self.assertIn("SIDE_H", self.g)
        self.assertGreater(self.g["SIDE_H"], self.g["SIDE"])

    def test_the_shell_and_the_window_use_the_height(self):
        for needle in ("self.rrect(0, 0, SIDE, SIDE_H",
                       "height=self.px(SIDE_H)",
                       'f"{width}x{self.px(SIDE_H)}'):
            with self.subTest(needle=needle):
                self.assertIn(needle, self.src)

    def test_nothing_still_treats_the_panel_as_square(self):
        self.assertNotIn("SIDE, SIDE,", self.src)
        self.assertNotIn("width=s, height=s", self.src)


class LeaderboardRows(unittest.TestCase):
    """The rows share the panel's height. This is what "too crunched to read" was."""

    CAP = 47          # no row is taller than this, or three strategies get slabs
    READABLE = 34     # two lines of text plus breathing room

    def setUp(self):
        self.g = panel_geometry(h.app_source())
        self.space = self.g["LEDGER_B"] - self.g["LEDGER_T"]

    def pitch(self, strategies):
        return min(self.CAP, self.space // strategies)

    def test_todays_strategies_get_readable_rows(self):
        per_world = len(kt.STRATEGIES)
        pitch = self.pitch(per_world)
        self.assertGreaterEqual(pitch, self.READABLE,
                                f"{per_world} strategies get {pitch}px each")

    def test_nine_rows_read_exactly_as_six_used_to(self):
        """Six rows in the old 234px got 39. Nine should now get the same."""
        self.assertEqual(self.pitch(9), 234 // 6)

    def test_the_old_height_really_was_the_problem(self):
        """Nine rows in the old space got 26px, which is one line of text and no margin."""
        self.assertLess(234 // 9, self.READABLE)

    def test_there_is_headroom_for_another_strategy_or_two(self):
        for extra in (1, 2):
            with self.subTest(strategies=len(kt.STRATEGIES) + extra):
                self.assertGreaterEqual(self.pitch(len(kt.STRATEGIES) + extra), 29)

    def test_rows_never_grow_into_slabs(self):
        self.assertEqual(self.pitch(2), self.CAP)

    def test_the_footer_sits_below_the_rows(self):
        src = h.app_source()
        self.assertIn("LEDGER_B + 14", src, "the footer is still pinned to the old height")


class OtherTabsUseTheRoom(unittest.TestCase):
    """A taller panel that left a third of itself blank would be worse than a short one."""

    def setUp(self):
        self.src = h.app_source()
        self.g = panel_geometry(self.src)

    def test_the_log_shows_more_rows_than_it_did(self):
        rows = (self.g["SIDE_H"] - 30 - 14 - 118) // 24
        self.assertGreater(rows, 8, "the log still shows what fitted at 360")
        self.assertIn("LOG_ROWS = (SIDE_H", self.src)

    def test_the_last_log_row_clears_its_footer(self):
        rows = (self.g["SIDE_H"] - 30 - 14 - 118) // 24
        last_row_bottom = 96 + 22 + (rows - 1) * 24 + 12
        self.assertLess(last_row_bottom, self.g["SIDE_H"] - 34 - 11,
                        "the bottom log row runs into the footer")

    def test_the_account_buttons_moved_to_the_bottom_edge(self):
        self.assertIn("self.rrect(x1, SIDE_H - 48, x2, SIDE_H - 20", self.src)
        self.assertNotIn("self.rrect(x1, 318, x2, 346", self.src)

    def test_everything_stays_inside_the_shell(self):
        """The shell's inner edge is 6px in; nothing may be drawn past it."""
        self.assertLess(self.g["SIDE_H"] - 20, self.g["SIDE_H"] - 6)
        self.assertLess(self.g["LEDGER_B"] + 14, self.g["SIDE_H"] - 6)


if __name__ == "__main__":
    unittest.main(verbosity=2)

"""The phone's own controls: the two side tabs, and the buttons across the top.

Checked as source rather than by clicking, because these are canvas tags and a tag shared
between two drawn items is invisible until someone presses one and both handlers fire --
which is exactly the bug this file exists to stop coming back.
"""

import unittest

import helpers as h

W = 360  # the phone, in logical pixels

# Where the three top-row controls sit, and how wide their hit areas are.
BELL_X, WORLD_X, CLOSE_X = 42, 84, W - 46
HIT_RADIUS = 20
PILL_TOP, PILL_BOTTOM = 71, 97  # the coin page switch, one pill per coin
ISLAND = (W / 2 - 52, W / 2 + 52)  # the notch, which nothing may sit under


def block(src, marker, end="\n    def "):
    body = src[src.index(marker):]
    return body[:body.index(end, 1)]


class SideTabs(unittest.TestCase):
    """Right tab opens the strategies, left opens their anti-world twins.

    They were broken in one way that produced two symptoms: `tab` was both the group tag used
    to delete the pair and the click tag bound to the right-hand one. Pressing the LEFT tab
    matched `tab` as well as its own name and opened both panels; the right-hand tab carried
    `tab` twice, fired its handler twice, and shut itself again immediately.
    """

    def setUp(self):
        self.body = block(h.app_source(), "    def _draw_tab(self):")

    def test_each_tab_has_its_own_click_tag(self):
        self.assertIn('"tab_real"', self.body)
        self.assertIn('"tab_anti"', self.body)

    def test_the_group_tag_is_never_bound_to_a_click(self):
        self.assertIn('c.delete("tabs")', self.body)
        for bound in ('_button("tabs"', '_button("tab"'):
            with self.subTest(tag=bound):
                self.assertNotIn(bound, self.body)

    def test_a_tab_carries_the_group_tag_and_its_own_name_and_nothing_else(self):
        self.assertIn('tags = ("btn", "tabs", name)', self.body)

    def test_both_panels_are_reachable(self):
        self.assertIn("self.hub.toggle_panel(a)", self.body)
        self.assertIn("for anti in (False, True)", self.body)


class TopRow(unittest.TestCase):
    """Close on the right; the bell and the world toggle together on the left."""

    def setUp(self):
        self.src = h.app_source()

    @property
    def top_row_y(self):
        return h.app_value("TOP_ROW_Y", self.src)

    @property
    def pill_span(self):
        """The coin pills' x range, read from the app rather than copied, so the overlap
        check below fails if either the pills or the controls move."""
        line = [l for l in self.src.splitlines() if "left, right, gap = " in l][0]
        ns = {"W": W}
        exec(line.strip(), ns)
        return ns["left"], ns["right"], ns["gap"]

    def test_close_is_on_the_right(self):
        self.assertIn("cx, cy = W - 46, TOP_ROW_Y",
                      block(self.src, "        # All three controls sit", "self._button("))

    def test_the_bell_is_on_the_left(self):
        self.assertIn(f"cx, cy, sc = {BELL_X}, TOP_ROW_Y",
                      block(self.src, "    def _draw_bell_button"))

    def test_the_world_button_sits_beside_the_bell(self):
        body = block(self.src, "    def _draw_world_button")
        self.assertIn(f"cx, cy, r = {WORLD_X}, TOP_ROW_Y", body)
        self.assertIn("toggle_chart_world", body)

    def test_nothing_in_the_top_row_covers_a_coin_pill(self):
        """The world toggle used to sit at y 84, the same row as the pills, with its hit
        area from x 64 to 104 -- directly on top of the first coin."""
        bottom = self.top_row_y + HIT_RADIUS
        self.assertLess(bottom, PILL_TOP,
                        f"the top row reaches y {bottom}, into the coin pills at {PILL_TOP}")

    def test_the_pills_use_the_width_the_controls_freed(self):
        left, right, gap = self.pill_span
        wide = (right - left - gap * 4) / 5
        self.assertGreater(wide, 50, "five coins do not get a readable pill each")
        self.assertGreaterEqual(left, 20)
        self.assertLessEqual(right, W - 20)

    def test_nothing_sits_under_the_island(self):
        for name, x in (("bell", BELL_X), ("world", WORLD_X), ("close", CLOSE_X)):
            with self.subTest(button=name):
                self.assertFalse(ISLAND[0] < x < ISLAND[1],
                                 f"{name} is behind the island")

    def test_the_three_hit_areas_do_not_overlap(self):
        spots = [("bell", BELL_X), ("world", WORLD_X), ("close", CLOSE_X)]
        for (n1, x1), (n2, x2) in zip(spots, spots[1:]):
            with self.subTest(pair=f"{n1} vs {n2}"):
                self.assertGreaterEqual(abs(x2 - x1), 2 * HIT_RADIUS,
                                        f"{n1} and {n2} hit areas overlap")

    def test_they_all_fit_inside_the_phone(self):
        for name, x in (("bell", BELL_X), ("world", WORLD_X), ("close", CLOSE_X)):
            with self.subTest(button=name):
                self.assertGreaterEqual(x - HIT_RADIUS, 0)
                self.assertLessEqual(x + HIT_RADIUS, W)


class ChartWorld(unittest.TestCase):
    """The globe flips which world's bets the chart marks. It is deliberately separate from
    the panels: the graph is the one place the two worlds are otherwise indistinguishable."""

    def setUp(self):
        self.src = h.app_source()

    def test_the_chart_reads_the_selected_world(self):
        self.assertIn("self.trader.account(anti=self.hub.chart_anti).log", self.src,
                      "the chart reads one world regardless of the toggle")

    def test_toggling_redraws_the_button_and_the_chart(self):
        body = block(self.src, "    def toggle_chart_world(self):")
        self.assertIn("self.chart_anti = not self.chart_anti", body)
        self.assertIn("_draw_world_button()", body)
        self.assertIn("_chart_dirty = True", body)

    def test_it_starts_in_the_ordinary_world(self):
        self.assertIn("self.chart_anti = False", self.src)

    def test_the_toggle_does_not_move_the_panels(self):
        """Flipping the graph should not open, close or re-track a panel."""
        body = block(self.src, "    def toggle_chart_world(self):")
        for leak in ("toggle_panel", "select(", "panels["):
            with self.subTest(touched=leak):
                self.assertNotIn(leak, body)


if __name__ == "__main__":
    unittest.main(verbosity=2)

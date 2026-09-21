"""The live order book panel.

Coinbase's `level2` channel refuses an anonymous subscription -- tested, it answers "Failed to
subscribe". `level2_batch` carries the same data and does not, which is what the feed uses.

The panel's geometry is checked as numbers rather than by eye, the same way the price chart
is: it has to sit under the chart without touching it, and a cumulative staircase has to stay
inside its box whatever the book looks like.
"""

import unittest

import helpers as h

W = 360


class Layout(unittest.TestCase):
    """The chart gave up height for this. The two must not overlap, and neither may reach
    into the range pills below."""

    PILL_TOP = 596

    def setUp(self):
        """Read the layout out of the app rather than copying it, so moving the panel there
        moves these checks with it."""
        self.src = h.app_source()
        ns = {"W": W}
        for line in self.src.splitlines():
            if line.startswith(("PLOT_L, PLOT_R", "PLOT_FOOT =", "DEPTH_T, DEPTH_B")):
                exec(line, ns)
        self.plot_l, self.plot_r = ns["PLOT_L"], ns["PLOT_R"]
        self.plot_t, self.plot_b = ns["PLOT_T"], ns["PLOT_B"]
        self.plot_foot = ns["PLOT_FOOT"]
        self.depth_t, self.depth_b = ns["DEPTH_T"], ns["DEPTH_B"]

    def test_the_price_plot_sits_above_its_caption(self):
        self.assertLess(self.plot_b, self.plot_foot)

    def test_the_depth_panel_starts_below_the_caption(self):
        self.assertLess(self.plot_foot, self.depth_t,
                        "the depth panel overlaps the low/high caption")

    def test_the_panel_clears_the_range_pills(self):
        # the panel writes its price axis 12px under its floor
        self.assertLess(self.depth_b + 12, self.PILL_TOP,
                        "the depth panel reaches into the range pills")

    def test_both_boxes_are_tall_enough_to_read(self):
        self.assertGreater(self.plot_b - self.plot_t, 100, "the price chart is too squashed")
        self.assertGreater(self.depth_b - self.depth_t, 50, "the depth panel is a smear")

    def test_the_charts_use_the_shared_constants(self):
        """Three chart methods share the plot box; a stray literal would drift silently."""
        self.assertEqual(
            self.src.count("left, right, top, bottom = PLOT_L, PLOT_R, PLOT_T, PLOT_B"), 3)
        self.assertNotIn(", 552,", self.src, "a caption is still at the old chart height")


class Staircase(unittest.TestCase):
    """The cumulative walk, reimplemented here so a wrong one in the app cannot agree with
    itself."""

    MID, SPAN = 87_000.0, 87_000.0 * 0.004

    def walk(self, book, outward):
        levels = sorted((p for p in book if abs(p - self.MID) <= self.SPAN),
                        reverse=not outward)
        run, out = 0.0, []
        for price in levels:
            run += book[price]
            out.append((price, run))
        return out

    def test_totals_only_ever_climb_away_from_the_middle(self):
        asks = {87_000 + 10 * i: 1.5 for i in range(30)}
        steps = self.walk(asks, True)
        totals = [t for _, t in steps]
        self.assertEqual(totals, sorted(totals))
        self.assertAlmostEqual(totals[-1], sum(asks.values()), places=6)

    def test_bids_walk_downward_and_asks_upward(self):
        bids = {86_900.0: 2.0, 86_950.0: 3.0, 86_990.0: 1.0}
        asks = {87_010.0: 4.0, 87_100.0: 5.0}
        down = self.walk(bids, False)
        up = self.walk(asks, True)
        self.assertEqual([p for p, _ in down], [86_990.0, 86_950.0, 86_900.0])
        self.assertEqual([p for p, _ in up], [87_010.0, 87_100.0])

    def test_levels_beyond_the_span_are_left_out(self):
        asks = {87_010.0: 1.0, 88_000.0: 999.0}  # the second is far outside 0.4%
        up = self.walk(asks, True)
        self.assertEqual(len(up), 1)
        self.assertAlmostEqual(up[-1][1], 1.0, places=6)

    def test_the_staircase_stays_inside_its_box(self):
        src = h.app_source()
        line = [l for l in src.splitlines() if l.startswith("DEPTH_T, DEPTH_B")][0]
        ns = {}
        exec(line, ns)
        top, bottom = ns["DEPTH_T"], ns["DEPTH_B"]
        asks = {87_000 + 10 * i: 2.0 for i in range(35)}
        steps = self.walk(asks, True)
        peak = steps[-1][1]
        ys = [bottom - (bottom - top - 14) * (t / peak) for _, t in steps]
        self.assertGreaterEqual(min(ys), top, "the staircase escapes the top of the panel")
        self.assertLessEqual(max(ys), bottom)

    def test_an_empty_side_does_not_divide_by_zero(self):
        self.assertEqual(self.walk({}, True), [])


class PriceAxis(unittest.TestCase):
    """Ruled lines at round prices, so a wall reads as a number rather than a position."""

    def setUp(self):
        import math as _math
        self.src = h.app_source()
        ns = {"math": _math}
        exec(self.src[self.src.index("def nice_step("):self.src.index("def mix_color(")], ns)
        self.nice_step = ns["nice_step"]
        self.span_pct = h.app_value("DEPTH_SPAN_PCT", self.src)

    def ticks(self, spot):
        import math as _math
        span = spot * self.span_pct
        step = self.nice_step(2 * span)
        out, price = [], _math.ceil((spot - span) / step) * step
        while price <= spot + span:
            out.append(price)
            price += step
        return step, out

    def test_steps_are_round_numbers_people_read_prices_in(self):
        """1, 2 or 5 times a power of ten. Anything else lands on ticks like 37 or 64."""
        for spot in (86_800.0, 2_637.0, 110.36, 1.4096, 0.0875, 7.0, 43_210.0):
            step, _ = self.ticks(spot)
            scaled = step / 10 ** round(__import__("math").log10(step) - 0.5)
            with self.subTest(spot=spot, step=step):
                self.assertAlmostEqual(min((1, 2, 5, 10), key=lambda m: abs(m - scaled)),
                                       scaled, places=6)

    def test_bitcoin_gets_a_fifty_dollar_grid(self):
        step, ticks = self.ticks(86_800.0)
        self.assertEqual(step, 50.0)
        self.assertIn(86_750.0, ticks)
        self.assertIn(86_800.0, ticks)

    def test_the_grid_spans_the_whole_panel(self):
        for spot in (86_800.0, 2_637.0, 0.0875):
            span = spot * self.span_pct
            _, ticks = self.ticks(spot)
            with self.subTest(spot=spot):
                self.assertGreaterEqual(len(ticks), 6, "too few lines to read a level off")
                self.assertLessEqual(len(ticks), 20, "so many lines it is a smear")
                self.assertGreaterEqual(ticks[0], spot - span)
                self.assertLessEqual(ticks[-1], spot + span)

    def test_labels_alternate_rows_rather_than_overlapping(self):
        body = self.src[self.src.index("        labels = ["):]
        body = body[:body.index("c.create_line(*self.pts([X(mid)")]
        self.assertIn("DEPTH_B + (10 if", body)
        self.assertIn("font.measure", body, "label width is assumed rather than measured")

    def test_the_label_row_clears_the_range_pills(self):
        ns = {}
        for line in self.src.splitlines():
            if line.startswith("DEPTH_T, DEPTH_B"):
                exec(line, ns)
        self.assertLess(ns["DEPTH_B"] + 19 + 5, 596, "the lower label row hits the pills")


class Feed(unittest.TestCase):

    def setUp(self):
        self.src = h.app_source()
        self.body = self.src[self.src.index("def book_feed("):]
        self.body = self.body[:self.body.index("\nKALSHI_HOST")]

    def test_it_uses_the_channel_that_works_without_credentials(self):
        """`level2` answers "Failed to subscribe" anonymously; `level2_batch` does not."""
        self.assertIn('"level2_batch"', self.body)
        self.assertNotIn('"level2"]', self.body)

    def test_it_prunes_rather_than_keeping_forty_thousand_levels(self):
        self.assertIn("BOOK_PRUNE_PCT", self.body)
        self.assertLessEqual(h.app_value("BOOK_PRUNE_PCT", self.src), 0.05)

    def test_a_zero_size_removes_the_level(self):
        """That is how the protocol says a level is gone; treating it as a size would leave
        phantom walls on the panel forever."""
        self.assertIn("book.pop(price, None)", self.body)

    def test_it_throttles_before_handing_over(self):
        """The feed batches every ~50ms. Redrawing that often would swamp the canvas."""
        self.assertIn("last_sent", self.body)

    def test_it_runs_on_its_own_socket(self):
        """A 40,000-level snapshot sharing the trade feed's connection would stall prices."""
        self.assertIn("target=book_feed", self.src)

    def test_the_page_starts_with_an_empty_book_rather_than_none(self):
        self.assertIn("self.book = ({}, {})", self.src)


if __name__ == "__main__":
    unittest.main(verbosity=2)

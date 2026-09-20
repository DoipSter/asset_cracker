"""What the app puts on screen. None of this opens a window: the values and the geometry
are pulled out of the source and checked numerically, so these run on a headless machine.
"""

import math
import unittest

import helpers as h

W, H = 360, 720  # the phone, in logical pixels
PLOT = (46, W - 46, 322, 518)  # left, right, top, bottom of the chart area

# The precision Kalshi quotes each coin's strikes to, read off our own trade logs. A price
# shown to fewer decimals than this cannot show an ordinary fifteen-minute move.
KALSHI_PRECISION = {"BTC": 2, "ETH": 2, "SOL": 4, "XRP": 4, "DOGE": 6}
LIVE_PRICES = {"BTC": 81124.55, "ETH": 2636.96, "SOL": 110.3578, "XRP": 1.4076,
               "DOGE": 0.087416}


class Decimals(unittest.TestCase):

    def setUp(self):
        self.assets = h.app_value("ASSETS")

    def test_each_coin_matches_what_kalshi_quotes(self):
        for coin, want in KALSHI_PRECISION.items():
            with self.subTest(coin=coin):
                self.assertEqual(self.assets[coin]["decimals"], want)

    def test_a_price_survives_its_own_formatting(self):
        for coin, price in LIVE_PRICES.items():
            dec = self.assets[coin]["decimals"]
            with self.subTest(coin=coin):
                self.assertEqual(float(f"{price:.{dec}f}"), price)

    def test_an_ordinary_move_is_visible(self):
        """0.05% over fifteen minutes is unremarkable. At two decimals DOGE showed $0.09
        before and after, which is what prompted this."""
        for coin, price in LIVE_PRICES.items():
            dec = self.assets[coin]["decimals"]
            before = f"${price:,.{dec}f}"
            after = f"${price * 1.0005:,.{dec}f}"
            with self.subTest(coin=coin):
                self.assertNotEqual(before, after)

    def test_no_coin_price_is_formatted_to_a_hardcoded_two(self):
        """Regression: the headline, both price-to-beat rows and all three notifications
        were `:,.2f`, so the per-coin precision never reached them."""
        src = h.app_source()
        offenders = [line.strip() for line in src.splitlines()
                     if ":,.2f}" in line
                     and any(k in line for k in ("self.ptb", "shown", "price:", "strike']",
                                                 "final:"))]
        self.assertEqual(offenders, [], "coin prices still hardcoded to two decimals")


class MinuteChart(unittest.TestCase):
    """The 1M view. Coinbase's smallest candle is a minute, so this range is drawn from the
    per-second feed buffer instead; LIVE_RANGES marks it."""

    def setUp(self):
        src = h.app_source()
        self.ranges = h.app_value("RANGES", src)
        self.live = h.app_value("LIVE_RANGES", src)
        self.assets = h.app_value("ASSETS", src)

    def _plot(self, prices, coin="BTC"):
        """The chart's own scaling, lifted out so it can be checked as numbers."""
        left, right, top, bottom = PLOT
        lift = 1.000057
        levels = [p * lift for p in prices]
        lo, hi = min(levels), max(levels)
        pad = max((hi - lo) * 0.18, hi * self.assets[coin]["min_pad_pct"] * 0.5)
        lo, hi = lo - pad, hi + pad
        xs = [left + (right - left) * i / 60 for i in range(len(prices))]
        ys = [bottom - (bottom - top) * (p - lo) / (hi - lo) for p in levels]
        return xs, ys

    def test_it_is_drawn_from_the_feed_not_candles(self):
        self.assertIn("1M", self.ranges)
        self.assertIn("1M", self.live)

    def test_poll_history_skips_live_ranges(self):
        src = h.app_source()
        body = src[src.index("def _poll_history"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertIn("LIVE_RANGES", body,
                      "_poll_history would fetch candles for a range that has none")

    def test_the_pill_row_fits_the_phone(self):
        w, gap = 56, 6
        span = w * len(self.ranges) + gap * (len(self.ranges) - 1)
        self.assertLessEqual(span, W - 16)
        self.assertGreater((W - span) / 2, 0)

    def test_a_normal_minute_fills_the_chart(self):
        prices = [81000 + i * 0.6 + 4 * math.sin(i / 3) for i in range(60)]
        xs, ys = self._plot(prices)
        left, right, top, bottom = PLOT
        self.assertGreaterEqual(min(xs), left - 0.01)
        self.assertLessEqual(max(xs), right + 0.01)
        self.assertGreaterEqual(min(ys), top)
        self.assertLessEqual(max(ys), bottom)
        self.assertGreater(max(ys) - min(ys), (bottom - top) * 0.4,
                           "the line is squashed; the padding is too generous")

    def test_a_flat_minute_does_not_collapse_the_scale(self):
        xs, ys = self._plot([81000.0] * 60)
        left, right, top, bottom = PLOT
        self.assertAlmostEqual(sum(ys) / len(ys), (top + bottom) / 2, delta=1)

    def test_a_cheap_coin_still_shows_its_move(self):
        """DOGE moves in the sixth decimal; at a fixed scale it would be a flat line."""
        prices = [0.087416 + i * 0.0000004 for i in range(60)]
        xs, ys = self._plot(prices, coin="DOGE")
        self.assertGreater(max(ys) - min(ys), 1)


if __name__ == "__main__":
    unittest.main(verbosity=2)

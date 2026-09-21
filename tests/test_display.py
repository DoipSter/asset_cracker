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



def _luminance(hex_colour):
    """WCAG relative luminance, so contrast can be checked without a display."""
    def channel(c):
        c /= 255
        return c / 12.92 if c <= 0.03928 else ((c + 0.055) / 1.055) ** 2.4
    r, g, b = (channel(int(hex_colour[i:i + 2], 16)) for i in (1, 3, 5))
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def contrast(a, b):
    la, lb = _luminance(a), _luminance(b)
    return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)


def blend(base, top, amount):
    """The app's mix_color, reimplemented so a wrong one there cannot agree with itself."""
    pairs = [(int(base[i:i + 2], 16), int(top[i:i + 2], 16)) for i in (1, 3, 5)]
    return "#" + "".join(f"{round(x + (y - x) * amount):02x}" for x, y in pairs)


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


class Symbols(unittest.TestCase):
    """Each coin's sign, on its page button and over the chart.

    The characters are pinned here rather than merely checked for existence, because a
    codepoint the font lacks renders as a hollow box and no assertion about presence would
    notice. U+25CE BULLSEYE was the obvious pick for Solana and is a box on Windows 11;
    research/font_probe.py is what rejected it and what to run before adding a coin.
    """

    VERIFIED = {
        "BTC": "₿",   # bitcoin sign
        "ETH": "Ξ",   # greek capital xi
        "SOL": "≡",   # identical to -- three bars
        "XRP": "✕",   # multiplication x
        "DOGE": "Ð",  # latin capital eth
    }
    PILL_WIDTH = (W - 26 - 26 - 3 * 4) / 5

    def setUp(self):
        self.assets = h.app_value("ASSETS")

    def test_every_coin_has_one(self):
        for coin in self.assets:
            with self.subTest(coin=coin):
                self.assertIn("symbol", self.assets[coin])

    def test_they_are_the_glyphs_the_font_probe_cleared(self):
        for coin, want in self.VERIFIED.items():
            with self.subTest(coin=coin):
                self.assertEqual(self.assets[coin]["symbol"], want)

    def test_each_is_a_single_character(self):
        """Two-character labels would not fit the pill beside the ticker."""
        for coin, asset in self.assets.items():
            with self.subTest(coin=coin):
                self.assertEqual(len(asset["symbol"]), 1)

    def test_no_two_coins_share_a_symbol(self):
        seen = [a["symbol"] for a in self.assets.values()]
        self.assertEqual(len(set(seen)), len(seen))

    def test_symbol_and_ticker_fit_one_pill(self):
        """Measured without Tk: the widest label is DOGE's, about 37px of a 59px pill."""
        for coin, asset in self.assets.items():
            label = f"{asset['symbol']} {coin}"
            with self.subTest(coin=coin, label=label):
                self.assertLessEqual(len(label), 7,
                                     "too long to sit beside the ticker in a pill")
        self.assertGreater(self.PILL_WIDTH, 50)

    def test_the_pill_shows_the_symbol(self):
        self.assertIn('asset["symbol"], coin, 10, mark', h.app_source())

    def test_the_chart_caption_shows_the_symbol(self):
        self.assertIn('self.asset["symbol"], caption, 12', h.app_source())


class SymbolColours(unittest.TestCase):
    """Only the sign is coloured; the ticker keeps the colour everything else uses.

    Each sign appears on three backgrounds: BUTTON on an inactive pill, TEXT on an active one
    (which is filled light), and BG in the caption over the chart. One colour cannot serve a
    near-black and a near-white background, so a bright version and a dimmed one are both
    checked. 3.0 is the WCAG threshold for large or bold text.
    """

    BG, BUTTON, TEXT, MUTED = "#0B0F1A", "#232A3D", "#F2F5FA", "#8A93A8"
    FLOOR = 3.0

    def setUp(self):
        self.src = h.app_source()
        self.assets = h.app_value("ASSETS", self.src)
        self.on_light = h.app_value("SYMBOL_ON_LIGHT", self.src)

    def test_every_coin_has_a_colour(self):
        for coin, asset in self.assets.items():
            with self.subTest(coin=coin):
                self.assertRegex(asset["colour"], r"^#[0-9A-Fa-f]{6}$")

    def test_no_two_coins_share_a_colour(self):
        seen = [a["colour"] for a in self.assets.values()]
        self.assertEqual(len(set(seen)), len(seen))

    def test_bright_enough_on_an_inactive_pill_and_the_caption(self):
        for coin, asset in self.assets.items():
            for bg, name in ((self.BUTTON, "BUTTON"), (self.BG, "BG")):
                ratio = contrast(asset["colour"], bg)
                with self.subTest(coin=coin, background=name):
                    self.assertGreaterEqual(ratio, self.FLOOR, f"{ratio:.2f}:1")

    def test_dimmed_enough_on_an_active_pill(self):
        """An active pill is filled with TEXT, so the bright version would vanish on it."""
        for coin, asset in self.assets.items():
            dimmed = blend(asset["colour"], self.BG, self.on_light)
            ratio = contrast(dimmed, self.TEXT)
            with self.subTest(coin=coin):
                self.assertGreaterEqual(ratio, self.FLOOR, f"{ratio:.2f}:1")

    def test_the_dimming_is_not_so_heavy_the_hue_is_lost(self):
        """Blended all the way to BG it would be invisible rather than merely darker."""
        self.assertLess(self.on_light, 0.8)
        for coin, asset in self.assets.items():
            dimmed = blend(asset["colour"], self.BG, self.on_light)
            with self.subTest(coin=coin):
                self.assertNotEqual(dimmed.lower(), self.BG.lower())

    def test_a_colour_reads_as_coloured_beside_the_grey_ticker(self):
        """If a sign were as grey as its ticker, colouring it would be pointless.

        Measured as saturation, not as contrast. Contrast only compares brightness, and
        Ethereum's periwinkle is almost exactly as bright as MUTED while being obviously blue
        next to it -- a luminance test called that a failure, which says the test was wrong
        rather than the colour. Channel spread is the thing that separates a hue from a grey.
        """
        def spread(hex_colour):
            channels = [int(hex_colour[i:i + 2], 16) for i in (1, 3, 5)]
            return max(channels) - min(channels)

        grey = spread(self.MUTED)
        for coin, asset in self.assets.items():
            with self.subTest(coin=coin, spread=spread(asset["colour"]), grey=grey):
                self.assertGreater(spread(asset["colour"]), grey * 2)

    def test_the_symbol_and_the_ticker_are_drawn_separately(self):
        """A single canvas text item takes a single fill, so two are needed."""
        body = self.src[self.src.index("    def marked_text(self"):]
        body = body[:body.index("\n    def ", 1)]
        self.assertEqual(body.count("self.text("), 2)
        self.assertIn("mark", body)
        self.assertIn("fill", body)

    def test_the_pill_dims_the_mark_only_when_active(self):
        body = self.src[self.src.index("        coins = list(ASSETS)"):]
        body = body[:body.index("\n        #", 1)]
        self.assertIn("SYMBOL_ON_LIGHT", body)
        self.assertIn("if active", body)


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

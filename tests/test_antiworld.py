"""The anti-world: for every strategy, a twin that believes the opposite of the model.

The pair is the point. Both sides pay the spread and the fee on every trade, so if a
strategy and its mirror both lose, the losses are costs rather than bad judgement and no
amount of tuning the model will fix them. If a mirror wins, the original is systematically
wrong and worth inverting. Nothing else in the project separates those two explanations.
"""

import unittest

import helpers as h
import kalshi_trader as kt


class Pairing(unittest.TestCase):

    def test_every_strategy_has_exactly_one_twin(self):
        self.assertEqual(len(kt.ANTI_STRATEGIES), len(kt.STRATEGIES))
        self.assertEqual(len(kt.ALL_STRATEGIES),
                         len(kt.STRATEGIES) + len(kt.ANTI_STRATEGIES))
        for real, anti in zip(kt.STRATEGIES, kt.ANTI_STRATEGIES):
            with self.subTest(strategy=real["name"]):
                self.assertEqual(anti["name"], f"Anti {real['name']}")
                self.assertTrue(anti["anti"])
                self.assertTrue(anti["invert"])

    def test_a_twin_differs_only_in_its_belief(self):
        """If a twin picked up different caps or thresholds it would be a different
        strategy, and the comparison would mean nothing."""
        ignored = {"name", "blurb", "anti", "invert"}
        for real, anti in zip(kt.STRATEGIES, kt.ANTI_STRATEGIES):
            with self.subTest(strategy=real["name"]):
                self.assertEqual({k: v for k, v in real.items() if k not in ignored},
                                 {k: v for k, v in anti.items() if k not in ignored})

    def test_the_real_strategies_are_untouched(self):
        for real in kt.STRATEGIES:
            with self.subTest(strategy=real["name"]):
                self.assertNotIn("anti", real)
                self.assertNotIn("invert", real)


class OwnMoney(unittest.TestCase):

    def test_twelve_separate_banks(self):
        t = h.new_trader()
        self.assertEqual(len(t.accounts), 12)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertEqual(acct.cash, kt.START_BALANCE)

    def test_a_twin_spending_does_not_touch_its_original(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        h.lagging_book_round(t)
        for real in kt.STRATEGIES:
            anti = t.accounts[f"Anti {real['name']}"]
            with self.subTest(strategy=real["name"]):
                self.assertAlmostEqual(anti.cash, h.cash_balances(anti), places=2)
                self.assertAlmostEqual(t.accounts[real["name"]].cash,
                                       h.cash_balances(t.accounts[real["name"]]), places=2)

    def test_a_twin_is_capped_like_anything_else(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        for acct in t.accounts.values():
            acct.cash = kt.START_BALANCE * 9
        h.lagging_book_round(t)
        for name, acct in t.accounts.items():
            with self.subTest(strategy=name):
                self.assertLessEqual(acct.committed(),
                                     kt.TOTAL_CAP * kt.START_BALANCE + 1e-6)


class InvertedBelief(unittest.TestCase):
    """What "the opposite" actually means here: the twin's view of UP is the original's view
    reflected through the market's own price, at the same instant."""

    @staticmethod
    def _views(p_model, mid=0.40, shrink=0.5):
        """The two beliefs, and the neutral one they are reflected through.

        `neutral` is what the strategy would believe if the model said 50/50 -- the market's
        price pulled `shrink` of the way toward a coin flip. It is that, not the raw market
        mid, that the pair is symmetric about: a strategy with shrink below 1 keeps some of
        the market's opinion, and the twin keeps exactly the same amount of it.
        """
        real = mid + shrink * (p_model - mid)
        anti = mid + shrink * ((1 - p_model) - mid)
        return real, anti, mid + shrink * (0.5 - mid)

    def test_the_two_views_reflect_through_the_neutral_one(self):
        for p_model in (0.05, 0.25, 0.5, 0.75, 0.95):
            for mid in (0.2, 0.5, 0.8):
                real, anti, neutral = self._views(p_model, mid)
                with self.subTest(p_model=p_model, mid=mid):
                    self.assertAlmostEqual((real + anti) / 2, neutral, places=9)

    def test_disagreement_is_equal_and_opposite(self):
        for p_model in (0.1, 0.35, 0.9):
            real, anti, neutral = self._views(p_model, mid=0.4)
            with self.subTest(p_model=p_model):
                self.assertAlmostEqual(real - neutral, -(anti - neutral), places=9)

    def test_at_full_conviction_it_is_a_pure_mirror(self):
        """shrink 1.0 keeps none of the market's opinion, so the pair sums to one -- the
        strategies that trade this way (Model, Late, Favorite) are exact opposites."""
        for p_model in (0.05, 0.4, 0.8):
            real, anti, neutral = self._views(p_model, mid=0.3, shrink=1.0)
            with self.subTest(p_model=p_model):
                self.assertAlmostEqual(real + anti, 1.0, places=9)
                self.assertAlmostEqual(neutral, 0.5, places=9)

    def test_a_confident_model_makes_the_twin_confident_the_other_way(self):
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        seen = []
        original = kt.Account.step

        def spy(self, market, price, now, p_model, paused, ctx=None):
            if self.name in ("Model", "Anti Model"):  # shrink 1.0: a pure mirror
                used = 1 - p_model if self.params.get("invert") else p_model
                seen.append((self.name, round(p_model, 6), round(used, 6)))
            return original(self, market, price, now, p_model, paused, ctx)

        kt.Account.step = spy
        try:
            h.lagging_book_round(t)
        finally:
            kt.Account.step = original
        by_name = {}
        for name, raw, used in seen:
            by_name.setdefault((name, raw), used)
        pairs = [(raw, by_name[("Model", raw)], by_name[("Anti Model", raw)])
                 for (name, raw) in by_name if name == "Model"
                 and ("Anti Model", raw) in by_name]
        self.assertTrue(pairs, "the two never saw the same model probability")
        for raw, real, anti in pairs:
            self.assertAlmostEqual(real + anti, 1.0, places=6)


class Separation(unittest.TestCase):
    """The two worlds are shown in their own panels, so they are never ranked together."""

    def test_standings_never_mix_the_worlds(self):
        t = h.new_trader()
        real = {r["name"] for r in t.standings(anti=False)}
        anti = {r["name"] for r in t.standings(anti=True)}
        self.assertEqual(real, {p["name"] for p in kt.STRATEGIES})
        self.assertEqual(anti, {p["name"] for p in kt.ANTI_STRATEGIES})
        self.assertFalse(real & anti)

    def test_each_world_tracks_its_own_strategy(self):
        t = h.new_trader(quiet=False)
        t.select("Anti Late")
        self.assertEqual(t.tracked(anti=True), "Anti Late")
        self.assertEqual(t.tracked(anti=False), kt.STRATEGIES[0]["name"],
                         "selecting a twin moved the other world's selection")
        t.select("Favorite")
        self.assertEqual(t.tracked(anti=False), "Favorite")
        self.assertEqual(t.tracked(anti=True), "Anti Late")

    def test_both_selections_survive_a_restart(self):
        import os
        t = h.new_trader(quiet=False)
        t.select("Anti Scalper")
        t.select("Late")
        t.save(force=True)
        reloaded = kt.KalshiTrader(os.path.dirname(t.json_path), {"BTC": {}})
        self.assertEqual(reloaded.tracked(anti=True), "Anti Scalper")
        self.assertEqual(reloaded.tracked(anti=False), "Late")


class TheExperiment(unittest.TestCase):

    def test_a_twin_trades_rather_than_sitting_out(self):
        """A mirror that never bets would answer nothing."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        h.lagging_book_round(t)
        traded = [n for n, a in t.accounts.items()
                  if a.params.get("anti") and a.log]
        self.assertTrue(traded, "no twin placed a single bet")

    def test_the_lottery_twin_is_not_a_no_op(self):
        """The lottery path reads its probability from ctx, not from p_model, so inverting
        p_model did nothing for it: Anti Lottery traded identically to Lottery, a twin that
        silently answered nothing.

        Driven straight through step() rather than through a round, because the lottery only
        wakes up on a volatility spike and a scenario that never wakes it tests nothing.
        A tail probability of 0.04 against a 5c ask is the case that separates them: the
        original sees a longshot barely worth its price and passes, the twin sees 0.96 and
        takes it.
        """
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        close = 1_800_000_900.0
        now = close - 600  # well inside the lottery's 3-minute minimum
        mkt = h.market("BTC", "KXBTC15M-LOTTO", 80000.0, close, yes_bid=0.04, yes_ask=0.05)
        ctx = {"p_tail": 0.04, "spike": 2.0}
        for name in ("Lottery", "Anti Lottery"):
            acct = t.accounts[name]
            acct.step(mkt, 79900.0, now, 0.04, False, dict(ctx))
        real, anti = t.accounts["Lottery"], t.accounts["Anti Lottery"]
        self.assertEqual(len(real.log), 0,
                         "the original took a longshot it thought was worth 4c at 5c")
        self.assertEqual(len(anti.log), 1,
                         "the twin passed on a longshot it should have valued at 96c")

    def test_inverting_does_not_corrupt_the_shared_context(self):
        """One ctx is handed to every account in a step. Inverting it in place would flip the
        signal for every strategy that saw it afterwards."""
        t = h.new_trader()
        t._append_csv = lambda *a, **k: None
        seen = []
        original = kt.Account._lottery

        def spy(self, market, price, now, p_model, paused, ctx):
            seen.append((self.name, ctx.get("p_tail")))
            return original(self, market, price, now, p_model, paused, ctx)

        kt.Account._lottery = spy
        try:
            h.lagging_book_round(t)
        finally:
            kt.Account._lottery = original
        by_step = {}
        for name, tail in seen:
            by_step.setdefault(name, []).append(tail)
        real, anti = by_step.get("Lottery", []), by_step.get("Anti Lottery", [])
        self.assertTrue(real and anti, "the lottery path never ran")
        for r, a in zip(real, anti):
            if r is None or a is None:
                continue
            with self.subTest(p_tail=r):
                self.assertAlmostEqual(r + a, 1.0, places=6,
                                       msg="the twin saw a tail probability that was not "
                                           "the original's complement")

    def test_a_twin_can_be_told_apart_in_the_logs(self):
        t = h.new_trader()
        rows = []
        t._append_csv = lambda e, now: rows.append(e["strategy"])
        h.lagging_book_round(t)
        self.assertTrue(any(r.startswith("Anti ") for r in rows),
                        "nothing in the trade log identifies a twin")


if __name__ == "__main__":
    unittest.main(verbosity=2)

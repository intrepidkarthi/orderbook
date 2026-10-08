"""Tests for obook (docs/PYTHON.md §4). Run with `make python`."""

import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, ".."))

import obook  # noqa: E402
from obook import BUY, SELL, MARKET, IOC, FOK, Status, Reason, EventKind  # noqa: E402

# The repository, for the digest test: next to the tests when run from a checkout,
# or named by OBOOK_REPO when the tests run against an installed wheel.
REPO = os.environ.get("OBOOK_REPO") or os.path.abspath(os.path.join(HERE, "..", ".."))


class Commands(unittest.TestCase):
    def setUp(self):
        self.b = obook.Book(max_orders=1000)

    def tearDown(self):
        self.b.close()

    def test_resting_then_crossing(self):
        a = self.b.submit(user=1, side=SELL, price=1010, qty=5)
        self.assertEqual((a.status, a.reason), (Status.NEW, Reason.NONE))
        self.assertGreater(a.order_id, 0)
        r = self.b.submit(user=2, side=BUY, price=1012, qty=3)
        self.assertEqual(r.status, Status.FILLED)
        self.assertEqual(self.b.last_trade_price(), 1010)
        rest = self.b.book()
        self.assertEqual(len(rest), 1)
        self.assertEqual((rest[0].order_id, rest[0].qty, rest[0].filled), (a.order_id, 5, 3))

    def test_partial_fill_rests_the_remainder(self):
        self.b.submit(user=1, side=SELL, price=1010, qty=2)
        r = self.b.submit(user=2, side=BUY, price=1010, qty=5)
        self.assertEqual(r.status, Status.PARTIALLY_FILLED)
        [o] = self.b.book()
        self.assertEqual((o.order_id, o.side, o.qty, o.filled), (r.order_id, BUY, 5, 2))

    def test_refusals_are_results(self):
        def check(r, reason):
            self.assertEqual((r.status, r.reason), (Status.REJECTED, reason))

        check(self.b.cancel(999_999, user=1), Reason.ORDER_NOT_FOUND)
        # A market order into an empty book is accepted and then cancelled for want of
        # liquidity, not refused: the engine's verdict, passed through.
        r = self.b.submit(user=2, side=BUY, price=1010, qty=1, type=MARKET)
        self.assertEqual((r.status, r.reason), (Status.CANCELLED, Reason.MARKET_ORDER_NO_LIQUIDITY))
        self.b.submit(user=1, side=SELL, price=1010, qty=2)
        check(self.b.submit(user=2, side=BUY, price=1010, qty=1, post_only=True),
              Reason.POST_ONLY_WOULD_CROSS)
        check(self.b.submit(user=2, side=BUY, price=1010, qty=5, tif=FOK),
              Reason.FOK_CANNOT_FILL)
        self.assertEqual(len(self.b.book()), 1, "a refusal changed the book")

    def test_cancel_reduce_replace(self):
        a = self.b.submit(user=1, side=BUY, price=1000, qty=10)
        r = self.b.reduce(a.order_id, new_qty=4, user=1)
        self.assertEqual(r.status, Status.REDUCED)
        self.assertEqual(self.b.book()[0].qty, 4)
        r = self.b.replace(a.order_id, user=1, side=BUY, price=1001, qty=6)
        self.assertEqual(r.status, Status.NEW)
        self.assertNotEqual(r.order_id, a.order_id)
        [o] = self.b.book()
        self.assertEqual((o.order_id, o.price, o.qty), (r.order_id, 1001, 6))
        c = self.b.cancel(r.order_id, user=1)
        self.assertEqual(c.status, Status.CANCELLED)
        self.assertEqual(self.b.book(), [])
        again = self.b.replace(r.order_id, user=1, side=BUY, price=1002, qty=1)
        self.assertEqual((again.order_id, again.status, again.reason),
                         (0, Status.REJECTED, Reason.ORDER_NOT_FOUND))

    def test_ioc_remainder_is_cancelled(self):
        self.b.submit(user=1, side=SELL, price=1010, qty=1)
        r = self.b.submit(user=2, side=BUY, price=1010, qty=3, tif=IOC)
        self.assertEqual(self.b.book(), [])
        kinds = [e.kind for e in self.b.events()]
        self.assertEqual(kinds[-2:], [EventKind.TRADE, EventKind.CANCELED])
        # The engine's verdict for an IOC order is its final state: the remainder
        # was cancelled.
        self.assertEqual(r.status, Status.CANCELLED)

    def test_out_of_range_enums_raise(self):
        with self.assertRaises(ValueError):
            self.b.submit(user=1, side=2, price=1, qty=1)
        with self.assertRaises(ValueError):
            self.b.submit(user=1, side=BUY, price=1, qty=1, tif=3)


class Events(unittest.TestCase):
    def test_kinds_order_and_trade_fields(self):
        with obook.Book() as b:
            a = b.submit(user=1, side=SELL, price=1010, qty=5)
            t = b.submit(user=2, side=BUY, price=1012, qty=3)
            evs = b.events()
            self.assertEqual([e.kind for e in evs], [EventKind.ACCEPTED, EventKind.ACCEPTED, EventKind.TRADE])
            tr = evs[-1]
            self.assertEqual((tr.price, tr.qty, tr.maker_id, tr.taker_id, tr.aggressor),
                             (1010, 3, a.order_id, t.order_id, BUY))
            self.assertEqual(b.events(), [], "draining did not empty the queue")

    def test_more_events_than_one_batch(self):
        with obook.Book() as b:
            n = 3 * obook.Book._EVENT_BATCH + 7
            for i in range(n):
                b.submit(user=1, side=BUY, price=900 + i % 50, qty=1)
            self.assertEqual(len(b.events()), n)


class BookOrder(unittest.TestCase):
    def test_priority_order_and_growth(self):
        with obook.Book(max_orders=10_000) as b:
            ids = {}
            for i in range(3000):  # past the first 1024-order buffer
                side = BUY if i % 2 == 0 else SELL
                price = 1000 - (i % 7) if side == BUY else 1010 + (i % 7)
                ids[b.submit(user=1 + i % 2, side=side, price=price, qty=1).order_id] = i
            rest = b.book()
            self.assertEqual(len(rest), 3000)
            bids = [o for o in rest if o.side == BUY]
            asks = [o for o in rest if o.side == SELL]
            self.assertEqual(rest, bids + asks, "bids do not all come first")
            key_b = [(-o.price, ids[o.order_id]) for o in bids]
            key_a = [(o.price, ids[o.order_id]) for o in asks]
            self.assertEqual(key_b, sorted(key_b), "bids not best-first, oldest-first")
            self.assertEqual(key_a, sorted(key_a), "asks not best-first, oldest-first")


class Lifecycle(unittest.TestCase):
    def test_closed_book_refuses(self):
        b = obook.Book()
        b.close()
        b.close()  # twice is fine
        for call in (lambda: b.submit(1, BUY, 1, 1), lambda: b.events(), lambda: b.book()):
            with self.assertRaises(ValueError):
                call()

    def test_books_are_independent(self):
        with obook.Book() as x, obook.Book() as y:
            x.submit(user=1, side=SELL, price=1010, qty=1)
            r = y.submit(user=2, side=BUY, price=1010, qty=1)
            self.assertEqual(r.status, Status.NEW)
            self.assertEqual(len(x.book()), 1)
            self.assertEqual(len(y.book()), 1)


class Loading(unittest.TestCase):
    def test_wrong_abi_version_is_refused(self):
        cc = shutil.which("cc")
        if not cc:
            self.skipTest("no C compiler")
        with tempfile.TemporaryDirectory() as d:
            src = os.path.join(d, "stub.c")
            lib = os.path.join(d, "libstub.so")
            with open(src, "w") as f:
                f.write("#include <stdint.h>\nint32_t ob_abi_version(void) { return 2; }\n")
            subprocess.run([cc, "-shared", "-fPIC", "-o", lib, src], check=True)
            with self.assertRaisesRegex(ImportError, "ABI 2"):
                obook.load(lib)

    def test_missing_library_says_how_to_build(self):
        with self.assertRaisesRegex(ImportError, "make python"):
            obook.load("/nonexistent/libobook.so")


class Digest(unittest.TestCase):
    """The same engine through Python: bench-basic-v1 must give the committed digest."""

    def test_bench_basic_tape_through_python(self):
        go = shutil.which("go")
        if not go:
            self.skipTest("no Go toolchain")
        if not os.path.exists(os.path.join(REPO, "go.mod")):
            self.skipTest("no repository at " + REPO + "; set OBOOK_REPO")
        tape = os.path.join(REPO, "internal/benchgate/testdata/bench-basic-v1.obt")
        digest = os.path.join(REPO, "internal/benchgate/testdata/bench-basic-v1.digest")
        with tempfile.TemporaryDirectory() as d:
            xeng = os.path.join(d, "xeng")
            subprocess.run([go, "build", "-o", xeng, "./cmd/xeng"], cwd=REPO, check=True)
            text = subprocess.run([xeng, "export", tape], check=True, capture_output=True).stdout
            out = subprocess.run([sys.executable, os.path.join(REPO, "python/examples/xeng_replay.py")],
                                 input=text, check=True, capture_output=True).stdout
            outfile = os.path.join(d, "python.out")
            with open(outfile, "wb") as f:
                f.write(out)
            table = subprocess.run([xeng, "table", tape, digest, "python=" + outfile],
                                   check=True, capture_output=True, text=True).stdout
            self.assertIn("| python | agrees |", table, table)


if __name__ == "__main__":
    unittest.main()

"""The orderbook matching engine, from Python (docs/PYTHON.md).

A ctypes binding over the engine's C ABI (docs/C-API.md). Standard library only.
"""

import ctypes
import enum
import os
import sys
from typing import List, NamedTuple

__all__ = [
    "Book", "Result", "Event", "Order", "Status", "Reason", "EventKind",
    "BUY", "SELL", "LIMIT", "MARKET", "GTC", "IOC", "FOK", "ABI_VERSION",
]

ABI_VERSION = 1

BUY, SELL = 0, 1
LIMIT, MARKET = 0, 1
GTC, IOC, FOK = 0, 1, 2


class Status(enum.IntEnum):
    NA = 0
    NEW = 1
    PARTIALLY_FILLED = 2
    FILLED = 3
    CANCELLED = 4
    REJECTED = 5
    REDUCED = 6


class Reason(enum.IntEnum):
    NONE = 0
    TRADING_HALTED = 1
    NEW_ORDERS_HALTED = 2
    POST_ONLY_WOULD_CROSS = 3
    FOK_CANNOT_FILL = 4
    MARKET_ORDER_NO_LIQUIDITY = 5
    ORDER_BOOK_FULL = 6
    ORDER_NOT_FOUND = 7
    ORDER_NOT_ACTIVE = 8
    INVALID_QUANTITY = 9
    NIL_ORDER = 10
    NOTIONAL_OVERFLOW = 11


class EventKind(enum.IntEnum):
    ACCEPTED = 1
    REJECTED = 2
    TRADE = 3
    CANCELED = 4
    REPLACED = 5


class Result(NamedTuple):
    order_id: int
    status: Status
    reason: Reason


class Event(NamedTuple):
    kind: EventKind
    reason: Reason
    aggressor: int  # trades: the taker's side, BUY or SELL
    order_id: int
    user: int
    price: int
    qty: int
    maker_id: int
    taker_id: int


class Order(NamedTuple):
    order_id: int
    user: int
    side: int
    price: int
    qty: int
    filled: int


class _CEvent(ctypes.Structure):
    _fields_ = [
        ("kind", ctypes.c_int32), ("reason", ctypes.c_int32),
        ("aggressor", ctypes.c_int32), ("_pad", ctypes.c_int32),
        ("order_id", ctypes.c_int64), ("user", ctypes.c_int64),
        ("price", ctypes.c_int64), ("qty", ctypes.c_int64),
        ("maker_id", ctypes.c_int64), ("taker_id", ctypes.c_int64),
    ]


class _COrder(ctypes.Structure):
    _fields_ = [
        ("order_id", ctypes.c_int64), ("user", ctypes.c_int64),
        ("price", ctypes.c_int64), ("qty", ctypes.c_int64),
        ("filled", ctypes.c_int64), ("side", ctypes.c_int32),
        ("_pad", ctypes.c_int32),
    ]


def _library_path() -> str:
    env = os.environ.get("OBOOK_LIBRARY")
    if env:
        return env
    name = "libobook.dylib" if sys.platform == "darwin" else "libobook.so"
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), name)


def load(path: str) -> ctypes.CDLL:
    """Loads the library at path, declares its signatures, and checks its ABI."""
    if not os.path.exists(path):
        raise ImportError(
            f"obook: no engine library at {path}. Build it with `make python` from the "
            "repository root, or set OBOOK_LIBRARY to its path.")
    lib = ctypes.CDLL(path)
    i32, i64 = ctypes.c_int32, ctypes.c_int64
    p32, p64 = ctypes.POINTER(i32), ctypes.POINTER(i64)
    lib.ob_abi_version.restype = i32
    lib.ob_abi_version.argtypes = []
    got = lib.ob_abi_version()
    if got != ABI_VERSION:
        raise ImportError(f"obook: {path} speaks ABI {got}; this package speaks ABI {ABI_VERSION}")
    sigs = {
        "ob_new": (i64, [i64]),
        "ob_free": (None, [i64]),
        "ob_submit": (i32, [i64, i64, i32, i32, i64, i64, i32, i32, p64, p32, p32]),
        "ob_cancel": (i32, [i64, i64, i64, p32, p32]),
        "ob_reduce": (i32, [i64, i64, i64, i64, p32, p32]),
        "ob_replace": (i32, [i64, i64, i64, i32, i32, i64, i64, i32, i32, p64, p32, p32]),
        "ob_events": (i32, [i64, ctypes.POINTER(_CEvent), i32]),
        "ob_book": (i32, [i64, ctypes.POINTER(_COrder), i32]),
        "ob_last_trade_price": (i64, [i64]),
    }
    for name, (res, args) in sigs.items():
        f = getattr(lib, name)
        f.restype, f.argtypes = res, args
    return lib


_lib = None


def _get_lib() -> ctypes.CDLL:
    global _lib
    if _lib is None:
        _lib = load(_library_path())
    return _lib


def _check_enum(name: str, v: int, hi: int) -> None:
    if not isinstance(v, int) or v < 0 or v > hi:
        raise ValueError(f"obook: {name} {v!r} is outside 0..{hi}")


class Book:
    """One order book: a handle on the engine. Single-writer, like the engine."""

    _EVENT_BATCH = 256

    def __init__(self, max_orders: int = 100_000, library: ctypes.CDLL = None):
        self._lib = library or _get_lib()
        self._h = self._lib.ob_new(max_orders)
        if self._h == 0:
            raise ValueError(f"obook: could not create a book with max_orders={max_orders}")
        self._events = (_CEvent * self._EVENT_BATCH)()
        self._orders = (_COrder * 1024)()

    def close(self) -> None:
        if self._h:
            self._lib.ob_free(self._h)
            self._h = 0

    def __enter__(self) -> "Book":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass

    def _handle(self) -> int:
        if not self._h:
            raise ValueError("obook: the book is closed")
        return self._h

    @staticmethod
    def _order_args(side, type, tif, post_only):
        _check_enum("side", side, 1)
        _check_enum("type", type, 1)
        _check_enum("tif", tif, 2)
        return side, type, tif, 1 if post_only else 0

    def _result(self, rc: int, call: str, oid: int, st, rs) -> Result:
        if rc != 0:
            raise RuntimeError(f"obook: {call} could not run (the library returned -1)")
        return Result(oid, Status(st.value), Reason(rs.value))

    def submit(self, user: int, side: int, price: int, qty: int, type: int = LIMIT,
               tif: int = GTC, post_only: bool = False) -> Result:
        """Submits an order. Result.order_id is the engine's id for it."""
        h = self._handle()
        side, type, tif, po = self._order_args(side, type, tif, post_only)
        oid, st, rs = ctypes.c_int64(), ctypes.c_int32(), ctypes.c_int32()
        rc = self._lib.ob_submit(h, user, side, type, price, qty, tif, po,
                                 ctypes.byref(oid), ctypes.byref(st), ctypes.byref(rs))
        return self._result(rc, "submit", oid.value, st, rs)

    def cancel(self, order_id: int, user: int) -> Result:
        h = self._handle()
        st, rs = ctypes.c_int32(), ctypes.c_int32()
        rc = self._lib.ob_cancel(h, order_id, user, ctypes.byref(st), ctypes.byref(rs))
        return self._result(rc, "cancel", order_id, st, rs)

    def reduce(self, order_id: int, new_qty: int, user: int) -> Result:
        """Reduces an order's total quantity to new_qty, keeping its priority."""
        h = self._handle()
        st, rs = ctypes.c_int32(), ctypes.c_int32()
        rc = self._lib.ob_reduce(h, order_id, new_qty, user, ctypes.byref(st), ctypes.byref(rs))
        return self._result(rc, "reduce", order_id, st, rs)

    def replace(self, order_id: int, user: int, side: int, price: int, qty: int,
                type: int = LIMIT, tif: int = GTC, post_only: bool = False) -> Result:
        """Cancels order_id and submits a new order. Result.order_id is the new id, or
        0 if the cancel was refused and nothing was submitted."""
        h = self._handle()
        side, type, tif, po = self._order_args(side, type, tif, post_only)
        oid, st, rs = ctypes.c_int64(), ctypes.c_int32(), ctypes.c_int32()
        rc = self._lib.ob_replace(h, order_id, user, side, type, price, qty, tif, po,
                                  ctypes.byref(oid), ctypes.byref(st), ctypes.byref(rs))
        return self._result(rc, "replace", oid.value, st, rs)

    def events(self) -> List[Event]:
        """Drains every event published since the last call, in publish order."""
        h = self._handle()
        out = []
        while True:
            n = self._lib.ob_events(h, self._events, self._EVENT_BATCH)
            if n < 0:
                raise RuntimeError("obook: events could not be read (the library returned -1)")
            for e in self._events[:n]:
                out.append(Event(EventKind(e.kind), Reason(e.reason), e.aggressor, e.order_id,
                                 e.user, e.price, e.qty, e.maker_id, e.taker_id))
            if n < self._EVENT_BATCH:
                return out

    def book(self) -> List[Order]:
        """The resting orders: bids best-first, then asks best-first, oldest first in a level."""
        h = self._handle()
        while True:
            n = self._lib.ob_book(h, self._orders, len(self._orders))
            if n >= 0:
                return [Order(o.order_id, o.user, o.side, o.price, o.qty, o.filled)
                        for o in self._orders[:n]]
            self._orders = (_COrder * (2 * len(self._orders)))()

    def last_trade_price(self) -> int:
        return self._lib.ob_last_trade_price(self._handle())

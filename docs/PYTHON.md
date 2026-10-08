# Python Bindings — the C ABI, From the Standard Library

Status: **implemented, tested on CI; step 2.1 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. Why, and why this shape

Most people who study order books do it in Python: notebooks, backtests, teaching. The
engine reaches them through [`C-API.md`](C-API.md). The bindings are `ctypes` over that
interface and nothing else.
- **No compiler on the user's machine.** The bindings themselves are not compiled.
- **No dependencies.** Only the standard library is imported.
- **No second definition of the semantics.** Every number Python sees comes from the
  same Go engine the rest of this repository tests.

## 2. Shape

`python/` holds a package, `obook`:

```python
import obook

with obook.Book(max_orders=100_000) as b:
    r = b.submit(user=1, side=obook.SELL, price=1010, qty=5)
    r = b.submit(user=2, side=obook.BUY, price=1012, qty=3)
    r.status                      # obook.Status.FILLED
    for e in b.events():          # Accepted, Accepted, Trade
        ...
    b.book()                      # [Order(order_id, user, side, price, qty, filled)]
    b.cancel(order_id, user=1)    # Result(order_id, status, reason)
```

- **`Book`** owns one handle. `close()` frees it, and so does leaving a `with` block.
  Using a closed book raises `ValueError`.
- **Commands** are `submit`, `cancel`, `reduce` and `replace`. Each returns
  `Result(order_id, status, reason)`, with `status` an `obook.Status` and `reason` an
  `obook.Reason`, both `IntEnum`s whose values are the ABI's codes. A refusal is a
  result, never an exception, as in the C ABI. An exception means the call could not
  run: a closed book, an out-of-range enum, or the library's `-1`.
- **`events()`** drains everything published since the last call, as `Event` named
  tuples (`kind`, `reason`, `aggressor`, `order_id`, `user`, `price`, `qty`,
  `maker_id`, `taker_id`), in publish order.
- **`book()`** returns the resting orders, bids best-first, then asks best-first,
  oldest first within a level. It grows its buffer and retries when the C call reports
  `cap` too small.
- **`last_trade_price()`** is 0 until something trades.

## 3. Finding the library

1. `OBOOK_LIBRARY`, if set, is the path to `libobook.so` or `libobook.dylib`.
2. Otherwise, the library next to the package's `__init__.py`. `make python` builds it
   there.
3. Otherwise, the import raises `ImportError`, and the message says how to build it.

**The ABI version is checked at load.** A library reporting a version other than 1 is
refused with an `ImportError` naming both versions. That check is the reason
`ob_abi_version` exists.

## 4. What is tested

`python3 -m unittest discover python/tests` runs on CI on Linux, against a library
built from the commit under test:

- **Every command's result**, including each refusal the ABI can report: a cancel of an
  unknown order, post-only that would cross, FOK that cannot fill, and a market order
  into an empty book.
- **Events**: kinds, order, the trade's maker, taker and aggressor, and that draining
  empties the queue.
- **The book's order**, and buffer growth past the first guess.
- **Lifecycle**: a closed book refuses calls, and two books are independent.
- **Loading**: a wrong ABI version is refused. This is tested against a stub library
  built for the test from a few lines of C.
- **The digest.** `examples/xeng_replay.py` drives the engine through `obook` as a
  cross-engine adapter ([`CROSS-ENGINE.md`](CROSS-ENGINE.md) §3). A test replays
  `bench-basic-v1` through it and requires `xeng table` to report the committed
  `core` digest. Python is then held to the same 21,856 trades and the same final book
  as every other path. The test skips without a Go toolchain.

`examples/quickstart.py` is the script the README points to. It runs as plain Python,
and each step is commented so it can be pasted into a notebook cell.

## 5. Not in this step

- **Windows** has no build here yet: `c-shared` needs a C toolchain on CI.
- **numpy or pandas helpers** are not provided. A list of named tuples converts to a
  DataFrame in one call, and a dependency would be a cost every user pays.

## 6. Wheels and publishing (2026-10-08)

The distribution name is **`obook`**, and it was free on PyPI.

- **One wheel per platform, each carrying its own `libobook`.**

  | Wheel tag | Built on |
  |---|---|
  | `manylinux_2_28_x86_64` | the `quay.io/pypa/manylinux_2_28_x86_64` container |
  | `manylinux_2_28_aarch64` | the matching container, on an arm64 runner |
  | `macosx_12_0_arm64` | macOS 14 |
  | `macosx_12_0_x86_64` | macOS 15 on Intel (`macos-15-intel`; `macos-13` is retired) |

  - The Linux wheels build inside the manylinux containers, so the library links
    against the glibc its tag claims. Go's `c-shared` output needs only libc and
    pthreads.
  - macOS 12 is the oldest release current Go toolchains support.
  - The Python and ABI tags are `py3-none`. The package itself is pure Python, and
    only the native library differs.
- **Each wheel is installed into a fresh virtual environment, away from the source
  tree, and the test suite runs against it** before anything is uploaded. The digest
  test needs the repository and a Go toolchain, so it runs on the Linux x86_64 wheel
  only.
- **Publishing is trusted publishing (OIDC)**, from `python-wheels.yml`, in a GitHub
  environment named `pypi`. No API token exists anywhere. It runs only on a manual
  dispatch with `publish: true`, and only after every wheel built and passed.
- **Before the first publish, the maintainer registers a pending publisher on PyPI:**
  - project `obook`;
  - owner `intrepidkarthi`, repository `orderbook`;
  - workflow `python-wheels.yml`, environment `pypi`.

  That is an account action, and nothing here can do it.
- **Versions.** `python/pyproject.toml` holds the version, starting at 0.1.0. It is
  bumped by hand per release, separate from the Go module's tags, because the wheel
  carries ABI 1, which is not tied to an engine release.

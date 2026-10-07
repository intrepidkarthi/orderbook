# C API — the Engine Behind a Stable C ABI

Status: **specified; built with step 1.2 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. Why

Three things on the adoption plan need the engine outside Go:
- an adapter for the independent flash1 harness, which drives every engine through a
  C ABI;
- Python bindings over `ctypes`;
- any C, C++ or Rust program that wants to embed it.

All three are thin layers over one C interface, so the interface is built once,
specified here, and tested at the C boundary itself rather than through Go calls that
skip it.

## 2. Shape

`cmd/libobook` builds with `go build -buildmode=c-shared` into `libobook.so` (or
`.dylib`) plus a generated `libobook.h`. Every function is plain C: integers, and
pointers to caller-owned memory. No Go pointer ever crosses the boundary.

```c
int32_t ob_abi_version(void);                      // 1
int64_t ob_new(int64_t max_orders);                // a handle > 0, or 0 on error
void    ob_free(int64_t h);

int32_t ob_submit (int64_t h, int64_t user, int32_t side, int32_t type,
                   int64_t price, int64_t qty, int32_t tif, int32_t post_only,
                   int64_t *order_id, int32_t *status, int32_t *reason);
int32_t ob_cancel (int64_t h, int64_t order_id, int64_t user,
                   int32_t *status, int32_t *reason);
int32_t ob_reduce (int64_t h, int64_t order_id, int64_t new_qty, int64_t user,
                   int32_t *status, int32_t *reason);
int32_t ob_replace(int64_t h, int64_t order_id, int64_t user, int32_t side,
                   int32_t type, int64_t price, int64_t qty, int32_t tif,
                   int32_t post_only, int64_t *new_order_id,
                   int32_t *status, int32_t *reason);

int32_t ob_events (int64_t h, ob_event *buf, int32_t cap);  // drains up to cap
int32_t ob_book   (int64_t h, ob_order *buf, int32_t cap);  // resting orders
int64_t ob_last_trade_price(int64_t h);
```

- **Return value.** Every command returns 0 when it ran and −1 when it could not:
  - an unknown handle;
  - a null out-pointer;
  - an enum value outside its range.

  "Ran" includes a refusal. A rejected order returns 0 with `status` set to Rejected and
  `reason` saying why.
- **Units.** Prices are int64 ticks and quantities int64 lots, as everywhere else in the
  engine. A user is an int64; the engine sees it as `u<n>`, the same spelling the
  `obtape` driver uses.
- **Enums.** Side is 0 buy, 1 sell. Type is 0 limit, 1 market. TIF is 0 GTC, 1 IOC,
  2 FOK.
- **Status and reason codes.** These are the OBDG codes of
  [`BENCH-GATE.md`](BENCH-GATE.md) §3.1:
  - status: 1 New, 2 PartiallyFilled, 3 Filled, 4 Cancelled, 5 Rejected, 6 Reduced;
  - reason: 0 None through 11 NotionalOverflow.

  An engine error with no code returns −1, because a guessed code would be worse.

```c
typedef struct { int32_t kind, reason, aggressor, _pad;
                 int64_t order_id, user, price, qty, maker_id, taker_id; } ob_event;
typedef struct { int64_t order_id, user, price, qty, filled; int32_t side, _pad; } ob_order;
```

- **Event kinds:** 1 Accepted, 2 Rejected, 3 Trade, 4 Canceled, 5 Replaced.
  - A trade carries price, qty, the maker and taker order ids, and the aggressor (0 buy,
    1 sell).
  - Other kinds carry the order id, its user, and a reason when refused.
- **Delivery.** Events accumulate per handle and `ob_events` drains them in publish
  order, so a caller that reads after every command sees each command's stream exactly
  as the Go `EventSink` would.
- **`ob_book`** writes the resting orders bids best-first, then asks best-first, oldest
  first within a level. It returns how many it wrote, or −1 if `cap` is too small.

## 3. Rules that will look like bugs

- **A handle is single-writer**, like the engine. Calls on one handle from two threads
  at once are undefined. Different handles are independent. The handle table itself is
  safe to use from any thread.
- **The ABI is versioned and frozen at 1.** A change to any signature, struct layout or
  code value is version 2, and `ob_abi_version` is how a binding refuses a library it
  was not written for.
- **No trade list is returned from a command.** Trades arrive as events. The digest test
  therefore takes both OBDG chains' trades from the event stream on this path, where the
  native driver takes `core`'s from the returned list. Each path is checked against the
  native digest, which is what makes the shortcut safe.

## 4. How it is tested

`TestCABIReplaysTheBenchTapeToTheSameDigest` (in `internal/benchgate`):
1. Builds the library with `go build -buildmode=c-shared`.
2. Compiles `testdata/capi_driver.c` against it with the system C compiler.
3. Feeds the driver the bench tape as one numeric line per command.
4. Folds the driver's printed statuses, events and final book into the OBDG encoder.
5. Requires both chains to equal the native engine's digest.

It skips when no C compiler is present. CI has one, so it runs there. A mapping broken
on the C side, such as a reason code off by one, must fail it.

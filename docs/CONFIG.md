# Configuration reference

Every knob the engine exposes, what it does, its default, how it's validated, and
— importantly — **what the matching core deliberately does *not* do**. The core
owns matching; risk, fees, credit, auth, persistence backends, and rate limits
belong to the layers around it (see [INTEGRATION.md](INTEGRATION.md)). Drawing
that boundary sharply is a deliberate design choice, not an omission.

Prices are **integer ticks** and quantities **integer lots** (`int64`)
everywhere inside the engine. Human decimals are converted only at the boundary,
by an [`Instrument`](#instrument--the-priceqty-grid). See
[SPEC.md §6.1](SPEC.md#61-price--quantity--int64-ticks--lots-decimal-at-the-edge)
for the rationale, and the generated
[API reference on pkg.go.dev](https://pkg.go.dev/github.com/intrepidkarthi/orderbook)
for exact type and method signatures.

---

## `types.Instrument` — the price/qty grid

`types.Instrument` defines a symbol's tick/lot grid and converts decimals ⇄
integers. Construct it once per symbol and reuse it.

```go
inst := types.NewInstrument("BTC-USD", dec("0.01"), dec("0.001"))
```

| Field | Type | Purpose | Default | Validation |
|-------|------|---------|---------|------------|
| `Symbol` | `string` | Book identity | — | non-empty (by convention) |
| `TickSize` | `decimal.Decimal` | Smallest price increment | `1` if zero | positive |
| `LotSize` | `decimal.Decimal` | Smallest quantity increment | `1` if zero | positive |

**Choosing tick/lot.** Match the venue you model: BTC-USD is often `0.01` /
`0.00000001`; an equity is `0.01` / `1`; a futures contract `0.25` / `1`. A price
of `30000.50` on a `0.01` tick becomes `3000050` ticks; a size of `0.25` on a
`0.001` lot becomes `250` lots. Values are rounded to the grid on conversion.

**Conversion methods** (the only place decimals touch the engine):

| Method | Returns |
|--------|---------|
| `PriceToTicks(decimal) int64` | price → ticks (rounded to grid) |
| `TicksToPrice(int64) decimal` | ticks → price |
| `QtyToLots(decimal) int64` | quantity → lots |
| `LotsToQty(int64) decimal` | lots → quantity |
| `NewOrder(user, side, type, price, qty decimal, tif) (*Order, error)` | build an order from decimals |

> If you already think in ticks/lots, skip the Instrument and call
> `types.NewOrder(user, symbol, side, type, priceTicks, qtyLots, tif)` directly.

---

## `matching.Config` — engine policy

Passed to `matching.NewEngine(cfg)`. `matching.DefaultConfig(symbol)` fills sane
defaults.

| Field | Type | Purpose | Default | Notes |
|-------|------|---------|---------|-------|
| `Symbol` | `string` | Book identity | — | one engine per symbol |
| `SelfTradePrevention` | `SelfTradePrevention` | What happens when an order would match the **same `UserID`** | `STPCancelNewest` | see below |
| `MaxOrders` | `int` | Capacity; further inserts return `ErrOrderBookFull` | `100_000` | 0 → default |
| `PriceBand` | `decimal.Decimal` | Circuit-breaker collar: reject a **limit** priced more than this fraction from the last trade (e.g. `0.10` = ±10%) | `0` (disabled) | decimal, cold path only |
| `ProRata` | `bool` | Size-proportional allocation at a price level instead of FIFO price-time | `false` (FIFO) | see below |
| `Clock` | `func() time.Time` | Timestamp source for orders/trades/snapshots | `nil` → `time.Now` | inject a deterministic clock for byte-identical replay |
| `DisabledClasses` | `[]OrderClass` | Advanced order families to reject with `ErrOrderTypeDisabled` (`ClassStop`/`Iceberg`/`Pegged`/`OCO`/`Trailing`) | `nil` (all enabled) | feature-flag off a risky exotic type without a redeploy |
| `Guardrail` | `Guardrail` | Self-output tripwire: trip to `Halted` when trades/notional in `Window` exceed `MaxTrades`/`MaxNotional` | zero (disabled) | guards the engine's *own* output (the Knight lesson) |
| `EventSink` | `EventSink` | Receives the ordered, sequence-numbered event stream (`Accepted`/`Rejected`/`Trade`/`Canceled`) | `nil` (no events) | non-blocking; the seam for market-data/drop-copy/recovery adapters |
| `MaxOrderQty` | `int64` | Reject any single order larger than this many lots (`ErrOrderExceedsMaxQty`) | `0` (disabled) | fat-finger cap; `Privileged` exempt |
| `MaxOrderNotional` | `int64` | Reject any single **limit** order with `price×qty` over this (`ErrOrderExceedsMaxNotional`) | `0` (disabled) | fat-finger cap; overflowing notional always rejected (`ErrNotionalOverflow`) |
| `MinRestingTime` | `time.Duration` | A resting book order cannot be cancelled until it has rested this long (`ErrCancelTooSoon`) | `0` (disabled) | anti-spoofing; `Privileged` exempt; bypassed on replay |
| `MaxMarkStep` | `decimal.Decimal` | Reject a `SetMarkPrice` update jumping more than this fraction from the current mark (`ErrMarkStepTooLarge`) | `0` (disabled) | anti oracle-pump; first mark and clearing to 0 always accepted |

See [THREAT-MODEL.md](THREAT-MODEL.md) for the attacks these controls address
and the real enforcement cases behind each.

### Operational states & determinism (Phase A)

- **Injectable clock.** The engine is the sole timestamp authority: it stamps
  order/trade/snapshot times from `Config.Clock`. Injecting a deterministic clock
  makes replay byte-identical down to the timestamps — the "no wall clock in the
  state transition" rule. Default is real time.
- **Degraded states.** `Engine.State()` is one of `StateOpen`, `StateCancelOnly`
  (cancels accepted, new liquidity rejected with `ErrNewOrdersHalted`), or
  `StateHalted` (everything rejected). Drive with `Halt()` / `SetCancelOnly()` /
  `Resume()` (also on `Runner`) — the cancel-only → auction → full recovery path
  real venues use.
- **Feature-flagged order types.** Any advanced family in `DisabledClasses` is
  rejected at entry, so one buggy exotic type can be switched off without downing
  the venue.
- **Self-output guardrail.** An optional cap on the engine's own trade/notional
  output per window that trips it to `Halted`.

### Pre-trade risk & anti-manipulation controls

Opt-in admission controls (all default to disabled) that gate the **live ingress
path** — enforced on the cold reject path, so the zero-alloc hot path is
untouched, and `Privileged` (liquidation/ADL) orders are exempt from the caps and
the resting-time floor. Each maps to a named enforcement case in
[THREAT-MODEL.md](THREAT-MODEL.md):

- **Per-order caps** (`MaxOrderQty`, `MaxOrderNotional`) — fat-finger guards on a
  *single* order, complementing `Guardrail` (which bounds *aggregate* output). An
  order whose `price×qty` overflows int64 is always rejected, and the guardrail's
  windowed notional accumulates with a saturating add so no wrap can hide a
  runaway.
- **Minimum resting time** (`MinRestingTime`) — defeats the post-size-then-pull
  spoofing pattern by refusing a too-soon cancel.
- **Mark-step guard** (`MaxMarkStep`) — `SetMarkPrice` returns an error and rejects
  an outsized jump, so a thin-book oracle pump cannot drag the price band with it.

These live-ingress checks (the time-based ones) are **bypassed during
deterministic replay** — the engine has a replay mode that `pkg/wal` `Restore`
wraps recovery in, so an already-accepted cancel is never re-litigated against
replay-time timestamps and the recovered book stays byte-identical.

### Self-trade prevention (STP)

STP keys on the order's `UserID`. When an incoming order would match a resting
order from the same user:

| Mode | Constant | Behavior | ~Venue analogue |
|------|----------|----------|-----------------|
| Cancel newest | `STPCancelNewest` | Cancel the *incoming* order's remainder (default) | Binance `EXPIRE_TAKER` |
| Cancel oldest | `STPCancelOldest` | Cancel the *resting* maker, keep matching | Binance `EXPIRE_MAKER` |
| Cancel both | `STPCancelBoth` | Cancel incoming **and** the resting maker | Binance `EXPIRE_BOTH` |
| Allow | `STPAllow` | Permit the self-trade (flagged via `Trade.IsSelfTrade()`) | `NONE` |

In **pro-rata** mode, self orders are skipped rather than STP-cancelled.

### Matching priority

- **FIFO (default):** strict price then time (arrival) priority — the incoming
  order fills the oldest resting order at the best price first.
- **Pro-rata (`ProRata: true`):** at each price level, fills are split across
  resting orders in proportion to their size, with any integer remainder
  distributed so the allocation sums exactly to the traded quantity.

### Price band

A symmetric static collar around the last trade price. `0` disables it (the
default); it also has no effect until the first trade sets a reference. It is
evaluated in the cold reject path only, so it never touches the integer hot path.

---

## `matching.RunnerConfig` — concurrency

Passed to `matching.NewRunner(cfg)`. Use a `Runner` when many goroutines submit
concurrently; the bare `Engine` is single-writer (drive it from one goroutine).

| Field | Type | Purpose | Default |
|-------|------|---------|---------|
| `Engine` | `matching.Config` | The wrapped engine's config | — |
| `QueueSize` | `int` | Command-queue buffer capacity | `0` → `1024` |

Size the queue to cover your worst-case burst × drain latency. See
[INTEGRATION.md → backpressure](INTEGRATION.md#backpressure--queue-sizing).

---

## Order-level options

Set per order (via `types.NewOrder` / `Instrument.NewOrder` and helpers):

| Option | Type | Values | Meaning |
|--------|------|--------|---------|
| `Type` | `OrderType` | `OrderTypeLimit`, `OrderTypeMarket` | rest at a price, or take immediately |
| `Side` | `Side` | `SideBuy`, `SideSell` | direction |
| `TimeInForce` | `TimeInForce` | `TIFGoodTillCancel`, `TIFImmediateOrCancel`, `TIFFillOrKill` | how long it lives (below) |
| `PostOnly` | `bool` | `o.AsPostOnly()` | reject if it would take (maker-only) |
| `ClientOrderID` | `string` | any | optional caller correlation id (for idempotency/dedup in your gateway) |

### Time-in-force

| TIF | Constant | Behavior |
|-----|----------|----------|
| Good-till-cancel | `TIFGoodTillCancel` | rest until filled or cancelled |
| Immediate-or-cancel | `TIFImmediateOrCancel` | fill what's available now, cancel the rest (never rests) |
| Fill-or-kill | `TIFFillOrKill` | fill the **entire** order atomically or reject it (no partial, book unchanged) |

---

## Order types (the real-world surface)

All constructed in `types`, submitted via the matching-`Process*` methods:

| Type | Constructor | Submit with |
|------|-------------|-------------|
| Limit / Market | `NewOrder(...)` | `Process` / `Match` |
| Stop / stop-limit | `NewStopOrder(order, stopPriceTicks)` | `ProcessStop` |
| Iceberg | `NewIcebergOrder(order, displayLots)` | `ProcessIceberg` |
| Post-only | `order.AsPostOnly()` | `Process` |
| Pegged | `NewPeggedOrder(order, ref, offsetTicks)` — `ref` ∈ `PegToBid/PegToAsk/PegToMid` | `ProcessPegged` |
| OCO / bracket | `NewOCOOrder(primary, stop)` | `ProcessOCO` |
| Trailing stop | `NewTrailingStop(order, trailTicks)` | `ProcessTrailingStop` |

See [SPEC.md §5](SPEC.md#5-the-order-model-real-world-surface) for semantics.

---

## Validation & errors

Construction and matching raise typed sentinel errors (use `errors.Is`):

`ErrInvalidPrice`, `ErrInvalidQuantity`, `ErrInvalidSide`, `ErrInvalidOrderType`,
`ErrInvalidTimeInForce`, `ErrInvalidStopPrice`, `ErrInvalidDisplayQuantity`,
`ErrInvalidPegReference`, `ErrOrderBookFull`, `ErrOrderNotFound`,
`ErrOrderNotActive`, `ErrMarketOrderNoLiquidity`, `ErrFOKCannotFill`,
`ErrPostOnlyWouldCross`, `ErrTradingHalted`, `ErrPriceOutsideBand`,
`ErrPegReferenceUnavailable`.

Rules enforced by the core: quantity > 0; limit price > 0 (market price forced to
0); side/type/TIF must be valid; stop/trail/display-qty positive; a full book
rejects new inserts.

---

## What the core deliberately does **not** configure

These belong to the gateway/risk/session layers above the engine — keep them
there. Documenting the boundary is itself the point.

| Concern | Why it's out of core | Where it goes |
|---------|----------------------|---------------|
| **Fees / rebates** | post-trade pricing, not matching | settlement layer (core emits maker/taker + fill price) |
| **Credit / margin / buying power** | account state, per-venue | pre-trade risk in the gateway |
| **Max order rate / message limits** | connection-level throttling | gateway |
| **Max position / notional caps** | account exposure | pre-trade risk |
| **Authentication / sessions** | connection concern | gateway |
| **Persistence backend & replication** | I/O must stay off the hot path | a downstream journaler stage (the core produces the event stream) |
| **Auction/session orchestration** | venue schedule | a session controller driving the engine |

### On the roadmap (production knobs not yet modeled)

Tracked for future config additions (see the repo roadmap): per-instrument
min/max order size and min-notional validation; `GTD`/`DAY` TIF with expiry; a
`DECREMENT` STP mode and cross-account `tradeGroupId`; an instrument
status/halt enum (`open`/`halt`/`cancel_only`/`post_only`/`reduce_only`); a
dynamic/asymmetric price band and a market-order slippage collar; and richer
pro-rata parameters (top-order %, minimum allocation). File an issue if you need
one prioritized.

# Learn: Order Books & Market Making from Scratch

No finance background needed. This walks from "what is an order book" to "how
market makers actually make money" — and points at the live demo and the code so
you can see each idea running.

> 🎮 **Play along:** the [live demo](https://intrepidkarthi.github.io/orderbook/)
> runs this exact engine (compiled to WebAssembly) in your browser. Place orders
> in the **Live book** scene; run a market maker in the **Market maker** scene.

---

## Part 1 — The order book

### The idea

An **order book** is just a list of who wants to buy and who wants to sell,
organized by price. That's it. Two sides:

- **Bids** — buy orders, stacked from highest price down.
- **Asks** (or offers) — sell orders, stacked from lowest price up.

```
        PRICE     SIZE
  asks  102.0      5     ← sellers (lowest = best ask)
        101.5      2
        101.0      3     ← best ask (cheapest you can buy)
        ── spread 1.0 · mid 100.5 ──
        100.0      4     ← best bid (most you can sell for)
  bids   99.5      3
         99.0      6     ← buyers (highest = best bid)
```

Three things to read off it immediately:

- **Best bid / best ask** — the highest buy and lowest sell. The trade happens
  here first.
- **Spread** — best ask − best bid (here `1.0`). Tight spread = liquid, active
  market. Wide spread = thin, risky.
- **Depth** — how much size is resting at each level. Deep book = hard to move
  the price; thin book = a modest order swings it.

> In code: [`pkg/orderbook`](../pkg/orderbook) keeps each side as a map of
> price → level plus a sorted price ladder, so the best price is O(1) to read.

### Two ways to trade: maker vs taker

- A **limit order** says "buy/sell *only* at this price or better." If it can't
  match right now, it **rests** in the book — you are a **maker** (you *made*
  liquidity for others).
- A **market order** says "buy/sell *right now* at whatever's available." It
  crosses the spread and trades against resting orders — you are a **taker** (you
  *took* liquidity).

Makers wait and may not get filled; takers get immediacy but pay the spread.
Exchanges usually charge takers more (and sometimes pay makers a rebate) to
encourage resting liquidity.

### How a match happens: price–time priority

When an order can trade, the engine matches it **best price first**, and among
orders at the same price, **first come first served** (FIFO / time priority).

```
A market BUY for 4 arrives. Best asks: 101.0 (size 3), then 101.5 (size 2).
  → fill 3 @ 101.0   (that level is exhausted)
  → fill 1 @ 101.5   (2 → 1 left)
Trades print at the *maker's* price. The buyer "walked the book" and paid a
worse average price than the top — that's slippage.
```

> In code: [`pkg/matching`](../pkg/matching) — the `match` loop peeks the best
> resting order, checks the price crosses, and fills `min(taker, maker)` until
> the taker is done or the book stops crossing.

### Order types you'll meet

| Type | What it does |
|------|--------------|
| **Market** | Take immediately, accept slippage. |
| **Limit** | Rest at a price (or better). |
| **Stop / stop-limit** | Dormant until price *reaches* a trigger, then fires (classic stop-loss). |
| **Iceberg** | Shows only a small slice; the hidden rest refills — so big players don't reveal their size. |
| **Post-only** | Maker-only; rejected if it would cross (guarantees the maker fee/rebate). |
| **OCO / bracket** | Two linked orders (take-profit + stop-loss); one filling cancels the other. |
| **Pegged / trailing** | Price tracks a reference (mid/bid/ask) or trails the market. |

Time-in-force controls lifetime: **GTC** (rest until cancelled), **IOC** (fill
now, cancel the rest), **FOK** (fill *entirely* now or cancel all).

---

## Part 2 — Market making

### What a market maker actually does

A **market maker (MM)** continuously posts *both* a bid and an ask, and profits
from the **spread** between them. Buy at 99.9, sell at 100.1, pocket 0.2 — over
and over. The MM isn't betting on direction; it's renting out *immediacy* to
impatient traders.

Sounds like free money. It isn't. Two risks make it a real job:

**1. Inventory risk.** Every fill pushes your position away from flat. If buyers
keep lifting your ask you pile up a short; if sellers keep hitting your bid you
pile up a long — right as the price drifts against you. You wanted to earn the
spread, not take a directional bet.

**2. Adverse selection.** The trades that fill you are disproportionately the
ones about to be *right*. Someone with better information picks off your stale
quote. The spread that's fine for random ("uninformed") flow is too tight when
informed flow shows up.

Managing those two risks *is* market making.

### Avellaneda–Stoikov: quoting with a brain

The [Avellaneda–Stoikov model](https://www.math.nyu.edu/~avellane/HighFrequencyTrading.pdf)
(2008) gives a principled answer to "where exactly do I quote?" Two ideas:

**Reservation price** — your *inventory-adjusted* fair value:

```
reservation = mid − inventory × γ × σ² × (time_left)
```

When you're long, the reservation price sits *below* mid, so both your quotes
shift down — you're keener to sell than buy, nudging inventory back to flat. `γ`
(gamma) is how aggressively you do this; `σ` is volatility.

**Optimal spread** — how wide to quote:

```
spread = γ × σ² × (time_left)  +  (2/γ) × ln(1 + γ/k)
```

The first term charges for inventory risk (wider when volatile); the second is
the order-flow economics (`k` = how sensitive fills are to your spread). Quote
too tight and you get run over; too wide and nobody trades with you.

> In code: [`pkg/strategy`](../pkg/strategy) is the quoter;
> [`pkg/backtest`](../pkg/backtest) runs it against simulated flow and scores it
> on inventory, PnL, and Sharpe. Try the **Market maker** scene in the demo and
> move the γ / k / σ sliders.

### The honest part

Two things the internet usually skips:

- **The formula is necessary, not sufficient.** In a benign simulation with
  uninformed flow, an AS maker captures spread and keeps inventory near flat —
  that's the *baseline*. Real edge also leans on fees/rebates, queue position,
  and speed. The model is the skeleton, not the whole animal.

- **"The order book predicts the next move" is mostly false.** Order-flow
  imbalance (OFI) has a *strong* relationship to price — but **contemporaneously**
  (over the *same* instant), which is nearly mechanical. As a *forecast* of the
  *next* move it's close to nothing. We tested exactly this in
  [`pkg/study`](../pkg/study): contemporaneous R² ≈ **0.33**, predictive R² ≈
  **0.00**. Run it: `go run ./cmd/ofistudy`. See
  [research-roadmap.md](research-roadmap.md).

---

## Where to go next

- **Run it:** `go run ./cmd/obdemo` (matching), `./cmd/obmm` (market maker),
  `./cmd/ofistudy` (the OFI experiment on simulated data), `./cmd/l2capture`
  (**live** OFI on real Coinbase data), `./cmd/surveil` (spoofing detection).
- **Read the design:** [SPEC.md](SPEC.md).
- **The research agenda:** [research-roadmap.md](research-roadmap.md).
- **Play:** the [live demo](https://intrepidkarthi.github.io/orderbook/).

*Nothing here is financial advice — it's how the machinery works.*

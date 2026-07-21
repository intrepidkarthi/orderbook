# Demo Spec — Animated, Hosted Explainer

> A web demo that runs the **real Go engine in the browser** (via WebAssembly)
> and teaches — with animation — how an order book works, how matching works,
> and how market making works. Free, always-on, zero-ops.

Status: **draft v0.1** · Author: Karthikeyan NG · Companion to
[`SPEC.md`](SPEC.md) and [`research-roadmap.md`](research-roadmap.md).

---

## 1. Why this exists

Most explanations of order books are static diagrams or 30-second clips that
point at a price chart. The thing that actually produces price — the live limit
order book — is rarely shown *moving*. This demo shows it moving, driven by the
same engine the library ships, so what you watch is real behavior, not a
hand-drawn approximation.

Three audiences:
- **Curious newcomers** — "what *is* an order book?"
- **Traders** — "what do OFI, absorption, and market making actually look like?"
- **Engineers / recruiters** — "the same Go engine you can `go get` is running
  in this browser tab."

---

## 2. Design principles

1. **Real engine, real behavior.** The core compiles to WASM; the UI never fakes
   matching. If the demo shows a fill, the engine produced it.
2. **Show, then explain.** Every scene animates first; text is a side panel, not
   a wall.
3. **Playable, not just watchable.** Play / pause / **step** / speed, and
   controls to place your own orders and tweak parameters.
4. **Honest.** Where a scene touches a contested claim (OFI predictiveness,
   "trapped traders"), the caveat is in the scene, not buried.
5. **One visual language.** Bids green, asks red, trades gold, hidden/iceberg
   dimmed, spoof orders flagged. Consistent across every scene. Light + dark,
   reduced-motion respected.

---

> **Implementation note (v1 shipped).** The first cut in [`../web`](../web) is a
> **zero-build static site** (vanilla HTML/CSS/JS + Go→WASM) rather than
> React/Vite — it deploys to Pages with no toolchain and was verified end-to-end
> (the WASM engine runs headlessly in Node). The React/Vite multi-scene build
> below remains the target for the richer scenes; it reuses the same WASM bridge.

## 3. Technical architecture

```
        ┌─────────────────────────────────────────────┐
        │  web/  (React + TypeScript + Vite)           │
        │                                              │
        │   Scenes ── Canvas/SVG + D3 + Framer Motion  │
        │      │                                       │
        │      ▼                                       │
        │   engine bridge (TS)  ◀──────────┐           │
        └───────────────│──────────────────│──────────┘
                        │ calls             │ events/state
                        ▼                   │
        ┌───────────────────────────────────────────────┐
        │  obwasm  (Go compiled to GOOS=js GOARCH=wasm)  │
        │  thin syscall/js bindings over pkg/matching,   │
        │  pkg/orderbook, pkg/signals, pkg/strategy      │
        └───────────────────────────────────────────────┘
```

- **Engine → WASM.** `cmd/obwasm` exposes a minimal, explicit API to JS
  (`newBook`, `submit`, `cancel`, `snapshot`, `step`, `signals`, `quote`). Data
  crosses as compact JSON or typed arrays; the boundary is small and versioned.
- **Frontend.** React + TypeScript + **Vite**. **Tailwind** for styling.
  **Framer Motion** for UI/element motion. **Canvas** (with a thin scene graph)
  for the high-frequency ladder/tape animation; **SVG + D3** for depth curves and
  axes; **TradingView `lightweight-charts`** for price/PnL time series.
- **State.** A small store (Zustand) holds sim clock, selected scene, params.
- **Determinism.** Scenes seed the engine's synthetic order flow with a fixed
  seed so a given scene animates identically every load (and matches the Go
  tests).
- **No backend required** for v1 — everything is client-side.

**Deferred (v2):** an optional live Go WebSocket backend (`cmd/observer`) for a
"continuously running simulated market" and multi-user shared books, hostable on
Fly.io/Render. The frontend already speaks to an engine behind an interface, so
swapping WASM-local for WS-remote is a transport change, not a rewrite.

---

## 4. Hosting & deployment

- **Primary:** **GitHub Pages**, built by a **GitHub Actions** workflow on push
  to `main`. Steps: build Go→WASM, `vite build`, publish `web/dist` to Pages.
- **URL:** `https://intrepidkarthi.github.io/orderbook/` (custom domain optional
  later).
- **Cost/ops:** zero. Static assets + a `.wasm` file (~a few MB, gzipped),
  cache-busted per build.
- **Perf budget:** first meaningful scene interactive in < 2.5 s on a mid laptop;
  WASM lazy-loaded; scenes code-split.

---

## 5. Layout & UX

- **Left rail:** scene list (progress-style, 1→8), each with a one-line summary.
- **Center stage:** the animation canvas.
- **Right panel:** "What's happening" + "What to notice" + any honest caveat.
- **Bottom bar:** transport (play/pause/step/speed) + scene-specific controls
  (place order, sliders, toggles).
- **Global:** light/dark toggle, reduced-motion honoring, a "show the code"
  affordance linking the relevant Go source on GitHub.

Accessibility: keyboard-drivable transport, ARIA labels on controls, color
choices checked for contrast and color-blind safety (never color as the *only*
channel — shape/label too), `prefers-reduced-motion` swaps animation for stepped
snapshots.

---

## 6. Scene storyboard

Each scene lists: **concept**, **animation**, **interactions**, **what to
notice**, and (where relevant) **honest caveat**.

### Scene 1 — Order Book 101
- **Concept:** bids, asks, spread, depth, best bid/ask.
- **Animation:** a ladder builds up level by level; the spread band and top-of-
  book highlight; sizes bar-scaled.
- **Interactions:** hover a level for cumulative depth; place a resting limit and
  watch it slot in by price then time.
- **Notice:** bids below, asks above, the gap is the spread; deeper = harder to
  move.

### Scene 2 — Placing Orders (limit vs market)
- **Concept:** makers rest, takers cross.
- **Animation:** a **limit** order flies into the ladder and *stays*; a **market**
  order *sweeps* upward, eating levels, each fill flashing gold on the tape.
- **Interactions:** buttons to fire limit/market of adjustable size; watch a big
  market order walk the book and pay slippage.
- **Notice:** market = immediacy + slippage; limit = price control + maybe no
  fill. Maker vs taker.

### Scene 3 — The Match (price–time priority)
- **Concept:** FIFO within a level; partial fills.
- **Animation:** a queue at one price; a crossing order consumes it front-to-
  back; a partial leaves a shrunken resting remainder; trade prints stream.
- **Interactions:** **step** through a match tick by tick.
- **Notice:** first in, first filled; trades print at the *maker's* price.

### Scene 4 — Depth & Liquidity
- **Concept:** cumulative depth, thin vs thick books, slippage.
- **Animation:** the ladder morphs into a cumulative **depth curve**; a big order
  drags a marker up the curve, shading the slippage it pays.
- **Interactions:** a slider thins/thickens the book; re-fire the same order and
  watch impact change.
- **Notice:** same order, different books, very different fills. (Sets up Kyle's
  λ.)

### Scene 5 — Order-Flow Imbalance
- **Concept:** OFI / book imbalance as pressure.
- **Animation:** a live signed **imbalance meter** tracks the ladder; when one
  side stacks faster, the meter leans and the touch tends to move.
- **Interactions:** toggle a synthetic buyer/seller pressure; watch meter + price
  move together.
- **Notice:** pressure often precedes the *tick* mechanically.
- **Honest caveat:** the strong relationship is **contemporaneous**, not a
  next-move oracle — a toggle shows the predictive R² collapsing. Links to the
  research write-up.

### Scene 6 — Price Impact (Kyle's λ)
- **Concept:** impact per unit flow; slice vs one-shot.
- **Animation:** two lanes — one fires a large order in one shot (big impact),
  the other slices it over time (small impact) — impact drawn on the tape.
- **Interactions:** slider for order size and slice count; read realized impact.
- **Notice:** why execution algorithms exist; why your stop in a thin pocket
  moves price.

### Scene 7 — ★ Market Making (Avellaneda–Stoikov)
- **Concept:** the centerpiece — quoting around an inventory-skewed reservation
  price.
- **Animation:** a maker posts bid/ask around a **reservation-price line**; as
  fills accumulate inventory, the line and quotes **skew**; the **spread widens**
  when volatility rises; an **inventory bar** fills/drains; a **PnL curve** ticks;
  adverse-selection fills flash a warning.
- **Interactions:** sliders for **γ** (risk aversion), **k** (arrival), **σ**
  (volatility), and inventory cap; toggle "informed flow" to see adverse
  selection bite; race AS vs a naive fixed-spread quoter side by side.
- **Notice:** the maker isn't predicting direction — it's collecting spread while
  steering inventory back to flat.
- **Honest caveat:** the formula is necessary structure, not a money printer;
  real MM edge leans on fees/rebates and queue position too.

### Scene 8 — Delta, CVD & Absorption ("trapped traders")
- **Concept:** signed aggression, cumulative delta, absorption, squeezes.
- **Animation:** a footprint-style column shows aggressive sells hitting a level
  that *holds* (passive absorption); CVD and price **diverge**; then trapped
  aggressors cover and price squeezes the other way.
- **Interactions:** step the episode; toggle the "true aggressor" vs the
  inferred tick-rule label to see mislabeling.
- **Notice:** the mechanism is real and *possible*.
- **Honest caveat:** the popular narrative is largely post-hoc and
  measurement-noisy; a base-rate readout shows how often "divergence → reversal"
  actually holds.

### Bonus scenes (later)
- **Iceberg & hidden orders:** a big order shows only its tip; the hidden
  remainder refills as it fills — why size hides.
- **Stop-hunt & liquidity pockets:** a cluster of stops gets swept in a cascade,
  then snaps back; cascade-protection halts the chain.
- **Spoofing / layering & surveillance:** fake walls appear then vanish; the
  surveillance layer flags the pattern — how manipulation looks and gets caught.
- **Auctions & circuit breakers:** orders accumulate, then uncross at a single
  price; an LULD band halts a spike.

---

## 7. Build milestones (maps to SPEC §10)

| # | Deliverable |
|---|---|
| M9  | `cmd/obwasm` bridge + `web/` scaffold (Vite/React/TS/Tailwind) + Scenes 1–4. |
| M10 | Scenes 5–8 (OFI, Kyle, Avellaneda–Stoikov, delta/CVD) + Pages deploy + CI. |
| M11+ | Bonus scenes (iceberg, stop-hunt, spoofing/surveillance, auctions); polish, a11y, perf. |

---

## 8. Definition of done (per scene)

- Runs off the real WASM engine (no faked matching).
- Deterministic on reload (seeded).
- Play/pause/step/speed all work; reduced-motion path exists.
- "What to notice" + honest caveat present where relevant.
- Links to the corresponding Go source and, if applicable, the research write-up.
- Passes contrast/color-blind and keyboard checks.

# Console — a live market console in the browser

Status: **SPEC + v1.1 implemented** (critic pass: trading panel, digest, OTR/flood, light-theme + narrow-viewport verified) (`web/console.html`, `cmd/obwasm`) · Author:
Karthikeyan NG · Last updated: 2026-08-03

Companion documents:
- [`DEMO-SPEC.md`](DEMO-SPEC.md) — the *teaching* demo (scripted, narrated). This page
  is the *showcase*: a running market, every feature live.
- [`research-roadmap.md`](research-roadmap.md) — the signals shown here and what they
  honestly do and do not predict.
- [`THREAT-MODEL.md`](THREAT-MODEL.md) — the surveillance detectors the console lets
  you trip on purpose.

---

## 1. Why, and why this shape

The question this answers: can someone who has never read the code see everything the
library does — and find the exact call that does it — in under a minute?

The reference point is VisualHFT: an open-source desktop app that visualizes real-time
market microstructure — depth ladder, order-flow analytics, execution quality, alerts —
fed by exchange connectors. It is the right *idea* for a showcase and the wrong
*architecture* for this project, three times over:

1. **It needs a market to connect to.** This library *is* the market. There is nothing
   to connect: the engine, the agents that trade against it, the signals, and the
   surveillance all compile to WebAssembly and run in the page.
2. **It needs an install.** The showcase's job is "look, quickly" — a static page on
   the existing GitHub Pages site, zero install, zero server, zero new dependency.
3. **It shows markets, not code.** This console's second job is *adaptability*: every
   panel names the library call that produced it, verbatim, so the path from "I see
   the depth ladder" to `engine.Snapshot(10)` is one glance, not a repository search.

The decision, therefore: **a browser console driven by the WASM engine and the
deterministic simulator** — the real engine matching, real `sim.NoiseTrader` agents
providing continuous flow, real `signals` and `surveillance` code computing what the
panels show. Nothing in the page reimplements library logic; the page is a renderer.

A ws-fed dashboard against a live `obgw` (the true VisualHFT analogue for operators)
is deliberately **phase 2**, not v1: browsers do not speak the venue's TCP protocol,
so it needs a websocket proxy — a real component with real decisions — and it
showcases operations rather than the library. Named here so it is a plan, not a gap.

## 2. Panels, and the call each one names

Every panel header carries the producing call. That mapping is the spec:

| Panel | What it shows | The call it names |
|---|---|---|
| Depth ladder | top-10 bids/asks, size bars, mid/spread/last | `engine.Snapshot(depth)` |
| Tape | last trades, aggressor-colored, sized | `MatchResult.Trades`, `Trade.TakerSide` |
| Price | mid sparkline with last-trade ticks | `Snapshot.Mid` |
| OFI | cumulative order-flow imbalance sparkline | `signals.NewOFI().Observe(snap)` |
| CVD | cumulative volume delta sparkline | `signals.NewCVD().Observe(trades)` |
| Imbalance | top-5 depth imbalance, signed gauge | `signals.DepthImbalance(snap, 5)` |
| Kyle λ | rolling price-impact fit, λ and R² | `signals.EstimateLambda(flow, dPrice)` |
| Surveillance | live alert feed | `surveillance.NewMonitor(...).Observe(ev)` |
| Trade as "you" | limit/market entry, resting orders with cancel, ● markers on the ladder | `engine.Process`, `engine.OpenOrdersFor`, `engine.Cancel` |
| Market bar | mid/spread/last/step and the book digest | `EngineSnapshot.Digest` |
| Controls | run/pause/speed/seed, spoof, flood | — |

The **spoof and flood buttons** are the showcase's teeth. Spoof places layered
away-from-touch size under a throwaway account and cancels it seconds later; the
SpoofDetector names the account. Flood fires a burst of far-from-touch IOC
placements that never rest and never fill — quote stuffing's signature — and the
OTRDetector prints its own arithmetic ("30 orders / 0 fills = OTR 30.0, limit
15.0"). The visitor manipulates a market and watches surveillance catch it, in a
browser tab, with the shipping detectors — no mock alert, no scripted timeline.

The **digest in the market bar** makes the determinism claim falsifiable from the
page: same seed, same number of steps, same `EngineSnapshot.Digest` — on any
machine, in any browser.

Honesty rules carried over from the research write-ups: the OFI panel says
*contemporaneous, not predictive* where it shows the signal (the study found a ~540×
R² gap); the λ panel shows R² beside λ rather than implying a clean constant; the
console never claims the noise-trader market contains exploitable signal.

## 3. The bridge (cmd/obwasm), extended

Existing: `obReset`, `obSubmit`, `obSnapshot`. Added, all returning JSON strings:

- `obStep(n)` — advance the simulation n steps: each step the `sim.NoiseTrader`
  agents act on a `sim.View` and their orders go through `engine.Process`; trades
  feed CVD/tick-rule/λ buckets and the surveillance monitor; the snapshot feeds OFI.
  Returns the step count, new trades, and the sequence — the page renders at
  animation-frame cadence and calls this per frame.
- `obSubmit` — unchanged signature, now also returns the order id (so the page can
  cancel), and user orders flow through the same signal/surveillance path as agent
  orders. A visitor's spoof is observed exactly like anything else.
- `obCancel(id, user)` — `engine.Cancel`, ownership enforced, observed by
  surveillance as `OrderCancelled`.
- `obSignals()` — current OFI cumulative, CVD, top-5 and best imbalance, rolling λ
  fit (value, R², points), mid/spread/last.
- `obAlerts(since)` — surveillance alerts after index `since`, so the page drains
  incrementally.
- `obReset(seed)` — rebuild engine, agents, signal state from a seed. Same seed,
  same market, every time — determinism is a feature the console demonstrates by
  putting the seed in the UI.

The bridge holds the step loop rather than calling `sim.Run` because the console
needs a market that advances while the page breathes; it still uses the real
`sim.Agent`/`sim.NoiseTrader`/`sim.View` types, so the flow is the study harness's
flow, not a lookalike.

## 4. Visual bar

The site's existing tokens (`web/style.css` — GitHub-dark palette, `--bid`/`--ask`
greens and reds, light-theme aware) are the console's tokens; the console reads as
another page of the same product, not a bolted-on toy. Canvas sparklines, no chart
library, no external requests. Numbers are set in the monospace stack. Nothing
animates that data did not change.

## 5. Non-goals (v1)

- **No obgw/websocket feed** — phase 2, see §1.
- **No latency histograms in the page.** WASM-in-a-browser timings would be noise
  presented as measurement; the honest numbers live in [BENCHMARKS.md](BENCHMARKS.md)
  and the console links them instead of faking its own.
- **No strategy PnL / backtest UI.** `pkg/backtest` exists, but a PnL panel invites
  "the demo strategy makes money" readings the research docs explicitly refuse.
- **No mobile-first layout.** It degrades acceptably; a ladder wants a desktop.

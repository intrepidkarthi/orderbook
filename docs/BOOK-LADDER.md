# Book Ladder — Where the Book's Time Goes, and the Design That Follows From It

Status: **adopted 2026-10-26 (§6.10)** · Part of M11 in
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) · Author: Karthikeyan NG ·
2026-10-26

## 1. Why this document exists

M11 lists seven data-structure experiments, chosen from the literature before this
engine was profiled closely. This document starts from the profile instead, and asks
which of them the measured costs actually point at. The answer is mostly none of
them. The book's time goes to **looking things up again** rather than to memory layout.

## 2. Evidence

A CPU profile of `BenchmarkTapeReplay/sink=nil` on `bench-v1.obt` (M4, go1.23.5,
10 s, captured 2026-10-25 for [`PGO.md`](PGO.md)). Of 21.8 s sampled, 9.6 s are
inside `Engine.Match`. The rest is the replay harness allocating each command's order,
and the GC and scheduler that this causes. That part is the caller's cost, not the
engine's.

Inside the 9.6 s:

| Where | Time | What it is |
|---|---:|---|
| `OrderBook.Add` | 3.47 s | resting an order |
| ↳ `userTable.intern` | 0.82 s | hashing the account **string** on every add |
| ↳ `mapaccess2_fast64` + `mapassign_fast64` | 1.00 s | the `price → *PriceLevel` Go map |
| ↳ `orderIndex.get` | 0.49 s | the duplicate-id check, **done twice** for a caller-supplied id (`orderbook.go:244`, `:257`) |
| ↳ `orderIndex.put` | 0.36 s | inserting into the id index |
| `Engine.match` | 4.24 s | crossing |
| ↳ `PeekBestAskOrder` + `PeekBestBidOrder` | 1.37 s | of which ~1.1 s is the map lookup `asks[askPrices[0]]` (`:401`) |
| ↳ `OrderBook.Remove` | 0.65 s | id lookup, unlink, delete |
| ↳ `executeTrade` | 0.66 s | the trade itself |
| ↳ `recordLast` | 0.46 s | |
| locks (`RWMutex`) | ~0.15 s | uncontended, as SPEC §6.3 says |

What this shows:

- **About 2.1 s, roughly a fifth of the engine, is the price→level Go map.** It is
  consulted on every peek at the best price and on every add. The sorted price slice
  beside it already knows the best price. It just doesn't hold the level, so every
  access goes through the map.
- **0.82 s is interning the account string**, on every add. The engine already had the
  string when the order arrived.
- **About 0.25 s is a second index lookup** in `Add`, redundant on the engine's path (§5.2).
- **Data layout is not visible here.** M11's arena and hot/cold experiments (4 and 5)
  target cache misses and pointer chasing. This profile can't show either, because
  there is no hardware-counter instrument (M11 says so). They may matter, but nothing
  measured so far points at them.

## 3. Constraints any design must keep

1. **The public API of `pkg/orderbook` is unchanged.** `OrderBook`'s 33 methods,
   `PriceLevel{Price, TotalQty}`, `Config`, the snapshot types. `internal/apicheck`
   enforces this. `pkg/itch` and the examples use it directly.
2. **Same answers.** Every digest (`bench-v1`, `bench-basic-v1`, semcheck corpus), the
   refmatch differential tests and flash1 conformance stay byte-identical. Iteration
   order is part of this: `Orders()`, `Snapshot`, `SnapshotL3` and `GetBidLevels` walk
   best-first, FIFO within a level.
3. **Sparse prices still work.** `pkg/itch` books span a whole stock's price range in
   ticks, so a structure sized by price range (M11 experiment 2, a dense tick array) is
   wrong as the only structure.
4. **Many small books stay small.** `Config.IndexHint` exists because thousands of
   books each with a high cap must not each pay for it.
5. **Single writer, concurrent readers** (SPEC §6.3). The `RWMutex` stays. Its cost
   measured ~1.5%.

## 4. Options considered

| Option | Addresses | Verdict |
|---|---|---|
| **A. Ladder: sorted `[]*PriceLevel` per side, best at the end**, no price map | the 2.1 s map cost, at peek and add | **chosen** (§5) |
| B. Dense tick array + bitmap (M11 exp 2) | the same, faster on a bounded grid | breaks constraint 3 as the only structure. Possible later as a second ladder for configured narrow grids, behind the same internal type |
| C. Adaptive radix tree (M11 exp 3) | sparse price lookup | solves a problem the profile does not show. The ladder's binary search is already O(log P) on the rare non-best access |
| D. Arena + int32 indices (M11 exp 4) | GC scanning, pointer chasing | unmeasured target. Orders are caller-allocated `*types.Order`, which the engine hands back to callers, so an arena would change ownership: a public API break |
| E. Hot/cold split (M11 exp 5) | cache lines per order (`types.Order` is 280 B) | same: unmeasured, and the type is public |
| F. Account handle | the 0.82 s intern | real, but its natural form is a new engine entry point (§7) |

## 5. The design

### 5.1 The ladder (phase 2)

Each side keeps one slice, `[]*PriceLevel`, sorted so the **best level is last**:
bids ascending, asks descending.

- **Best price, best level, front order: O(1)** — `levels[len-1]`, no map. This is the
  peek in every match iteration.
- **Removing the best level: O(1)** — truncate by one. Today that is a memmove of the
  whole price slice, because best is at index 0.
- **Finding a level for an add:** probe from the best end for a few levels (most adds
  land at or near the touch), then binary search. O(log P) worst case; today it is one
  map lookup plus, for a new level, an O(P) insert.
- **Inserting or removing a level away from the best:** a memmove of the levels behind
  it. The same as today's price slice, so no regression.
- **The price map is deleted**, along with its `mapassign` and `mapdelete` cost and the
  memory it pins.
- **Readers** that walk best-first walk the slice backwards. `GetOrdersAtPrice` and
  `DepthWithin` binary search. `Snapshot` and `Orders` produce the same order as now.

`PriceLevel` keeps its exported fields. The level pool and `Prealloc` keep working:
the ladder slices get the capacity `prefill` already gives the price slices.

### 5.2 The redundant lookup (phase 1)

`Add` checks `nodes.get(order.ID)` before and after assigning an id. When the caller
supplied the id, the second check repeats the first, and that is the engine's only
path, since it numbers every order itself. When the book assigns the id, the second
check is **not** redundant: an assigned id can collide with one a caller supplied
earlier. So the second lookup moves inside the assigning branch. A test pins both
cases: a duplicate supplied id is still ignored, and an assigned id that collides is
still refused.

### 5.3 What is deliberately not changed

The id index (`orderIndex`), the node pool, the intrusive FIFO, `types.Order` and the
lock. None of them is where the measured time is.

## 6. How it is checked

### 6.1 Correctness

- **A model test for the ladder.** A plain `map + sort` ladder, about 40 lines in a test
  file, is the oracle. Random adds, cancels, fills and level-emptying sweeps are
  applied to both, and best price, depth, level order and `Orders()` must agree after
  every step. This is how M11's "keep alternatives behind internal implementations"
  is met without a hot-path interface: the old structure survives as the oracle, and
  the bench gate's base-against-head comparison is the A/B. *This is a deliberate
  reading of M11's rule, and is open question 1.*
- **Sabotage:** an off-by-one in the probe, and a reversed walk in `Orders()`, must
  each fail the model test and a digest test.
- **Unchanged:** both tape digests, semcheck, refmatch differential tests, the WAL
  recovery tests, `pkg/itch` replay, and flash1 conformance on CI.

### 6.2 Performance, with the rules set before the data

Each phase is its own commit and is measured on its own: 20 alternating runs of the
old and new test binaries on the M4, plus the gate on push.

- **Phase 1:** `BenchmarkOrderBook_Add`'s median is lower, and no gated benchmark is
  slower than 1.02×. Small, so the bar is only "faster and harms nothing".
- **Phase 2:** `TapeReplay/sink=nil` at least **10%** faster, and `Engine_MatchInto`
  at least 5%. `OrderBook_LevelChurn` no slower than 1.05×, because level insert and
  remove away from the best are the ladder's weak case. Allocations unchanged or lower.
  The profile's 2.1 s bounds the possible gain at about 20% of engine time. 10% asks
  for half of it.
- **Either phase failing** is recorded in this document and reverted, as
  [`PGO.md`](PGO.md) was.

### 6.3 Phase 1, measured (2026-10-26): failed, reverted

Twenty alternating local runs, medians, head ÷ base:

| Benchmark | Ratio |
|---|---:|
| `OrderBook_Add` | 1.037 |
| `Engine_CancelReplaceInto` | 1.129 |
| `Engine_MatchInto` | 1.049 |
| `OrderBook_LevelChurn` | 0.996 |
| `TapeReplay/sink=nil` | 0.983 |

**Fails §6.2:** `Add` is not faster, and two benchmarks are above 1.02. Reverted. The
test from §5.2, which pins duplicate-id refusal on both paths, stays, because it
holds whatever `Add` does.

The numbers say more about the method than about the change. Phase 1 removes one
hash lookup, about 3 ns of a 260 ns add. `Engine_CancelReplaceInto` never reaches
the changed line, yet read 1.129. Its own runs spread from 136 to 281 ns in the old
build and 139 to 337 ns in the new one. On this laptop, a change smaller than about
10% cannot be told from noise by twenty local runs. That is also the size of phase 2's
bar.

### 6.4 Phase 2's measurement, restated before its data

Written after §6.3 and before any phase 2 code is built.

- **Judged on CI, not the laptop.** Three `bench-gate.yml` dispatches in `compare`
  mode, base the commit before the ladder and head the ladder, on CI's go1.27. The
  medians are the judge's, from interleaved pairs on one runner. In the PGO runs they
  agreed across dispatches to about ±2% (PGO §6.2).
- **The thresholds of §6.2 are unchanged** and must hold in all three runs:
  `TapeReplay/sink=nil` ≤ 0.90, `Engine_MatchInto` ≤ 0.95, `OrderBook_LevelChurn`
  ≤ 1.05, allocations unchanged or lower, digests equal.
- **The code lives on a branch** until it passes, so a failure leaves `main` as it was.
- Twenty local runs are still made and reported, but decide nothing.

### 6.5 Phase 2, first version (2026-10-26): the gate fails it

Three CI dispatches, base `720d26c`, head `effd11e`
([40283694438](https://github.com/intrepidkarthi/orderbook/actions/runs/40283694438),
[40283697849](https://github.com/intrepidkarthi/orderbook/actions/runs/40283697849),
[40283701012](https://github.com/intrepidkarthi/orderbook/actions/runs/40283701012)):

| Benchmark | Run 1 | Run 2 | Run 3 | §6.4 |
|---|---:|---:|---:|---|
| `TapeReplay/sink=nil` | 0.862 | 0.858 | 0.852 | ≤ 0.90, passes |
| `Engine_MatchInto` | 0.915 | 0.872 | 0.885 | ≤ 0.95, passes |
| `OrderBook_LevelChurn` | 0.879 | 0.851 | 0.881 | ≤ 1.05, passes |
| `Engine_CancelReplaceInto` | **1.102** | **1.120** | **1.117** | not in §6.4; **the gate fails it** |

Digests agreed. Allocations on the tape fell from 9,067 to 9,055.

§6.4's own thresholds pass, but it named three benchmarks, and the gate enforces a
fourth. A push of this version would fail on `main`. It is not adopted. §6.4 should
have required every enforced benchmark to pass, and now does (§6.6).

**Why.** `Engine_CancelReplaceInto` keeps 5,000 bids on 2,000 levels and cancels and
re-adds at random depth, almost never near the best. That is the ladder's weak case:
a probe that misses, then a binary search over 2,000 `*PriceLevel` pointers, reading
`.Price` through each one. Profiled at a bounded iteration count, `ladder.search` was
80 ms of flat time where the map lookup had been 10 ms.

**The revision.** The ladder keeps its keys in a contiguous `[]int64`, in step with the
level pointers, and searches only the keys. That is eight prices per cache line and no
pointer read per comparison. The model test passes. The probe off-by-one, and a new
sabotage that leaves a removed level's key behind, each fail the model test, the tape
digests and the differential tests.

### 6.6 The revision's rule, before its data

§6.4 unchanged, plus: **every benchmark the gate enforces passes in all three runs.**
Today that is `Engine_CancelReplaceInto`, `Engine_MatchInto`, `TapeReplay/sink=nil`
and `OrderBook_LevelChurn`, and allocations for every gated benchmark.

### 6.7 The revision, measured (2026-10-26): fails, worse

Three CI dispatches, base `720d26c`, head `7f2cfb0`
([40284461396](https://github.com/intrepidkarthi/orderbook/actions/runs/40284461396),
[40284464561](https://github.com/intrepidkarthi/orderbook/actions/runs/40284464561),
[40284467930](https://github.com/intrepidkarthi/orderbook/actions/runs/40284467930)):

| Benchmark | Run 1 | Run 2 | Run 3 | §6.6 |
|---|---:|---:|---:|---|
| `TapeReplay/sink=nil` | 0.880 | 0.876 | 0.868 | passes |
| `Engine_MatchInto` | 0.908 | 0.906 | 0.929 | passes |
| `Engine_CancelReplaceInto` | 1.106 | 1.110 | 1.143 | **fails** |
| `OrderBook_LevelChurn` | 1.327 | 1.316 | 1.333 | **fails** |

Not adopted. §6.5's diagnosis was wrong. A deep lookup does not cost the pointer read.
It costs the binary search itself: about eleven data-dependent branches over 2,000
prices, against one hash of an integer key. A sorted array loses that comparison
however it is laid out. The second slice also doubled the copying on every level
insert and removal, which is what `LevelChurn` measures.

**What the two versions establish together:**

- Removing the map from **the best-level path** is worth 12-15% on the tape and 7-13%
  on `Engine_MatchInto`, consistently, on two layouts.
- Removing it from **deep lookups** costs 10-14% where a book is wide and accessed at
  random depth. That is the cancel-heavy pattern of a market maker re-quoting across
  many levels.
- So the map should not go. It should stop being on the path that reads the best.

§8.4 proposes that design. It needs a decision before any more code.

### 6.8 The hybrid, measured (2026-10-26): fails, and the trade-off is real

Three CI dispatches, base `720d26c`, head `95b3b2e`
([40298147424](https://github.com/intrepidkarthi/orderbook/actions/runs/40298147424),
[40298151367](https://github.com/intrepidkarthi/orderbook/actions/runs/40298151367),
[40298155472](https://github.com/intrepidkarthi/orderbook/actions/runs/40298155472)):

| Benchmark | Run 1 | Run 2 | Run 3 | §6.6 |
|---|---:|---:|---:|---|
| `TapeReplay/sink=nil` | 0.920 | 0.947 | 0.972 | ≤ 0.90, **fails** |
| `Engine_MatchInto` | 1.044 | 1.044 | 1.041 | ≤ 0.95, **fails** |
| `Engine_CancelReplaceInto` | **1.110** | 1.041 | 1.041 | the gate fails run 1 |
| `OrderBook_LevelChurn` | 1.040 | 1.037 | 1.038 | ≤ 1.05, passes |

Not adopted. §6.7's attribution was partly wrong too. Most of the first ladder's gain
was not the map-free peek. It was creating and destroying price levels without a map
write and a map delete. `Engine_MatchInto` does both on every op, and the hybrid puts
them back.

**What three versions establish:**

| | No map (v1) | Today (map + price slice) | Hybrid |
|---|---|---|---|
| Top-of-book work: tape replay, match, level churn | **12-15% faster** | — | 3-8% faster on the tape, ~4% slower elsewhere |
| Deep random cancel-and-replace on a wide book | **10-12% slower** | — | about even |

No layout tried here wins both. **Which workload matters more is a product decision,
not a measurement**, and this document stops there (§8.5).

### 6.9 Real order flow decides between v1 and today (rule before data)

§8.5 was decided on 2026-10-26 as: v1 if real flow does not penalise it, else today's
structure. "Activity clusters near the touch" is the argument for v1, and it is a
prediction of the kind that §6.5 and §6.7 got wrong. So it is measured.

- **Data:** the first 50 M messages of a real NASDAQ TotalView-ITCH 5.0 day
  (`01302019.NASDAQ_ITCH50.gz`, the file `itch.yml` already replays). Every stock's
  book is built, as `pkg/itch` does: real adds, executions, cancels, deletes and
  replaces, at the depths they actually land at.
- **What is timed:** `itchbook -replays 3` decodes the messages into memory once,
  then builds every book from them three times, each from empty, timing only the
  builds. Download, decompression and decoding are outside the clock.
- **Arms:** `itchbook` built from `main` at `720d26c` (today) and from `effd11e`
  (v1), on one runner, ten invocations of each in ABBA order. A pair's ratio is head's
  median build time over base's. The result is the median over the ten pairs.
- **Correctness on real data:** both arms' summaries (message counts by type, the
  anomaly counters, the number of books, and AAPL's top ten levels) must be
  byte-identical.
- **Runs:** three dispatches of `.github/workflows/itch-ab.yml`.
- **Rule:** v1 is adopted if the result is **≤ 1.02 in all three runs** and the
  summaries are identical. The synthetic benchmarks of §6.5 then stand as measured:
  `Engine_CancelReplaceInto` is accepted with a `Bench-Accept` trailer, and the gate's
  other enforced benchmarks must pass. Otherwise today's structure stays and the
  branch is closed.

### 6.10 Real order flow, measured (2026-10-26): v1 adopted

Three dispatches of `itch-ab.yml`, base `720d26c`, head `effd11e`, 50 M messages of
`01302019.NASDAQ_ITCH50.gz`, ten ABBA pairs each
([40359794185](https://github.com/intrepidkarthi/orderbook/actions/runs/40359794185),
[40359800349](https://github.com/intrepidkarthi/orderbook/actions/runs/40359800349),
[40359806425](https://github.com/intrepidkarthi/orderbook/actions/runs/40359806425)):

| Run | Base, ns/message | v1, ns/message | Median of 20 paired ratios | Range |
|---|---:|---:|---:|---|
| 1 | 9.27 | 8.66 | **0.935** | 0.906-0.996 |
| 2 | 5.76 | 5.37 | **0.930** | 0.882-0.953 |
| 3 | 4.99 | 4.66 | **0.931** | 0.872-0.970 |

Both arms built byte-identical books in every invocation. **§6.9 passes: v1 is
adopted** (`1c495af`), with `Bench-Accept: BenchmarkEngine_CancelReplaceInto
ratio<=1.18` for the deep random re-quote that §6.5 measured at 1.10-1.12.

On real flow v1 is not only no slower but 7% faster, which is consistent with §6.5's
synthetic top-of-book results. The prediction behind it, that real activity sits near
the touch, held this time. It was measured before it was relied on, because the two
predictions before it had not held.

The three rejected versions stay on the `book-ladder` branch, with the runs that
judged them.

## 7. Later, each with its own design

- **Account handles.** An engine entry point taking an account already interned to an
  integer would remove the 0.82 s. FIX, the C ABI and `obgw` all know the account
  before the order arrives. It is a public API addition, and needs its own document.
- **A front-of-book cursor for the match loop.** After the ladder, each fill still
  does `Remove(maker.ID)` or `UpdateOrderQuantity(maker.ID, …)`: an id lookup for the
  order the loop is already holding. Removing that means exported methods that act on
  the current front, which is an API addition. Measure what remains after phase 2
  first.
- **Arena and hot/cold (M11 4, 5)** after a hardware-counter instrument exists (Linux
  `perf` on a runner), so there is something to point them at.

## 8. Open questions for review

### 8.5 After §6.8: which workload to favour

*Decided 2026-10-26: option 1 (v1), on condition that real order flow does not
penalise it, as measured under §6.9. Option 3 is declined: a knob users cannot reason
about without this analysis, two structures to keep correct, and new public API.*

1. **Adopt v1 (no map)** with a `Bench-Accept: Engine_CancelReplaceInto ratio<=1.25`
   trailer (BENCH-GATE §5.3). Faster wherever activity is near the touch, which is
   most venues, the tape and every flash1 scenario. Slower for a market maker
   re-quoting at random depth across thousands of levels.
2. **Keep today's structure.** Record the result, and the model test stays as a
   guard.
3. **Make it a `Config` choice**, defaulting to today's. An API addition, and two
   structures to keep correct, which the model test makes affordable.

### 8.4 After §6.7: a hybrid, proposed and accepted 2026-10-26

Keep the `price → level` map **and** the ladder of level pointers, best last. One
slice: the price slice becomes the level slice, so there is no second slice to copy.

- **Best level, peek, front order:** the ladder's last element. No map.
- **Add:** probe the top four levels of the ladder first. On a miss, look up the map,
  as today. A new level is inserted in both, as today.
- **Remove of an emptied level:** at the best, a truncation plus a map delete.
  Elsewhere, a map delete plus the same memmove as today's price slice.
- **Expected:** the best-level gain of §6.5 on the tape and `MatchInto`. Deep
  cancel-and-replace and level churn back to today's cost: the same map operations and
  one slice moved. This is a prediction, and §6.7 shows how a prediction here can be
  wrong.
- **Judged** under §6.6 as it stands, on three CI dispatches.


*Answered 2026-10-26: the proposals stand. (1) The model-test oracle plus the gate's
base-against-head comparison is the A/B; no build tag. (2) Phases 1 and 2 only; §7
waits for its own designs. (3) 10% on the tape replay.*


1. **M11's interface rule.** Is the model-test oracle plus the gate's base-against-head
   A/B an acceptable reading, or should the old ladder stay compiled in, behind a
   build tag, for side-by-side runs?
2. **Scope.** Phases 1 and 2 only, with §7 deferred? Or design the account handle now,
   since it is the next largest single cost?
3. **The phase 2 bar.** Is 10% on the tape replay the right threshold?

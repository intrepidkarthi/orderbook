# Book Ladder — Where the Book's Time Goes, and the Design That Follows From It

Status: **accepted 2026-10-26 (§8 answered); phase 1 measured and reverted (§6.3); phase 2's first version failed the gate (§6.5); the revision is judged under §6.6** · Part of M11 in
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

*Answered 2026-10-26: the proposals stand. (1) The model-test oracle plus the gate's
base-against-head comparison is the A/B; no build tag. (2) Phases 1 and 2 only; §7
waits for its own designs. (3) 10% on the tape replay.*


1. **M11's interface rule.** Is the model-test oracle plus the gate's base-against-head
   A/B an acceptable reading, or should the old ladder stay compiled in, behind a
   build tag, for side-by-side runs?
2. **Scope.** Phases 1 and 2 only, with §7 deferred? Or design the account handle now,
   since it is the next largest single cost?
3. **The phase 2 bar.** Is 10% on the tape replay the right threshold?

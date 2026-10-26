# Snapshot Copy — the Checkpoint's Pause on the Matching Goroutine

Status: **first run failed §5; rule restated in §6.2 before new data** · Written before its code · Author:
Karthikeyan NG · 2026-10-26

## 1. The question

`Runner.Checkpoint` runs `Engine.TakeSnapshot` on the matching goroutine, and trading
waits for it. The disk write that follows runs elsewhere (STAGES §7, corrected
2026-10-25). `BenchmarkTakeSnapshot` puts the pause at about 105-190 ns and one
allocation per resting order on an M4: 0.1 ms at 1,000 orders, 1.1 ms at 10,000,
19 ms at 100,000. This document asks how much of that is allocation, and removes it
if the answer is "most".

## 2. Where the allocations are

For a book of *n* resting orders, `TakeSnapshot` currently makes:

- one slice of *n* pointers in `OrderBook.Orders()`;
- one heap `types.Order` per resting order, in `copyOrder`;
- about log₂ *n* regrowths of `snap.Orders`, which is appended to with no capacity.

## 3. The change

- **One block for the copies.** The resting orders are copied into a single
  `[]types.Order` of length *n*, and `snap.Orders[i]` points at element *i*. That is
  one allocation instead of *n*.
- **`snap.Orders` is made at its final length**, so it never regrows.
- **Nothing else moves.** Stops, trailing stops, icebergs, OCOs and dedup keys are
  few in a normal book and keep their current code. `EngineSnapshot`'s fields and
  types are unchanged, so no caller, file format or `SemanticsVersion` changes.

What a caller could notice: holding any one `*types.Order` from a snapshot keeps the
whole block alive. That is documented on `EngineSnapshot.Orders`. `LoadSnapshot`
already copies each order before it rests, so the block never reaches a live engine.

## 4. Tests, before the change

- **Independence:** a new test takes a snapshot, then fills, partially fills and
  cancels orders it holds, and requires every snapshot order to equal a deep copy made
  right after the snapshot. Sabotage: `copyOrder` returning its argument must fail it.
- **Unchanged:** the existing snapshot round-trip, digest and differential tests, and
  `internal/semcheck`.

## 5. The decision rule, before the data

`BenchmarkTakeSnapshot`, ten alternating runs of the old and new builds on the M4:

1. At 10,000 and 100,000 orders, the new build's median ns/op is at least 20% lower.
2. At 1,000 orders, it is no slower than 1.05× the old build.
3. Allocations per op fall from about *n* to a constant.
4. Every test in §4 passes, and the bench gate passes on the push.

If 1 fails, the change is reverted and §6 records why: the cost is the copy itself,
not allocation, and only a design that avoids copying can remove it (§7).

## 6. Results

### 6.1 The first run, against §5 (2026-10-26)

Ten alternating runs of each build, `BenchmarkTakeSnapshot`, M4, medians:

| Book | Old | New | Ratio | Allocations |
|---|---:|---:|---:|---|
| 1,000 orders | 56.0 µs | 39.3 µs | 0.702 | 1,013 → 4 |
| 10,000 orders | 684.5 µs | 347.8 µs | 0.508 | 10,020 → 4 |
| 100,000 orders | 9.79 ms | 9.83 ms | 1.003 | 100,030 → 4 |

**Against §5 this fails**: criterion 1 asked for 20% at 100,000 orders as well, and
got none. It is recorded as a failure.

The 100,000 row does not say what §5's failure branch assumed, though. Its runs
spread from 8.3 to 14.7 ms in the new build and 9.3 to 14.2 ms in the old one. Each
op allocates about 30 MB, so the benchmark collects garbage on nearly every op, and
that collection, the same size in both builds, is what the row measures. At 10,000
orders, where it does not dominate, half the pause was allocation. A checkpoint in
`obgw` runs every 30 s, not back to back, so the benchmark's garbage rate at that size
is its own, not a venue's.

### 6.2 The rule restated, before its data

Written after §6.1 and before any of the runs it judges. It does not reuse §6.1's
numbers.

- **Fresh runs:** twenty alternating runs per build, binaries rebuilt.
- **At 1,000 and 10,000 orders:** the new build's median is at least 20% lower. That
  replicates §6.1 on new data, or the change is not adopted.
- **At 100,000 orders**, judged with garbage collection out of the timing (`GOGC=off`
  for both builds, so each op times the copy and not the previous op's garbage): the
  new build's median is no slower than 1.05× the old build's.
- §5's criteria 3 and 4 stand.

### 6.3 The restated run

*To be written after the measurement.*

## 7. Not in this change

- **Not copying at all:** copy-on-write book nodes, or building the snapshot from the
  log on another goroutine. A redesign, worth it only for books well past 100,000
  resting orders.
- **The `Orders()` pointer slice:** removing it means a new iteration method on
  `pkg/orderbook`'s public API, which is a decision of its own.

# Last Trade Price — A Lock per Command for One Integer

Status: **bound measured, passes (§4); design in §5, judged under §6** · Part of M11 in
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) · Author: Karthikeyan NG ·
2026-10-27

## 1. The cost

The engine reads the last trade price from its book on every command
(`e.book.LastTradePrice()`, `pkg/matching/engine.go:1096`, and six other places), and
sets it after every batch of trades (`:2078`). Each read takes the book's read lock.
Each set takes the write lock and reads the book's clock, to stamp `lastTradeTime`.

Profiled on `main` (`BenchmarkTapeReplay/sink=nil`, M4, 2026-10-27), the two are
0.36 s of the engine's 4.04 s, about 9%. [`ACCOUNT-HINT.md`](ACCOUNT-HINT.md) §8 found
an M4 profile share of 10% that CI measured at about 3%, so this share is not trusted
until CI has measured it.

## 2. Why it is not simply a field on the engine

The book's `LastTradePrice`, `SetLastTradePrice` and its `Snapshot` are public. A
caller holding `engine.Book()` may read the last price from the book, from another
goroutine, under its lock. So the book's copy must stay right. Whatever the engine
keeps for itself cannot replace it, only spare the engine the lock on its own reads.

## 3. The upper bound, and the rule, before the data

A throwaway branch, `lasttrade-bound`, where the engine keeps the price in its own
field and never reads or writes the book's. It is wrong (the book's copy goes stale),
and it exists only to measure. Three `bench-gate.yml` dispatches against `main`.

**Rule:** a design is written only if `TapeReplay/sink=nil` is **≤ 0.95 in all three
runs**, the bar [`ACCOUNT-HINT.md`](ACCOUNT-HINT.md) used. Below that, this document
records the number and stops.

## 4. The upper bound, measured (2026-10-27): passes

The first three dispatches compared nothing. They named `main`'s tip as the base, and
that tip, which added this document, is not an ancestor of the branch, so the gate
refused: "not compared: no base … That is not a pass." The next three used the commit
the branch came from, `705ea1a`
([40545261940](https://github.com/intrepidkarthi/orderbook/actions/runs/40545261940),
[40545267184](https://github.com/intrepidkarthi/orderbook/actions/runs/40545267184),
[40545272746](https://github.com/intrepidkarthi/orderbook/actions/runs/40545272746)):

| Benchmark | Run 1 (EPYC 9V74) | Run 2 (Xeon 8573C) | Run 3 (EPYC 9V74) |
|---|---:|---:|---:|
| `TapeReplay/sink=nil` | **0.926** | **0.930** | **0.936** |
| `Engine_MatchInto` | 0.830 | 0.773 | 0.781 |
| `Engine_CancelReplaceInto` | 0.969 | 0.959 | 0.981 |
| `OrderBook_LevelChurn` | 0.997 | 1.003 | 0.999 |

≤ 0.95 in all three: §3's rule passes. Unlike `ACCOUNT-HINT.md`, CI found about what
the profile showed.

## 5. The design

Two facts make it small:

- **`lastTradeTime` is never read.** It is an unexported field, written by
  `SetLastTradePrice` and read by nothing in the repository. Every trade batch reads
  the book's clock to stamp it. The field and the clock read go.
- **The last trade price needs no lock of its own.** It becomes an `atomic.Int64`.
  `LastTradePrice` is a load and `SetLastTradePrice` is a store, neither taking the
  book's mutex.

Nothing else changes. The engine keeps calling the book, so there is no second copy of
the price to go stale, which was the throwaway's flaw. The public API is unchanged.
Concurrent readers through `engine.Book()` or `Runner.LastTradePrice()` stay safe,
because an atomic is safe to read from any goroutine.

**What a reader could notice.** `Snapshot` reads the price under the read lock, while
`SetLastTradePrice` no longer takes the write lock. So a snapshot taken while a match
is being applied can pair new levels with the old price, or the reverse. That is
already true today. The engine sets the price in its own lock acquisition after the
fills (`engine.go:2078`), so no snapshot has ever been atomic across the levels and the
price. The change keeps that guarantee as it is and adds no new gap.

## 6. How it is judged, before the data

- Three `bench-gate.yml` dispatches, base the commit before the change.
- `TapeReplay/sink=nil` ≤ 0.95 in all three. That is the bound's bar, applied to the
  real change.
- Every enforced benchmark passes, and digests are equal.
- **Race test:** a test runs `SetLastTradePrice` on one goroutine against
  `LastTradePrice` and `Snapshot` on others. `-race` must be clean, which CI's race
  job enforces.

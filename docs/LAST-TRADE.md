# Last Trade Price — A Lock per Command for One Integer

Status: **measuring the upper bound (§3); no design yet** · Part of M11 in
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

## 4. Result

*To be written after the measurement.*

# ITCH 5.0 Replay — Real Market Data Into the Book

Status: **implemented, real NASDAQ day replayed on CI; step 2.2 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. What and why

NASDAQ TotalView-ITCH 5.0 is the order-level feed most order-book research starts
from, and NASDAQ publishes full trading days of it as sample files. fmstephe's engine,
CppTrader and several Rust books each ship an ITCH replay ([`LANDSCAPE.md`](LANDSCAPE.md)).
This project has none, so nobody can point it at real data.

**ITCH is market data, not order entry.** It reports what the venue's book did: an
order was added, executed, cancelled, deleted or replaced. Rebuilding the book from it
means *applying* those changes, never re-matching them. So the replay drives
`pkg/orderbook`, the book this engine matches against, and not `pkg/matching`. A replay
that matched ITCH adds would invent trades the venue never printed.

## 2. `pkg/itch`

- **Framing.** NASDAQ's sample files put a 2-byte big-endian length before each
  message. `Reader` reads that framing from any `io.Reader`. `Next(*Message)` fills a
  caller-owned `Message` and does not allocate.
- **Messages decoded:**

  | Type | Name | Bytes | Fields kept |
  |---|---|---:|---|
  | `A` | Add Order | 36 | ref, side, shares, stock, price |
  | `F` | Add Order with MPID | 40 | as `A` (attribution ignored) |
  | `E` | Order Executed | 31 | ref, executed shares, match number |
  | `C` | Order Executed with Price | 36 | as `E`, plus printable and execution price |
  | `X` | Order Cancel | 23 | ref, cancelled shares |
  | `D` | Order Delete | 19 | ref |
  | `U` | Order Replace | 35 | original ref, new ref, shares, price |

  Every message also keeps type, stock locate, tracking number and the 6-byte timestamp
  (nanoseconds since midnight). Any other type is returned with only those, so a caller
  can count it. **A known type at the wrong length is an error**, not a guess.
- **`Append(b []byte, m *Message) []byte`** encodes a message, framing included.
  Fixtures are built with it, and decode(encode(m)) = m is tested for every type.
- **Prices** are ITCH `Price(4)`, an integer with 4 implied decimals. That is already an
  integer tick count at 1/10,000 of a dollar, so it goes into the book unchanged.

## 3. Rebuilding the book: `itch.Books`

`Books.Apply(*Message)` keeps one `orderbook.OrderBook` per stock locate:

| Message | Book operation |
|---|---|
| `A`, `F` | `Add` a resting order, id = order reference number |
| `E`, `C` | reduce the order by the executed shares; `Remove` it at zero |
| `X` | reduce it by the cancelled shares; `Remove` it at zero |
| `D` | `Remove` |
| `U` | `Remove` the original, `Add` the new reference at the new price and size, at the back of its level (ITCH: a replace loses priority) |

- A message naming an order the book does not hold is **counted, not fatal**. A file cut
  mid-day contains such messages.
- An execution larger than the order's remaining shares is counted, and removes the
  order.
- **Books are created when a symbol is first seen**, and only for symbols the caller
  selects, or for all of them. `orderbook.Config` gains `IndexHint`, the initial size
  of the order index. Until now `MaxOrders` set that size as well as the cap, and
  8,000 books sized for their busiest symbol would not fit in memory. `IndexHint` 0
  keeps the old behaviour, so nothing that exists changes.
- Removed orders are reused, so a steady-state replay does not allocate per message.

## 4. `cmd/itchbook`

```
itchbook [-symbol AAPL] [-levels 5] [-n MAX] FILE|-        (gzip read if FILE ends .gz)
```

It prints the message counts by type, the anomaly counters, the replay rate, and the
final book of the selected symbol to `-levels` levels.

## 5. How it is tested

- **Round trip.** Every decoded type goes through `Append` and back unchanged.
- **Wrong lengths, a truncated frame, a truncated file**: each is an error naming the
  offset.
- **The book against a model.** A random message stream built with `Append` is replayed
  into `Books` and into a plain-map model (order ref to side, price and shares). After
  every message, each touched level's total quantity and order count must agree, and at
  the end both sides of every book must agree level by level, in priority order. The
  stream includes partial executions, partial cancels, replaces, references never added,
  and over-executions.
- **A committed fixture**, `testdata/sample.itch`: 17 messages written by hand through
  `Append`, plus one system event. Every operation and both anomaly kinds appear. Its
  final book is worked out by hand in the test's comment and asserted row by row.
  `itchbook`'s whole output for it is pinned in `cmd/itchbook`.
- **The layout against the specification's bytes.** One Add Order is written out
  byte by byte from the ITCH field table, so `Append` and `Decode` cannot agree on a
  wrong layout between themselves.
- **Sabotage.** A replace that keeps priority, an execution that does not reduce the
  level total, and a mis-read side must each fail a test.
- **Benchmark.** `BenchmarkApply` replays a 1 M-message synthetic stream: about
  135 ns and 1 allocation per message on an Apple M4. The allocation is the book
  growing, because half the synthetic messages are adds.
  `TestSteadyStateDoesNotAllocate` holds the claim that matters: with an add and a
  delete per step at a fixed book size, nothing is allocated.

## 6. The real file, on CI only

`.github/workflows/itch.yml` runs on dispatch. It streams a NASDAQ sample day from
`emi.nasdaq.com`, keeps the first part of it (an input sets how much), and replays it
with `itchbook`. It reports the counts, the anomaly counters and the rate in the job
summary. It fails only if decoding fails, which would mean the parser and NASDAQ
disagree on the format. Nothing is downloaded on a maintainer's machine. The file is
data, but the rule is kept simple.

**First run** ([37593894826](https://github.com/intrepidkarthi/orderbook/actions/runs/37593894826),
2026-10-07): the first 50,000,000 messages of `01302019.NASDAQ_ITCH50.gz`, 1.55 GB
uncompressed, with AAPL's book built.

- **Every message decoded.** There were 17 types, among them 21.4 M adds, 20.0 M deletes,
  3.9 M replaces, 1.5 M cancels and 0.8 M executions.
- **No anomaly at all.** Not one execution, cancel, delete or replace named an order the
  book did not hold, and none removed more shares than an order had. An index or
  quantity mistake would surface here first.
- **AAPL ends uncrossed.** 39,387 resting orders; best bid 160.83, best ask 160.87.
- **298 ns per message**, end to end on the runner. That includes the network pipe
  and `gzip -dc`, and 48.2 M of the messages were skipped as other stocks. It is a
  pipeline figure, not a book figure; §5's benchmark is the book figure.

## 7. Not in this step

- MOLDUDP64 and SoupBinTCP session framing: live feeds, not files.
- Trade (`P`), cross (`Q`) and NOII messages beyond counting them.
- Matching the replayed flow. That would be a different study: §1 says why.

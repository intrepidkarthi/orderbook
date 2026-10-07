# Cross-Engine Comparison — Same Work, Checked, Then Timed

Status: **running on CI; step 1.5 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. The question, and the one it refuses

**Asked:** on one tape of limit orders and cancels, do these engines produce the same
trades and the same final book as this one? For those that do, how long does each take
to replay the tape on the same machine in the same job?

**Refused:** "which engine is fastest". One tape, one shape of flow, one machine class,
a shared CI runner. The figures rank nothing outside this table, and the README says
so wherever it shows them. [`LANDSCAPE.md`](LANDSCAPE.md) §5 has the published numbers
and why they do not compare. This document exists because they do not.

## 2. The engines

| Engine | Language | Pinned at | Adapter |
|---|---|---|---|
| this one | Go | the commit under test | `cmd/xeng run` |
| [geseq/orderbook](https://github.com/geseq/orderbook) | Go | `088c6cf` (2026-07-25) | `bench/xeng/geseq` |
| [OrderBook-rs](https://github.com/joaquinbejar/OrderBook-rs) | Rust | `54df8eb` (2026-10-05), crate 0.15.0 | `bench/xeng/orderbook-rs` |
| [CppTrader](https://github.com/chronoxor/CppTrader) | C++ | `39421f5` (2026-09-09), gil modules pinned in `fetch.sh` | `bench/xeng/cpptrader` |

Each is pinned to one commit, recorded in its adapter's build file, and bumped
deliberately. All three are MIT. **None of their code is built or run on a maintainer's
machine.** Their source is read through the GitHub API, and every build and run happens
on CI.

## 3. The tape and the protocol

The tape is `bench-basic-v1.obt` ([`BENCH-GATE.md`](BENCH-GATE.md) §17): 50,000 limit
GTC submits and cancels. Every engine can express it, no account meets itself, and
every cancel that names an order comes from that order's owner.

**In.** `xeng export` renders it as lines, one per command:

```
N <count>
S <pos> <acct> <side> <price> <qty>      side 0 buy, 1 sell
C <pos> <acct> <target>                  cancel the order made at position target
```

**Out.** Each adapter prints, after its replay:

```
c <pos> <refused>                        one per command, in tape order
X <price> <qty> <maker> <taker> <B|S>    that command's trades, in the engine's order
L <pos> <side> <price> <qty> <filled>    the final book, read from the engine
E <last trade price> <trade count>
T <replay ns>
```

Orders are named by the tape position that made them, never by an engine's id.

**Folded by one encoder.** `benchgate.DigestText` turns any adapter's output into the
OBDG `core` chain (BENCH-GATE §3.1): refused-or-not per command, every trade, the final
book. It is the same chain the native driver writes. So a digest that differs comes
from the engine or its adapter, never from a second encoder. It is strict: a command
out of order, a trade naming a position that made no order, or a trade count that
disagrees with the trades printed is an error, not a mismatch.

**Checked to be sound.** Two tests in `cmd/xeng`:

- This engine, through the protocol, reproduces the committed `core` digest.
- Fourteen adapter mistakes, applied to a correct output, each either fail or change
  the digest. Among them: a reordered trade, a flipped aggressor, a dropped trade, two
  orders of one level out of priority order, a filled quantity lost.

## 4. Timing

- **The timed region is the replay loop only.** Input is parsed and orders are built
  before the clock starts. Output is recorded into preallocated memory inside it and
  printed after. Each adapter times itself with its language's monotonic clock.
- **One job, interleaved.** Every engine runs once per round, in a rotated order, for
  the same number of rounds, on the same runner. This is the bench gate's reason for
  interleaving (BENCH-GATE §5): a runner's speed drifts within a job, and a block of
  one engine's runs followed by a block of the next would time the drift.
- **Reported:** median, fastest and slowest replay over the rounds, per engine.
  Nothing is gated on time.
- **Only for engines that agree.** An engine whose digest differs in any round is
  listed with its digest and not timed. A time for different work is not a comparison.

## 5. What a figure here does not include, stated against each engine

- **Adapters pay for recording.** Each one appends every trade to an array inside the
  timed loop, because its output must be checked. An engine's own benchmarks may not.
- **Each engine runs the way its README presents it**, with defaults unless the
  adapter says otherwise. One engine's locking, decimal conversion or callback
  dispatch counts against it here, as it would for a user of it.
- **This engine runs with its defaults**, the same configuration the native digest
  driver uses, through `Engine.Match` and `Engine.Cancel`.
- Process start, input parsing and output printing are outside the timed region for
  everyone.

## 6. How it runs

`.github/workflows/xeng.yml` runs on dispatch only. It builds each adapter at its pinned
commit, exports the tape, runs the rounds, and writes `xeng table`'s output to the job
summary. Its outputs are uploaded as artifacts. One run produces the table that step
1.6 of the plan quotes, with its run link and the runner's CPU model.

## 7. Engines that disagree

A disagreement is a finding, not a failure of the job. The table records it. Before
anyone reads it as the other engine's bug, its adapter is checked first, against that
engine's documentation and the first command where the output departs. A confirmed
engine defect is reported upstream, with the command sequence, as flash1 does. An
adapter defect is fixed here, and the run is repeated.

## 8. What each adapter pays inside its timed loop

Read from each engine's source at its pin. A user of that engine pays the same.

- **This engine.** `Engine.Match` and `Engine.Cancel` with the default configuration:
  the self-trade check, the per-account open-order count, the counter clock and the
  trade list. Engine ids become positions after the clock stops.
- **geseq.** `AddOrder` and `CancelOrder` with a monotonic token per call, and an
  interface call per notification. Prices and quantities are fixed-point decimals,
  converted before and after timing. The order pool is prefilled at construction,
  before the clock starts.
- **OrderBook-rs.** `add_limit_order_with_user_and_result` and `cancel_order` on a book
  built for concurrent use: a read lock and per-level lock stripes on every submit, two
  wall-clock reads per submit, a UUID per trade, a cloned `TradeResult` per submit
  that trades, and `DashMap` updates for the order index. Its README offers no
  single-writer path.
- **CppTrader.** `MarketManager::AddOrder` and `DeleteOrder` with matching enabled.
  Each fill reaches the adapter as two `onExecuteOrder` callbacks (maker, then taker),
  which the adapter pairs. After every command the engine also runs its stop-order
  activation pass and its cross-book check, both empty on this tape.

The final book is read from each engine after the clock stops. geseq and OrderBook-rs
keep only an order's remaining quantity, so their adapters take the original from the
tape. CppTrader and this engine report both.

## 9. The first run

[Run 37588247202](https://github.com/intrepidkarthi/orderbook/actions/runs/37588247202),
2026-10-07, AMD EPYC 9V45 (4 vCPUs), 11 rounds interleaved:

| Engine | `core` digest | Median replay | Range |
|---|---|---|---|
| CppTrader | agrees | 3.15 ms | 2.90–3.29 |
| geseq | agrees | 4.25 ms | 3.76–5.28 |
| this engine | agrees | 12.22 ms | 9.94–13.87 |
| OrderBook-rs | agrees | 38.51 ms | 37.86–73.52 |

- **All four agree, on the first run, in every round.** Three engines built by other
  people, in three languages, produce the same 21,856 trades and the same 3,975-order
  final book as this one and as `refmatch`. That is the strongest outside evidence this
  repository has that its price-time matching is the ordinary kind.
- **On this tape this engine is about 3× slower than geseq and 4× slower than
  CppTrader.** Moving the adapter's id mapping out of the loop, as the others do it,
  saved about 0.5 ms locally. The gap is in the engine.
- What the gap is made of is step 3.1's question, and this table does not answer it.


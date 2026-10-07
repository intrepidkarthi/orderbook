# Cross-Engine Comparison — Same Work, Checked, Then Timed

Status: **specified; step 1.5 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
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
| [geseq/orderbook](https://github.com/geseq/orderbook) | Go | see `bench/xeng/geseq` | `bench/xeng/geseq` |
| [OrderBook-rs](https://github.com/joaquinbejar/OrderBook-rs) | Rust | see `bench/xeng/orderbook-rs` | `bench/xeng/orderbook-rs` |
| [CppTrader](https://github.com/chronoxor/CppTrader) | C++ | see `bench/xeng/cpptrader` | `bench/xeng/cpptrader` |

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

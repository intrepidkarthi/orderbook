# flash1 Adapter — the Engine in an Independent Harness

Status: **implemented, checked on CI; step 1.3 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. What and why

[flash1-dev/matching-engine-benchmark](https://github.com/flash1-dev/matching-engine-benchmark)
(MIT) replays about 2.0 M messages per scenario into every engine through one C ABI,
`api/matching_engine_api.h`. It hashes each engine's report stream against a consensus
and publishes worst-case throughput. About 30 Go engines are on its list
([`LANDSCAPE.md`](LANDSCAPE.md) §2). This one is not.

The adapter makes it eligible. It also gives this repository something it has never had:
**an outside oracle** for its matching, written by people who have never read
`engine.go`. That is the second model [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.2
says the differential harness lacks.

## 2. Shape

- **`internal/flash1`** holds the adapter's logic in plain Go: messages in, harness
  reports out, through the public engine API. It is unit-tested here against the
  header's contract.
- **`cmd/flash1engine`** is cgo glue built with `-buildmode=c-shared`. It exports
  `engine_init`, `engine_on_new_order`, `engine_on_cancel`, `engine_on_modify`,
  `engine_on_batch`, `engine_flush`, `engine_query_best_bid`, `engine_query_best_ask`,
  `engine_query_depth_at` and `engine_shutdown`.
- **The ABI structs are declared in our own preamble**, from the header's documented
  layout, with `_Static_assert` on every size (32, 16, 32, 40 and 64 bytes). The header
  is not vendored: per this project's rule, no third-party code is brought onto the
  maintainer's machine. A layout mismatch fails the harness's correctness hash on CI.
- **`engine_on_batch` is exported.** The header says an engine across a foreign runtime
  should do this, or "its figure measures the ABI boundary, not the matcher". Reports
  are buffered per call and handed to the transport in one C call, so the outbound
  crossing is amortised the same way.

## 3. Mapping

| Harness | Engine | Reports, in order |
|---|---|---|
| new order (GTC) | `Match`, limit GTC | `OrderAck`; one `Trade` per fill |
| new order (IOC) | `Match`, limit IOC | `OrderAck`; trades; `CancelAck` for the residual, if any |
| cancel, resting | `Cancel` | `CancelAck` with side, price and remaining quantity |
| cancel, not resting | — | `CancelReject`, all payload zero |
| modify, resting | `Replace` (cancel and re-add, priority lost) | `ModifyAck` with the new price and quantity, **then** the re-added order's trades, carrying the modify's sequence number |
| modify, not resting | — | `ModifyReject`, payload zero |

- **Trade reports.** Every trade carries the incoming order's sequence number, the
  maker's id as `order_id`, the maker's resting price, and `side` 0.
- **Ids.** Order ids are the client's `uint64`. The engine assigns its own ids, and the
  adapter keeps the map both ways.
- **Self-trade prevention is `ALLOW`.** The workload has no accounts, so every order
  comes from one user, and any other mode would cancel orders the harness expects to
  trade.
- **Audit queries** read the engine's own book: best bid and ask from the book, and
  depth at a price summed from that side's levels. The empty sentinels are
  `INT64_MIN` and `INT64_MAX`.

## 4. How it is checked

- **Locally:** `internal/flash1` unit tests cover each row of the table above, priority
  loss on modify, the IOC residual, rejects for unknown and already-cancelled ids, and
  the audit queries. Each test is watched failing against a broken mapping.
- **Across the boundary, locally:** `TestTheCABIEmitsWhatTheAdapterEmits` builds the
  shared library and drives it from a C stand-in for the harness
  (`internal/flash1/testdata/driver.c`), whose transport refuses every fifth push. A
  random 20,000-message stream, through both `engine_on_batch` and the one-message
  entry points with audit queries in between, must print exactly what the Go adapter
  emits. That checks the glue, not the layout: the stand-in and the glue declare the
  structs from the same documentation.
- **On CI:** `.github/workflows/flash1.yml` checks out the harness at a pinned commit
  (`60049226c1a1dad127a50a7c12d62baf0377fbaa`), builds it, builds this adapter, and runs
  all five scenarios in `--mode perf`.
  - **It gates on correctness:** every scenario's report hash must equal the consensus.
  - **Throughput is reported and never gated.** Shared runners are not calibrated, and
    no CI figure is a claim.
  - **The harness's own verdict is reported and not gated.** It also fails a run on core
    pinning and timing checks that a shared runner cannot promise.
  - **The anti-cheat state audit is skipped**, as geseq's own CI skips it, because it
    replays through a Liquibook build.
- **Submission to flash1** is **needs Karthik**.

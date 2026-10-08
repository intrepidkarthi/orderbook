# Adoption Plan — the Go order book you can trust, proven in public

Status: **in progress** · Started 2026-10-07 · Author: Karthikeyan NG

This is the working checklist for the adoption program that follows the landscape
survey in [`LANDSCAPE.md`](LANDSCAPE.md). It is executed one step at a time, and each
step is ticked here, in the commit that finishes it, with the commit named. A step is
not finished until it meets its acceptance criteria. A step that needs a person
(publishing, emailing, scheduling) is marked **needs Karthik** and skipped until it is
done.

The position the plan is built to earn: other Go books are fast and thin (geseq),
complete and GPL (0x5487), or abandoned (i25959341). This one is MIT, carries the widest
feature set and the deepest correctness tooling in the survey, and has never been
compared with anything in a way someone else could re-run. Phase 1 fixes that. Phase 2
makes it easy to adopt. Phase 3 spends engineering where Phase 1's measurements point.

## Rules every step follows

- **No third-party code runs on this machine.** Not built, not run, not in Docker.
  Competitor engines, Docker images and outside harnesses are built and run only on
  GitHub Actions. Their source may be read through the GitHub API.
- **Spec before code** for anything with a design decision, in the repository's house
  style. Tests are watched failing against deliberately broken code before they count
  (`TESTING.md`).
- **Outward actions wait for Karthik**: a PyPI publish, an email, a scheduled routine,
  a release tag.
- Commits are by Karthikeyan NG only, with no co-author line, small and focused. A
  commit that touches `cmd/benchgate` or `bench-gate.yml` carries `Bench-Gate-Change:`.
- Numbers about other projects are quoted from their sources with the conditions
  stated, never re-measured here and never presented as a ranking.

## Phase 1 — credibility

- [x] **1.1 `LANDSCAPE.md`.** *(done 2026-10-07)* The survey of Go, Rust and C++ books (and exchange-core)
  as evidence, every claim cited, dated, with what each published number measures.
  *Done when:* committed and linked from the docs index.
- [x] **1.2 A portable C interface.** *(done 2026-10-07; [`C-API.md`](C-API.md))* A `c-shared` build of the engine behind a small C
  ABI (submit, cancel, replace, events out), in our own code, with a Go test that drives
  it through cgo and checks the OBDG digest of the bench tape matches the native
  engine's. It is shaped so that an adapter for the flash1 harness is a thin shim.
  *Done when:* the digest test passes, sabotaged once.
- [x] **1.3 flash1 adapter.** The shim from 1.2 to flash1's `matching_engine_api.h`
  (read via the API; its license checked before anything is copied), built and checked
  only in CI. *Done when:* a CI job builds it against the pinned header. Then
  **needs Karthik:** submit it to flash1.
  *Done 2026-10-07* ([`FLASH1.md`](FLASH1.md), `.github/workflows/flash1.yml`): the
  first CI run reproduced the consensus hash on all five scenarios, with the harness
  verdict VALID on each.
- [x] **1.4 A second, minimal tape.** `bench-basic-v1.obt`: limit orders and cancels
  only, which every engine in the survey can express, so cross-engine digests are
  comparable. *Done when:* frozen, shape-tested, digest committed, refmatch agrees.
  *Done 2026-10-07* (BENCH-GATE §17): 21,856 trades and 3,975 resting over 50,000
  commands. Every cancel that names an order comes from its owner, so an engine with
  no ownership checks replays it the same way.
- [x] **1.5 Cross-engine comparison on CI.** A dispatch-only workflow that checks out
  pinned commits of geseq/orderbook (Go), OrderBook-rs (Rust) and CppTrader (C++),
  builds a small adapter for each that replays `bench-basic-v1.obt`, checks each
  engine's `core` digest against ours, and times the replay interleaved in one job.
  Engines whose digest differs are reported as disagreeing, not timed. *Done when:* one
  run produces the table, and the document says exactly what it measures.
  *Done 2026-10-07* ([`CROSS-ENGINE.md`](CROSS-ENGINE.md)): all four engines agree on
  the `core` digest in every round of two runs. This engine replays the tape in about
  12.5 ms; geseq in 4.5, CppTrader in 3.5, OrderBook-rs in 40–46. Step 3.1 starts from
  that gap.
- [x] **1.6 README performance and comparison.** A "what each number measures" table,
  percentiles with machine and workload, the 1.5 result linked, a factual feature
  checklist against the surveyed projects, and the liquibook "3×" claim retired or
  qualified with its book depth. *Done when:* every number in the section has a source.
  *Done 2026-10-07*: each figure is tied to its source and machine, the cross-engine
  table links its CI run, every cell of the fact table was read from the engine's
  source or tags, and the liquibook claim is qualified where it was made (CHANGELOG
  v0.13.0, PERFORMANCE-ROADMAP).

## Phase 2 — adoption

- [x] **2.1 Python bindings.** A `pip`-installable package over the 1.2 C interface,
  written with the standard library (`ctypes`) only, with tests and an example notebook
  script. *Done when:* tests pass locally on our own build. Then **needs Karthik:**
  publish to PyPI.
  *Done 2026-10-07* ([`PYTHON.md`](PYTHON.md)): 14 tests, among them bench-basic-v1
  through Python to the committed digest, green locally and on CI (Python 3.9).
- [x] **2.2 ITCH 5.0 replay.** A parser for the add, execute, cancel, delete and
  replace messages, an example that rebuilds a book from a file, and a benchmark.
  Tested on a synthetic fixture; a real NASDAQ sample is fetched and replayed only on CI.
  *Done 2026-10-07* ([`ITCH.md`](ITCH.md)): 50 M messages of a NASDAQ day decoded on CI
  with no anomaly; AAPL's rebuilt book ends uncrossed.
- [x] **2.3 FIX 4.4 order entry** (issue #6). `pkg/fix`: tag=value with BodyLength and
  CheckSum verified, NewOrderSingle and OrderCancelRequest in, ExecutionReports out of
  the event stream. Session layer out of scope. *Done when:* the issue's checklist holds.
  *Done 2026-10-07* ([`FIX.md`](FIX.md), `pkg/fix`): every item on issue #6's list holds;
  a ten-message session is pinned byte for byte, and twelve sabotages are caught.
- [x] **2.4 Exchange in a box.** A compose file that runs the gateway, the dashboard,
  a market maker and the console. Built and smoke-tested only on CI.
  *Done 2026-10-07* ([`EXCHANGE-IN-A-BOX.md`](EXCHANGE-IN-A-BOX.md)): the first CI run built
  it and passed every check: 12 quotes resting, 45 trades in 20 s, the dashboard
  streaming, and 49 orders recovered across a gateway restart. The console in the box
  is the dashboard; the browser console cannot attach to a venue (§2).
- [x] **2.5 README front page.** A quickstart that shows the book before and after each
  call, and an order-type checklist at the top.
  *Done 2026-10-07*: the checklist's rows are read from the code, and the quickstart's
  output is the program's, held to the README by a test.

## Phase 3 — engineering, led by Phase 1's numbers

- [x] **3.1 Read 1.5 and decide.** If a competitor is measurably faster on the shared
  tape, profile the gap and pick the M11 experiment that addresses it; if not, record
  that and skip 3.2.
  *Done 2026-10-07* ([`CROSS-ENGINE.md`](CROSS-ENGINE.md) §10): the profile puts ~40% of
  the loop in resting orders, much of it allocating book nodes that geseq prefills
  before its clock starts. 3.2 is a prefill option for the book's pools.
- [x] **3.2 The chosen optimisation**, behind the bench gate, with the digest unchanged.
  *Done 2026-10-07* ([`CROSS-ENGINE.md`](CROSS-ENGINE.md) §11): prefilled pools, gated
  clean, digests unchanged, 16% off the local replay. The rest of the gap is the cost of
  features geseq lacks, which is a design question, not a pooling one.
- [x] **3.3 Calibration restated on the lower bound.** A spec change to §14.2 of
  `BENCH-GATE.md` and a fresh 60-run calibration, which may enforce more benchmarks.
  *Done 2026-10-07* (BENCH-GATE §18, §19): 60 A/A runs and 40 power runs. Timing is
  enforced on four benchmarks instead of one; `OrderBook_CancelReplace` failed power
  and stays report-only.
- [x] **3.4 Stage attribution.** Queue, match, log and publish delay measured separately.
  *Done 2026-10-07* ([`STAGES.md`](STAGES.md), [`BENCHMARKS.md`](BENCHMARKS.md)): four new
  histograms beside the two WAL ones. The first measurement found `Writer.Sync` holding
  the log's mutex through each `fsync`, which stalls the matcher's next append (queue
  p99 ≤ 5 ms on an M4).
- [x] **3.4a The `fsync` outside the log's lock.** Spec first: what a group commit must
  still guarantee when appends continue during its `fsync`. Durability tests, then the
  stage histograms re-measured. Found by 3.4.
  *Done 2026-10-07* ([`WAL-SYNC.md`](WAL-SYNC.md) §5): WAL append p99 5 ms → 100 µs, the
  client's p99.9 100 → 25 ms; the match stage rose a bucket, recorded as the next thing to
  attribute.
- [ ] **3.5 `pull_request` for the gate**, no earlier than 2026-10-14 and only after a
  week of enforced pushes without a false failure.

## Needs Karthik

Collected here as they come up, so nothing outward happens by default.

- [x] The 2026-10-14 check: a one-time cloud routine, created 2026-10-08, runs at 10:00
  IST and reports whether the `pull_request` trigger may be added
  ([routine](https://claude.ai/code/routines/trig_0179Xh1Cza3a1CQcrm6j7T67)). Read-only.
- [ ] Publish the Python bindings (2.1) as `obook`. Wheels for four platforms are
  built and tested by `python-wheels.yml` (docs/PYTHON.md §6). Left: register the
  PyPI pending publisher (project `obook`, owner `intrepidkarthi`, repo `orderbook`,
  workflow `python-wheels.yml`, environment `pypi`), then dispatch with
  `publish: true`.
- [x] Issue #6 (2.3): closed 2026-10-07, with the checklist mapped to the commits
  and the session layer and OrderCancelReplaceRequest left open for contributors.
- [x] Submit the adapter to flash1 (1.3): opened 2026-10-08 as
  [flash1-dev/matching-engine-benchmark#5](https://github.com/flash1-dev/matching-engine-benchmark/pull/5),
  pinned to `eaf4049`. Whether and how it is listed is flash1's call; watch the PR for
  review comments.
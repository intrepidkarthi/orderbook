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

- [ ] **1.1 `LANDSCAPE.md`.** The survey of Go, Rust and C++ books (and exchange-core)
  as evidence, every claim cited, dated, with what each published number measures.
  *Done when:* committed and linked from the docs index.
- [ ] **1.2 A portable C interface.** A `c-shared` build of the engine behind a small C
  ABI (submit, cancel, replace, events out), in our own code, with a Go test that drives
  it through cgo and checks the OBDG digest of the bench tape matches the native
  engine's. It is shaped so that an adapter for the flash1 harness is a thin shim.
  *Done when:* the digest test passes, sabotaged once.
- [ ] **1.3 flash1 adapter.** The shim from 1.2 to flash1's `matching_engine_api.h`
  (read via the API; its license checked before anything is copied), built and checked
  only in CI. *Done when:* a CI job builds it against the pinned header. Then
  **needs Karthik:** submit it to flash1.
- [ ] **1.4 A second, minimal tape.** `bench-basic-v1.obt`: limit orders and cancels
  only, which every engine in the survey can express, so cross-engine digests are
  comparable. *Done when:* frozen, shape-tested, digest committed, refmatch agrees.
- [ ] **1.5 Cross-engine comparison on CI.** A dispatch-only workflow that checks out
  pinned commits of geseq/orderbook (Go), OrderBook-rs (Rust) and CppTrader (C++),
  builds a small adapter for each that replays `bench-basic-v1.obt`, checks each
  engine's `core` digest against ours, and times the replay interleaved in one job.
  Engines whose digest differs are reported as disagreeing, not timed. *Done when:* one
  run produces the table, and the document says exactly what it measures.
- [ ] **1.6 README performance and comparison.** A "what each number measures" table,
  percentiles with machine and workload, the 1.5 result linked, a factual feature
  checklist against the surveyed projects, and the liquibook "3×" claim retired or
  qualified with its book depth. *Done when:* every number in the section has a source.

## Phase 2 — adoption

- [ ] **2.1 Python bindings.** A `pip`-installable package over the 1.2 C interface,
  written with the standard library (`ctypes`) only, with tests and an example notebook
  script. *Done when:* tests pass locally on our own build. Then **needs Karthik:**
  publish to PyPI.
- [ ] **2.2 ITCH 5.0 replay.** A parser for the add, execute, cancel, delete and
  replace messages, an example that rebuilds a book from a file, and a benchmark.
  Tested on a synthetic fixture; a real NASDAQ sample is fetched and replayed only on CI.
- [ ] **2.3 FIX 4.4 order entry** (issue #6). `pkg/fix`: tag=value with BodyLength and
  CheckSum verified, NewOrderSingle and OrderCancelRequest in, ExecutionReports out of
  the event stream. Session layer out of scope. *Done when:* the issue's checklist holds.
- [ ] **2.4 Exchange in a box.** A compose file that runs the gateway, the dashboard,
  a market maker and the console. Built and smoke-tested only on CI.
- [ ] **2.5 README front page.** A quickstart that shows the book before and after each
  call, and an order-type checklist at the top.

## Phase 3 — engineering, led by Phase 1's numbers

- [ ] **3.1 Read 1.5 and decide.** If a competitor is measurably faster on the shared
  tape, profile the gap and pick the M11 experiment that addresses it; if not, record
  that and skip 3.2.
- [ ] **3.2 The chosen optimisation**, behind the bench gate, with the digest unchanged.
- [ ] **3.3 Calibration restated on the lower bound.** A spec change to §14.2 of
  `BENCH-GATE.md` and a fresh 60-run calibration, which may enforce more benchmarks.
- [ ] **3.4 Stage attribution.** Queue, match, log and publish delay measured separately.
- [ ] **3.5 `pull_request` for the gate**, no earlier than 2026-10-14 and only after a
  week of enforced pushes without a false failure.

## Needs Karthik

Collected here as they come up, so nothing outward happens by default.

- [ ] Reply on the scheduled 2026-10-14 check (routine proposed, not created).

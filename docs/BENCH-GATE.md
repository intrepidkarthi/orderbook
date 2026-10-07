# Benchmark Gate — A Slowdown That Can Fail a Build, and a Tape Another Matcher Can Replay

Status: **built: slice A, and slice B's calibration and enforcement (§15); `pull_request` pending** — part of milestone M10 in
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md), written before the code, as this
repository does it · Author: Karthikeyan NG · Last updated: 2026-10-06

> **Position as written.** `bench.yml` runs `go test -bench` once per package group
> (`.github/workflows/bench.yml:36,46`) and pipes the raw output into the run summary
> (`:48`). It keeps no baseline, records no machine facts, and nothing in it can fail.
> Only three performance facts can fail a test in this repository: the allocation ratios
> in `pkg/orderbook/alloc_test.go:51,75,92`.

> **What this slice is.** This slice adds four things:
>
> - a committed, frozen command tape in a language-neutral text format;
> - a portable output digest with two layers: `core`, which covers trades and the book, and `full`, which adds verdicts and events;
> - one benchmark that replays the tape;
> - a CI job that compares base against head in one job on one runner and records the machine conditions with every result.
>
> The job runs report-only on push. In `enforce` mode it **can fail**, and this slice
> must show that: planted regressions S1 and S4 (§10) must exit non-zero.
>
> **What this slice is not.** It has no C++ or Rust and no stage attribution. It does
> not cover the durable, replicated, multi-symbol, conditional-order or gateway paths.
> It publishes no absolute figure from a shared runner. Enforcement on every push,
> commit trailers, the `pull_request` trigger and the 60-run calibration belong to
> **slice B** (§9.4). The `core` digest and the `Portable` tape are designed so that a
> matcher without self-trade prevention and without this engine's event conventions
> can still be compared (§2.4, §3.1). Whether that holds is tested only when such a
> matcher exists (§11.6).

> **The one decision this document exists to get right is §5.2: what may fail a build
> on a shared runner.** Every other section supports it.

> **Measured so far, and only so far:**
>
> - noise from 30 historical `bench.yml` runs, read through `gh`;
> - local A/A runs on one Apple M4;
> - planning probes of a Bench-like profile through `go test -overlay`, kept in a scratch directory. The M4 was busy (load 9-21), and nothing was written to the repository.
>
> Every threshold in §5 is a target until the A/A′ runs in §9.3 have run on
> `ubuntu-latest`.

Companion documents:

- [`BENCHMARKS.md`](BENCHMARKS.md), top blockquote (:6-15). It states the gap. Its methodology must not be contradicted: book size is a parameter of the result (:81-104), and `0 allocs/op` is integer division (:107-110).
- [`TESTING.md`](TESTING.md), "The rule" and case study 3. Every check is run against code that has been broken on purpose, and a timing threshold must sit above its own noise.
- [`SOAK.md`](SOAK.md) :220-222 gives the house's interleaved A/B method. `soak.yml:9-13` says a shared runner cannot produce a figure worth comparing; §5 responds to that.
- [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2 and §4: `internal/refmatch` is the second implementation, and `internal/tape` is the one generator.
- [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §5.4, Rules 21-22 (:724-732): how the expected digest may change (§3.5).
- [`COMPATIBILITY.md`](COMPATIBILITY.md) :109: format versions get their own rule, so the two new formats get rows there (§7).

---

## 1. Why this exists

### 1.1 The gap, precisely

- `BENCHMARKS.md:6-15` says: "There is no stored baseline, no `benchstat` comparison, and no condition that can fail a build."
- `PERFORMANCE-ROADMAP.md` M10 says there is no shared tape and no portable digest. It also says CI benchmarks record no machine conditions and no baseline. `EngineSnapshot.Digest` rules itself out as the portable digest: it hashes JSON and is "stable between processes running the same release and nothing stronger" (`pkg/matching/snapshot.go:313-315`). The roadmap cites `:297-302` for this at `PERFORMANCE-ROADMAP.md:926`, and that citation is stale.
- `bench.yml` triggers on push to `main` (paths `pkg/**` and the workflow itself) and on `workflow_dispatch` (`bench.yml:3-9`). It never runs on a pull request.
- It takes one sample per benchmark, with time-based `b.N`: no `-count` and no `-benchtime` (`:36,46`).
- Several benchmarks size their book by `b.N`: `OrderBook_Add`, `OrderBook_Cancel`, `Engine_RestingInsert`, `Engine_Cancel`, `Latency_CancelOnly` (preload `b.N+1000`, latency_scenarios_test.go:94) and `CancelHeavy`. On a faster machine, a time-based run measures a different book.
- `README.md:48` says "The benchmarks are regression checks for the core." That is still not true after this slice, which gates five timings and runs report-only, so the line is reworded (§13).

This slice is item 5 in "Immediate next slice" (`PERFORMANCE-ROADMAP.md:1451`).

### 1.2 What a regression gate can and cannot prove

- **It can show** that head is not grossly slower than base on five fixed workloads, in one job on one machine. "Grossly" is defined in §5.2.
- **It can show** that head allocates no more than base on the listed benchmarks at their fixed N. It shows this only for those benchmarks and only at that N.
- **It can show** that the engine reproduces the committed digest, and that the engine and `refmatch` agree on it.
- **It cannot catch a slowdown that comes with a behaviour change on the tape.** A different digest suspends the `TapeReplay` timing (§3.5). The microbenchmarks are still compared.
- **It cannot catch drift that adds up.** Ten commits at +8% each all pass against their own base. A weekly comparison against the last release tag reports this (`mode: anchor`, §5.5). In slice A that report fails nothing.
- **It does not block in practice.** One merge commit in 341 (`git log --merges`) means changes land by direct push. The gate turns `main` red after the fact. The only pre-push check is `make bench-check` (§5.4).
- **It cannot give a throughput or latency figure.** It publishes none (soak.yml:9-13).

---

## 2. The shared tape

### 2.1 Source: `tape.Gen`, frozen once to a committed file

There is no second generator. The tape is produced once by `tape.Gen(tape.Bench, seed, n)`
(`internal/tape/tape.go:266-308`). `Gen` is a pure function of profile, seed and length,
and its LCG is pinned (`tape_test.go:16-26`).

None of the existing profiles is a performance workload. The probe ran `Differential`
with seed `0x5EED1234` and n = 100 k. It rejected 65% of submits, because the halt,
resume and cancel-only weights (tape.go:191) leave the engine halted for long stretches.
95% of cancels found no live order, and 3 orders were resting at the end.

`tape.Bench` is a **new `Profile` value**:

| Field | Value | Why |
|---|---|---|
| Weights | Submit 60, Cancel 25, Reduce 7, Replace 8; all others 0 | No halted stretches. Cancel-heavy. |
| Users / TradeGroups | 64 / 0 | With `TradeGroups` at 0, no trade-group draw is taken (tape.go:458). |
| PriceLo / PriceSpan / QtyMax | 1000 / 41 / 9 | Depth builds up across 41 levels. |
| Exotic / ExoticDamp | on / 4 | Market orders, IOC, FOK and post-only at a quarter of the differential rate. With Exotic off, `fillOrder` returns before any of them is drawn, and the tape would be GTC limit orders only (tape.go:427-434). |
| `Portable` (new knob) | on | See below. |

`Portable` follows the ExoticDamp rule (tape.go:156-180): every draw is still taken, so
the stream does not move. Only the values change:

- the per-order STP draw and the Privileged draw (tape.go:452-463) are taken and then discarded;
- a user's side is fixed by its parity: even users buy and odd users sell. A Replace takes its owner's side after `aimAtOwner`.

The result is that no user is ever on both sides, so **no self-cross is possible by
construction**, and the tape needs no STP semantics at all (§2.4).

Not taken: an `AimLive` targeting knob. The generator never simulates matching
(`Gen` builds only `[]Cmd`), so it cannot know which orders have filled. A probe that
emulated live-targeting moved the share of hits on live orders from 14-16% to only
18-22%.

**Shape, probed on the near-final profile.** The probe used Exotic with damp 4 and no
`Portable`, n = 50 k:

- 3.6% of submits rejected;
- 14.3% of Cancel, Reduce and Replace commands reached a live order;
- one trade per 2.5 commands;
- 3,646 orders resting at the end, which is also the peak.

**The book grows across the tape.** That is a fixed property of the tape, not of `b.N`,
and the peak is recorded in §12. `TestBenchTapeShape` asserts floors below the probe
values. If the `Portable` tape misses any of them, §12 says so and gives the reason:

- at most 5% of submits rejected;
- zero `OrderBookFull`;
- `refmatch.STPDecisions()` all zero (refmatch.go:341);
- no user on both sides;
- at least 10% of Cancel, Reduce and Replace commands reach a live order;
- at least 1,000 orders resting;
- at least one trade per 4 commands.

### 2.2 File format (`obtape 1`)

The file is ASCII with LF line endings and no trailing whitespace. It contains no Go
names, no gob and no JSON. Each line is one record, with `key=value` fields in a fixed
order per kind. Integers are decimal, and enum values are written as names.

```
obtape 1
generator internal/tape lcg mul=6364136223846793005 inc=1442695040888963407 shr=11
provenance profile=bench seed=0x<hex> n=50000
config maxorders=1000000
kinds Submit Cancel Reduce Replace
features limit market gtc ioc fok postonly
body-sha256 <64 hex>
---
pos=0 kind=Submit user=17 side=S type=LIMIT price=1017 qty=4 tif=GTC postonly=0
pos=1 kind=Cancel target=0 user=17
pos=2 kind=Reduce target=0 user=17 newqty=2
pos=3 kind=Replace target=0 user=17 side=S type=MARKET qty=3 tif=IOC postonly=0
```

Rules:

- **`pos`** is dense from 0 and equals the line's index in the body.
- **`target`** names an earlier `pos`, never an engine ID. The tape is deletion-closed (tape.go:80-87; `TestDifferentialTapesAreDeletionClosed`). Every implementation keeps its own table from position to order ID.
- **`user`** is an integer, and every Cancel, Reduce and Replace carries one. Ownership is part of the semantics: "not yours" is answered exactly like "not found" (engine.go:2368-2372). About one targeted command in four is deliberately aimed at the wrong owner (tape.go:358-360), and every implementation must reject those as not found. The Go driver renders `user=17` as `u17`.
- **MARKET lines carry no `price`.** The writer drops the drawn price. `types.NewOrder` zeroes it anyway (`pkg/types/order.go:151-152`), and so does refmatch (commands.go).
- **`config maxorders`** is an explicit capacity. `0` would *not* mean unlimited: both implementations turn 0 into 100,000 (`pkg/orderbook/orderbook.go:171-172`, `internal/refmatch/refmatch.go:315-316`). The shape test asserts zero `OrderBookFull`, so the value never affects the output. Another implementation only has to accept the recorded peak.
- **`body-sha256`** covers the bytes after the `---\n` line, up to and including the final LF.
- **`kinds`** and **`features`** list exactly what the body uses.
- A reader **rejects** any unknown key, unknown enum name, header line out of order, or unknown version. It never skips one.

The file is `internal/benchgate/testdata/bench-v1.obt` with **n = 50,000**. In this
format the probe measured 72.2 bytes per line, so the file is about 3.6 MB. At
n = 100 k it would be about 7.2 MB, roughly twice the draft's estimate. If the final file
is over 4 MiB, n drops to 40,000 and §12 says so.

### 2.3 Why a committed file and a seed

`tape_test.go:9-15` and `tape.go:124-127` argue against golden-pinning generated tapes.
This file is not such a golden. **The file is the contract, and the seed is only
provenance.** A seed is not portable: reproducing the tape would mean re-implementing
about 250 lines of draw order, `intn`'s modulo, `aimAtOwner` and `accumulating` exactly.
Reading the file takes about 50 lines in any language.

`TestBenchTapeMatchesGenerator` regenerates the tape from the provenance line and
compares it byte for byte. If the generator has moved, it fails with: `generator moved:
tape.Gen(bench, seed, n) no longer reproduces bench-v1.obt; leave v1 frozen and cut
bench-v2.obt`. A new tape is a new file, and the old file stays for as long as any
`.digest` file refers to it.

### 2.4 Workloads carried

| M10 workload | This slice | How |
|---|---|---|
| Mixed order flow | **Covered** | `bench-v1.obt` |
| Cancel-heavy market making | **Partly covered** | 40% of commands are targeted, but only about 14% of those reach a live order. Also `Engine_CancelReplaceInto` and `OrderBook_CancelReplace`. |
| Many price levels | **Partly covered** | `OrderBook_LevelChurn` (2 k levels) |
| Aggressive sweeps | **Partly covered** | `Engine_MatchInto` (pair). `Latency_AggressiveWalk` is gated on allocations only. |
| Deep book, one long FIFO queue | **Deferred** | Needs new generator knobs and their own tapes. |
| Conditional orders, STP, halts, phases | **Deferred** | `T`/`U` records are reserved. STP is excluded by construction (§2.1). SetPhase has no refmatch support. |
| Multi-symbol, durable, replicated, gateway | **Deferred** | fsync- and network-bound (§5.2). |

The v1 alphabet is limit and market orders, GTC, IOC and FOK, post-only, Cancel, Reduce
and Replace. Another matcher can be expected to express all of these. It needs no STP,
because self-crosses cannot occur.

### 2.5 Configuration the tape assumes

The driver takes only `maxorders` from the tape. It sets nothing else except a counter
clock, as `diffClock` does (differential_test.go:56-62). The tape carries no timestamps.
The engine's default STP mode is irrelevant, because no self-cross can happen.

---

## 3. The portable output digest (`OBDG` v1)

### 3.1 What is hashed: two chains

| Chain | Contents | Who compares on it |
|---|---|---|
| **`core`** | Per command: whether it was refused. Every trade. The terminal book. | The future cross-language harness. Two correct matchers with different event conventions agree here. |
| **`full`** | Per command: status and reason. The whole event stream in publish order, with trades as events. The terminal book. | This repository's gate and tests. |

`full` is needed because of defect B: a reject and an accept-then-cancel leave the same
book and the same trades (`internal/semcheck/semcheck.go:33-39`). It also catches a
cancel of an unknown order, which emits no event at all (semantics.txt `fifo/0005 … E[]`).
In `full`, trades come from the event stream. In `core`, they come from the returned
trade list. These are two different sources, so `full` and `core` together check both.

**Naming.**

- Orders are named by tape position, never by engine ID. `OrderRef` is 9 bytes: `pos u64` followed by `leg u8`, where `leg` is 0 and 1 is reserved for a one-cancels-other second leg.
- Each driver learns its own ID → `pos` table from the first Accepted or Rejected event during that command.
- Any other event that names an unseen ID is a hard driver error.
- Trades are named by a dense ordinal starting at 1.

**Encoding.** Integers are big-endian, and i64 values are two's complement (the wire's
rule, `internal/wire/wire.go:11`). Each record starts with an ASCII tag and has a fixed
width per tag.

| Chain | Tag | Fields | Width |
|---|---|---|---|
| both | header | `"OBDG"`, format u8 = 1, SHA-256 of the **whole** tape file (32) | 37 |
| core | `'c'` | pos u64, refused u8 (1 if status = Rejected) | 10 |
| full | `'C'` | pos u64, status u8, reason u8 | 11 |
| full | `'A'` / `'D'` / `'P'` | ref (Accepted / Canceled / Replaced) | 10 |
| full | `'R'` | ref, reason u8 | 11 |
| both | `'X'` | ordinal u64, price i64, qty i64, maker ref, taker ref, aggressor u8 `'B'`/`'S'` | 44 |
| full | `'H'`/`'O'`/`'Q'`, `'T'`/`'U'` | reserved; never emitted on a v1 tape, and the encoder fails if it sees one | — |
| both | `'E'` terminal | resting count u64; per resting order an `'L'` record (ref, side u8, price i64, qty i64, filled i64: 35 bytes); state u8 `'O'`/`'C'`/`'H'`; last trade price i64 (0 if no trade); trade count u64 (the number of `'X'` records) | variable |

Within a command, its `C`/`c` record comes first, then its events in publish order. In
`core`, the trades follow in the order they were returned.

In the `L` record, `qty` is the current total after reduces and STP decrements, and
`filled` is the filled quantity. Remaining quantity is `qty − filled` and is not stored.

The terminal book lists bids best-first, then asks best-first, oldest first within each
level. This is stated here as a rule; it happens to equal `book.Orders()`
(`orderbook.go:508-518`).

**Verdict per command kind**, matching refmatch (commands.go):

| Kind | Status / reason | Events |
|---|---|---|
| Submit | The order's status, and its reason if refused | Accepted or Rejected, then trades, then Canceled for an IOC or market remainder (`emitResult`, engine.go:807-826) |
| Cancel | Cancelled / None, or Rejected / reason | Canceled on success, nothing on failure |
| Reduce | Reduced / None, or Rejected / reason | Replaced on success (engine.go:2407) |
| Replace, where the cancel fails | Rejected / the cancel's reason | none (engine.go:2441-2443; commands.go:241-245) |
| Replace, where the cancel succeeds | The replacement's status and reason | Canceled (target), then the replacement's Submit events (engine.go:2444; commands.go:246-251). The target is identified by code reading here, and that reading is verified by `TestBenchTapeRefmatchAgrees`. |

**Enums** belong to this format and are frozen by the golden vector, not by Go `iota`:

- status: 0 NA, 1 New, 2 PartiallyFilled, 3 Filled, 4 Cancelled, 5 Rejected, 6 Reduced (refmatch.go:84-98);
- reason: 0 None through 11 NotionalOverflow (refmatch.go:102-115).

Engine errors map to reasons through the same 11-entry table the differential harness
uses (`tierOneRejections`, `pkg/matching/differential_test.go:212`). That table is
copied into `internal/benchgate`, because a test file cannot be imported. **An unmapped
error is a hard encoder failure**, for example `ErrCancelTooSoon`, which is unreachable
with `MinRestingTime` 0.

In v1, `D` has no reason byte. A Canceled event carrying a non-nil `Reason`
(`emitCancelReason`, engine.go:864-870) is an encoder failure. `orderentry.ReasonFor`
was rejected as the reason source because it loses information
(`pkg/orderentry/reason.go:47-102`).

### 3.2 What is excluded, and why

| Excluded | Reason |
|---|---|
| Engine order IDs and trade IDs | They are policy, not behaviour. IDs carry shard bits (`id.go:41`). A failed FOK burns trade IDs (engine.go:2067-2070, 2083). semcheck still pins the numbering. |
| `Event.Seq`, timestamps | Seq is implied by record order (engine.go:835-836). Timestamps depend on the clock (engine.go:695-698). |
| User strings, symbol | Fixed per order by the tape. The digest contains no strings. |
| Fields read through an event's `*Order` | The pointer shows the order's state at publication, not at the event (`fifo/0003 … ACCEPTED#4/FILLED/5`). |
| `SemanticsVersion`, `maxorders` | Another implementation has no such number. Capacity never binds (§2.2). |

### 3.3 Hash, framing and the vector

Each chain is hashed separately with `crypto/sha256`, in blocks of 4,096 commands.
Events belong to the block of the command that caused them.

```
H0 = SHA256(header)                    (identical for both chains)
Hk = SHA256(H(k-1) || records of commands [4096·(k-1), min(4096·k, n)))   k = 1..⌈n/4096⌉
F  = SHA256(H_last || terminal)        (H_last = H0 when n = 0)
```

The vector is a hand-written 6-command tape. It covers:

- a resting order;
- a crossing order that trades;
- a reduce;
- a cancel of an unknown position;
- a Replace;
- an IOC remainder.

`testdata/vector-v1.obt` holds the tape. `vector-v1-core.hex` and `vector-v1-full.hex`
hold its record bytes as three lines (`header`, `records`, `terminal`), and
`vector-v1-f.txt` holds both `F` values:

```
core 839300013cc1707d42bad84cf1438ff5b9e86336429e33dbc57abde3ebc4b8bf
full a08b096375f8c15138036b194e4e5a6f13b433afa4f000211947c8d8cd64b66d
```

`TestDigestVector` builds the expected bytes by hand from the layout table above, with
its own helpers, and requires both drivers' encoder output to equal them. A step in
`bench-gate.yml` re-derives each `F` from the hex files with `perl` and `sha256sum`,
chaining H0, H1 and F exactly as above, so the byte layout is checked by something
that is not the Go encoder.

**`internal/benchgate/testdata/bench-v1.digest`:**

```
obdigest 1
semantics 4
tape sha256:<hex> file=bench-v1.obt
terminal resting=<n> trades=<n>
block 1 core=<hex> full=<hex>
…
block 13 core=<hex> full=<hex>
core <hex>
full <hex>
```

`semantics 4` is the current `SemanticsVersion` (`pkg/matching/semantics.go:77`).

### 3.4 Three fingerprints, three questions

| Fingerprint | Question | Portable? |
|---|---|---|
| `EngineSnapshot.Digest` | Is this restored state the same as that one, within one release? | No, by its own comment (snapshot.go:313-315). |
| `internal/semcheck` golden | Did the engine's semantics change, including ID numbering? | No. It embeds engine IDs and `shortDigest` (semcheck.go:817,844-845). |
| `OBDG` `core` / `full` | On this tape, does this implementation produce the same trades and book (`core`), and the same verdicts and events (`full`)? | **Designed to be.** Checked today by engine == refmatch, and by the vector outside Go. |

The OBDG encoder must not call `Digest()` or `shortDigest`.

**Position naming is load-bearing by construction.** `refmatch` runs with
`ShardIndex: 1`, so its IDs differ from the engine's (`composeID`, refmatch.go:327).
An encoder that wrote IDs instead of positions would make the two digests disagree.

### 3.5 When the expected digest may change

| Case | Result |
|---|---|
| Same `SemanticsVersion`, different digest | **Hard fail.** Extend the semcheck corpus, sabotage-measure it, then bump. |
| Recorded version below the current one | Fail until regenerated with `BENCHGATE_UPDATE=1`. Regeneration is allowed only when the current version is strictly greater (Rule 21). Every bump procedure therefore gains this step (§13, SEMANTICS-VERSION.md). |
| Recorded version above the current one | Fail (a downgrade). |
| Version bumped, digest identical | Allowed. Only the `semantics` line changes. Rule 22 does not apply, because this tape is narrower than the corpus. |
| Encoding change | New OBDG format byte and a new `.digest` file. Independent of `SemanticsVersion`. |

In the comparison (§5.2), the base and head binaries each print the `full` digest they
produce on **base's** tape. If the two differ, only `TapeReplay/*` is `not compared:
digest differs`. The microbenchmarks and allocation rules still apply.

---

## 4. The replayer, and what it measures

`internal/benchgate` holds:

- the `obtape 1` reader and writer;
- the OBDG encoder;
- an engine driver and a refmatch driver;
- the reason table;
- `BenchmarkTapeReplay`.

It imports `pkg/matching`, `pkg/types`, `internal/tape` and `internal/refmatch`.
`internal/tape` and `internal/refmatch` stay stdlib-only (`TestReferenceMatcherImportsNothing`,
internal/refmatch/imports_test.go:31).

**One op is one full replay.** The benchmark reports ns/op, `ns/cmd` through
`b.ReportMetric`, and allocs/op. Because the op is the whole replay, allocs/op is a total
over 50,000 commands, not a per-command figure rounded by integer division.

> **Correction.** This said the total was exact, so that one extra allocation anywhere
> in 50 k commands would show. It is not exact on darwin: identical code read 9,060 to
> 9,063 per replay within one arm of a local A/A run. On linux/amd64 it read 9,067 in
> every one of 13 CI runs. The rule in §5.2 therefore carries a slack of 8 on this
> benchmark, and an extra allocation per replay below that is not seen.

| Inside the timed region | Under `b.StopTimer` or done once |
|---|---|
| `Match(order, buf[:0])` for Submit; `Cancel`, `Reduce` and `Replace`, with IDs looked up from a preallocated `[]int64` indexed by pos | Parsing (once). A fresh `NewEngine` per op, with the counter clock and a nil sink. Fresh `*types.Order` values per op, from templates. `runtime.GC()` before each op, so the garbage from the previous replay is not collected during the next one. |

Never in the loop: `tape.Gen`, `fmt`, hashing, maps, or per-command `time.Now`.

There is one variant, `sink=nil`, with emission off (event.go:166-167).

**Guard.** After each op, under `StopTimer`, the benchmark compares `OrderCount()`
(engine.go:2645) and the driver's own count of returned trades with the `terminal` line
in the `.digest` file. A mismatch is `b.Fatal`. The engine has no trade-count accessor,
and its trade sequence is burned by a failed FOK, so it cannot be used.

**Correctness never runs inside the benchmark.** `TestBenchTapeDigest` and
`TestBenchTapeRefmatchAgrees` replay the tape once each. The second compares both
chains block by block and names the first block that diverges.

The probe measured refmatch on 50 k commands at 0.22-0.29 s, and 2.5-3.4 s under
`-race`. The engine took 0.3 s under `-race`. At 100 k, refmatch took 0.85-1.17 s
without race, which is superlinear. So n = 50 k needs no `-short` skip in `ci.yml`'s
`go test -race ./...` (ci.yml:40).

**It does not measure:** quantiles, durable, replication or gateway paths, GC pauses
beyond what ns/op absorbs, or any book shape the tape does not produce.

---

## 5. The comparison, and what may fail a build

### 5.1 Deterministic facts: hard fail, in ordinary `go test`

These run in `ci.yml` on both matrix Go versions:

- `TestBenchTapeFileIsFrozen`
- `TestBenchTapeMatchesGenerator`
- `TestBenchTapeShape`
- `TestBenchTapeDigest`
- `TestBenchTapeRefmatchAgrees`
- `TestDigestVector`
- `TestDigestFileRules` (§3.5)

The three ratios in `alloc_test.go` stay exactly as they are.

### 5.2 Timing and allocations: base against head, one job, one runner

There is no stored baseline and no comparison between runs. In 30 `bench.yml` runs,
GitHub assigned 5 CPU models. Across run pairs with no change to the benchmarked code:

- the |ratio| on different CPUs had a median of 1.177 and a maximum of 3.520;
- on the same CPU it had a median of 1.020 and a p90 of 1.103.

`OrderBook_Cancel` read 337.5 ns and then 158.2 ns on one CPU model with no code change.

**Who judges.** `cmd/benchgate` is **built from the base tree**. The gated list,
thresholds and benchtimes are its constants. A head commit therefore cannot loosen the
gate that judges it, and a change to the gate takes effect only after it lands. If base
has no `cmd/benchgate`, the verdict is `not compared: no base gate`.

**Procedure:**

1. Check out with `fetch-depth: 0`. Add a worktree at base. On push, base is `github.event.before`. A zero `before`, or a `before` that is not an ancestor, gives `not compared: no base`.
2. Build `base.test` and `head.test` with `go test -c` for `./internal/benchgate`, `./pkg/matching` and `./pkg/orderbook`, using one toolchain. `debug/buildinfo` must report the same Go version for both binaries; otherwise the verdict is `not compared`.
3. Both binaries replay **base's** `bench-v1.obt` (through `BENCHGATE_TAPE`), which base can always parse, and print their digests (§3.5).
4. **Benchmark identity.** Hash each gated benchmark's source file, plus `internal/benchgate/*.go` for `TapeReplay`, in both trees. Any difference makes that benchmark `not compared: benchmark changed`. A gated benchmark missing in head is `missing`. One that exists only in head is `new`.
5. **Fixed work.** Every invocation is `-run '^$' -bench '^BenchmarkX$' -benchtime=Nx -count=1 -benchmem`. The comparator rejects output with more than one result line.

   | Benchmark | N |
   |---|---|
   | `TapeReplay` | `5x` |
   | `pkg/orderbook` | `500000x` |
   | `pkg/matching` | `300000x` |
   | `Latency_MassCancelBurst` | `200x` (latency_scenarios_test.go:180-183) |

6. **Interleaving.** Each round runs four invocations, A B B A on odd rounds and B A A B on even rounds. Round 1's order comes from the low bit of the head SHA, which is recorded. Each round gives **two pairs** of adjacent A/B invocations, and each pair gives ratio = head/base. R = 10 rounds gives 20 pairs.
7. **Probes per invocation.** Immediately before each invocation, `cmd/benchgate` runs two probes:
   - an ALU probe, which is obsoak's `speedProbe`, `cmd/obsoak/main.go:788-796`, copied;
   - a memory probe, a dependent random walk over 64 MiB.

   A pair is **tainted** when either probe differs by more than 10% between its two invocations. The 10% is a target.

**Timing verdict, per gated benchmark:**

| Verdict | Condition (precedence top-down) |
|---|---|
| `inconclusive` | Fewer than 12 untainted pairs remain, more than 2 pairs are missing or crashed, or the comparator's 20-minute budget runs out. The run is retried once automatically. Inconclusive is never shown as pass. |
| `fail` | Over the untainted pairs, **median > 1.10** and the **k-th smallest ratio > 1.05**. k is the largest value with P(Binom(n, ½) ≤ k−1) ≤ 0.025, which is k = 6 for n = 20. In other words, a distribution-free lower bound on the median has to clear 1.05. |
| `faster` / `pass` | Otherwise. Faster never fails. |

The draft's "≥ 9 of 10 rounds slower, then a confirming rerun" rule is dropped. With
per-round noise, that rule catches a true 15% slowdown with a probability of only about
0.06. 1.10 and 1.05 are targets. Slice B's calibration (§9.4) can move them, in this
document, with the data. Pairs inside one round are not independent, so the 2.5% is
nominal (§11.2).

> **Correction.** The `inconclusive` rule was "more than 2 of 20 pairs tainted". On a
> loaded M4 that made the gate blind: a planted 7× slowdown came back inconclusive
> because 7 of 20 pairs were tainted, and a pair whose probes moved 10-30% cannot
> explain a 7× ratio. Tainted pairs are now left out, at least 12 clean pairs are
> required, and k is computed for the number actually used (3 for 12, 6 for 20), so the
> order statistic's bound holds on what the verdict rests on. On CI, 0 to 12 of 20
> pairs were tainted per benchmark (§12).

**Timing is gated on** `TapeReplay/sink=nil`, `Engine_MatchInto`,
`Engine_CancelReplaceInto`, `OrderBook_CancelReplace` and `OrderBook_LevelChurn`.

**It is never gated on:**

- benchmarks whose book depends on `b.N`;
- p99 or higher, or max;
- `Shards_Scaling`;
- any fsync-bound benchmark, or anything in `pkg/wal`.

**Allocation verdict.** Each arm's **median** count is compared: over the 20
invocations of a timing-gated benchmark, and over 3 interleaved invocations per arm for
the rest. Head must be ≤ base, with no slack, except `TapeReplay`, which may exceed base
by 8. Counts are compared within the job, never against a stored number, because they
differ by platform: `RunnerBare` reports 5 on darwin and 3 on linux, and `TapeReplay`
reads about 9,061 on darwin and 9,067 on linux.

> **Correction.** This was "one invocation per arm, exactly". The exact rule failed
> identical code on the tape replay (9,060 against 9,063 within one arm), and a single
> invocation lets one noisy run tip an integer-divided allocs/op across a boundary. The
> slack is twice the spread measured on darwin. One extra allocation per `Match` is
> +30,840 on this tape.

The allocation rule covers:

- `TapeReplay`;
- every `OrderBook_*` and `Engine_*`;
- every `Latency_*`, including `STPSweep/*` and `CancelHeavy`.

It excludes `Shards_Scaling` and all of `pkg/wal`.

**Known blind spot.** A microbenchmark that prints `0 allocs/op` hides one allocation
per 1,000 ops, because of integer division. `TapeReplay`'s total and the
`alloc_test.go` ratios cover the Match, Cancel, Reduce and Replace paths at better
resolution, `TapeReplay` down to its slack of 8 allocations per replay.

**Modes.** `report` writes the verdicts and exits 0. `enforce` exits non-zero on any
`fail`, `missing` or allocation increase. In slice A, enforce is used only through
`workflow_dispatch`.

### 5.3 Accepting a slowdown on purpose (slice B)

Specified here, built in slice B with enforcement:

- **The trailer.** `Bench-Accept: <name|glob> ratio<=<x> -- <reason>` accepts a slowdown. The stated bound may not exceed 1.5, and may not exceed the measured ratio × 1.10. Above 1.5, a change to this document is needed.
- **Allocations.** `Bench-Accept-Allocs: <name|glob> +<k>/op -- <reason>` must equal the measured delta. An escape-analysis change is a valid reason when a `-gcflags=-m` diff backs it.
- **Gate changes.** A change to `cmd/benchgate` or the gate workflow needs `Bench-Gate-Change:`.
- **Malformed trailers** fail the job.
- **Where trailers are read.** Every commit in the range.
- **Counting.** The summary prints the trailer count for the last 90 days.

A PR label or a workflow input is never an acceptance.

### 5.4 Why there is no benchstat

The judge is `cmd/benchgate`, which is stdlib-only. It parses the standard benchmark
line format and computes the paired statistic. Benchstat is not used:

- its Mann-Whitney test assumes independent samples, which batched noise violates;
- the gate's statistic is paired;
- `golang.org/x/perf` is third-party code, which may never run locally and would add a dependency only for display.

`make bench-check` runs the same comparator locally against `git merge-base HEAD main`,
in the style of `cover-check` (Makefile:49-50). Because changes land by direct push,
CONTRIBUTING.md asks for it before pushing a change to `pkg/matching` or `pkg/orderbook`.

### 5.5 Workflow

The new file `.github/workflows/bench-gate.yml` holds the gate. `bench.yml` is not
touched: its triggers would otherwise run the publishing job on every trigger the gate
adds.

- `push` to `main`, with paths `pkg/**`, `internal/tape/**`, `internal/refmatch/**`, `internal/benchgate/**`, `cmd/benchgate/**`, `go.mod` and the workflow file. Report-only.
- `workflow_dispatch` with three inputs:
  - `mode: compare | aa | anchor`. `aa` builds head and head′, where head′ is head with an unused function of 64-512 bytes added to `pkg/matching` in the runner's checkout, so the code layout moves. `anchor` uses the latest `v*` tag as base.
  - `base`.
  - `enforce: bool`.
- `schedule`, weekly, `mode: anchor`, report-only.

Go is pinned to `"1.27"`, as `bench.yml:22` is. Permissions are `contents: read`. The
job has `timeout-minutes: 30`; the comparator's own budget expires first and writes
`inconclusive`. The `pull_request` trigger waits for slice B, which needs fork handling
and a path decision made inside a step so that a required check never hangs.

---

## 6. Machine conditions recorded with every result

`cmd/benchgate` writes `bench-result.json`. It is uploaded with
`actions/upload-artifact@v4` and `retention-days: 90`, and its verdict table is echoed
to `$GITHUB_STEP_SUMMARY`.

| Field | Source |
|---|---|
| `go_version` (each binary), `goos`, `goarch`, `gomaxprocs`, `num_cpu` | `debug/buildinfo`, `runtime` |
| `gogc`, `gomemlimit` | environment, recorded even when empty |
| `cpu_model`, `kernel` | the `cpu:` line or `/proc/cpuinfo`; `uname -r` |
| `runner_image`, `runner_image_version` | `ImageOS` / `ImageVersion` if set (not verified), otherwise `unknown` |
| `loadavg_before`, `loadavg_after` | `/proc/loadavg` |
| per invocation: `probe_alu_ns`, `probe_mem_ns`; per pair: `tainted` | §5.2 step 7 |
| `mode`, `enforce`, `base_sha`, `head_sha`, `round1_order`, `tape_file`, `tape_sha256`, `digest_base`, `digest_head` | job |
| `benchtime`, `rounds`, `retried`, source-file hashes | gate |
| per benchmark: `ratios[]`, `median`, `k`, `kth_ratio`, `allocs_base`, `allocs_head`, `verdict` | comparator |

The calibration and sabotage numbers that matter are copied into §12. The artifact
expires after 90 days; §12 does not.

---

## 7. Backward compatibility

- **No `pkg/` surface change.** New code goes in `internal/benchgate`, `cmd/benchgate` and `tape.Bench` / `Portable`. `internal/apicheck/testdata/surface.txt` does not move.
- **Format versions.** `obtape 1` and `OBDG 1` each get a row in COMPATIBILITY.md's version table (:109). The promise is only that a reader rejects any version it does not know, and that a frozen tape file is never edited.
- **Unchanged:** `bench.yml` and `make bench`.

---

## 8. What this deliberately does not do

- No C++ or Rust, and no stage attribution.
- No absolute timing published from CI.
- No comparison across runs, across runners, or against `BENCHMARKS.md`.
- No gating of tails, fsync paths, sharding, `pkg/wal`, or any workload whose book depends on `b.N`.
- It does not replace `alloc_test.go`, semcheck or the differential harness.
- No STP, conditional-order, deep-book or single-FIFO tape in v1.
- In slice A: no enforcement on push, no trailers, no `pull_request` trigger, no `sink=count` variant, and no move of obsoak's probe.

---

## 9. Deliverables and acceptance criteria

| # | Deliverable (slice A) | Done when |
|---|---|---|
| 1 | `tape.Bench` and the `Portable` knob | `TestDifferentialTapeIsPinned` and the semcheck golden are unchanged. `Gen` hashes for Differential, Recovery and ProRata at seeds {1, 0x5EED1234, 7} and n ∈ {140, 200, 5000} are identical before and after, and are listed in the commit body. `TestPortableKeepsDrawCount` passes. |
| 2 | `obtape 1` reader and writer | Round-trip tests pass. Unknown keys, enums, versions and header order are rejected. |
| 3 | `bench-v1.obt` | The three tape tests pass. Size ≤ 4 MiB. |
| 4 | OBDG v1 and both drivers | `TestDigestVector` passes on both drivers and in the shell check. `TestBenchTapeRefmatchAgrees` passes, with refmatch at `ShardIndex: 1`. |
| 5 | `bench-v1.digest` | `TestBenchTapeDigest` and `TestDigestFileRules` pass. |
| 6 | `BenchmarkTapeReplay/sink=nil` | The guard is live. Allocs per replay are identical across 3 local runs. |
| 7 | `cmd/benchgate` (orchestrator and judge) | Unit tests cover pairing, the order statistic, precedence, the allocation rule, output rejection, and the benchmark-identity hashes, all on fixed input. |
| 8 | `bench-gate.yml` and `make bench-check` | The job runs on push, dispatch and schedule, and writes §6. |
| 9 | "Can fail" shown | In dispatch with `enforce: true`, S1 and S4 exit non-zero and P1 (10 A/A′ runs) exits zero. |
| 10 | Docs (§13) | Checklist done, not claimed by any test. |

### 9.1 Regressions it must catch

These are the §10 rows: S1, S2, S4, S5-S8b, S9-S12 and S13.

### 9.2 Changes it must not flag

An A/A′ run, a comment-only change in `engine.go`, a change confined to `pkg/marketdata`
(P8), and a 2× speedup, which must report `faster`.

### 9.3 Numbers recorded in slice A, not gating

- the S1, S2, S3 and S13 ratios as measured, per benchmark;
- the A/A′ medians and tainted counts from P1;
- the job's wall time (not measured; the estimate is 4-6 minutes plus two builds);
- whether the allocation totals were identical on linux/amd64 across the P1 runs;
- the tape's peak resting count.

### 9.4 Slice B (named so slice A cannot quietly absorb or drop it)

- **Calibration.** 60 A/A′ runs. Enforcement may turn on only if all of these hold:
  - no run fails. Zero failures in 60 bounds the per-run false-positive rate below 5% at 95% confidence;
  - at most 10% of runs are inconclusive;
  - the threshold is at least 1 + 3 × the A/A′ p99 of |median − 1|.
- **Power.** S2 sized at 1.20× must fail in at least 19 of 20 runs per gated benchmark. The measured detection floor is printed in every summary.
- Enforcement on push, the §5.3 trailers, and a second inconclusive becoming a failure.
- The `pull_request` trigger.
- Allocation classes for `pkg/wal`.
- `sink=count`.
- Moving obsoak onto a shared probe.

---

## 10. Sabotage runs required before this counts as done

Sabotage follows TESTING.md "Doing it": `cp` a backup, plant the change, run, restore.
**CI** rows run on a throwaway branch through dispatch and are never merged. File:line
for each planted change, and the *measured* ratio per benchmark, are recorded in §12.
Every planted loop writes its result to a package-level variable so the compiler cannot
delete it.

| # | Sabotage | Must |
|---|---|---|
| S1 (CI) | Busy loop at the top of `(*Engine).Match`, sized so that `Engine_MatchInto` is about 2× | **Fail** timing on `TapeReplay` and `Engine_MatchInto`, enforce exit ≠ 0 |
| S2 (CI) | The same loop at about 1.15× on `Engine_MatchInto` | **Fail**, or record it as below the floor; slice B owns power |
| S3 (CI) | The same loop at about 1.05× | Record only |
| S13 (CI) | A memory-bound plant: one lookup per `Match` into a 1 MiB table at a hashed index, about 15% | Record, then fail or report it, as S2 |
| S4 (CI and local) | `sink = append(sink, make([]byte, 1))` inside `Match` | **Fail** allocations on the first run, enforce exit ≠ 0 |
| S5 | The maker price + 1 when a trade is built | **Fail** `TestBenchTapeDigest` and `…RefmatchAgrees`; compare reports `TapeReplay: not compared: digest differs` while the microbenchmarks are still compared |
| S6 | The encoder writes engine IDs instead of positions | **Fail** `TestBenchTapeRefmatchAgrees` (refmatch's shard-1 IDs differ) |
| S7 | The terminal book lists a level newest-first | **Fail** both digests |
| S8a | The encoder drops `A` and `D` records | **Fail** `TestBenchTapeDigest` (`full`); record that `core` is unchanged |
| S8b | Engine driver: an unknown-target Replace is accepted and then cancelled instead of rejected | **Fail** `full`; record whether `core` catches it |
| S9 | Flip one byte in the body of `bench-v1.obt` | **Fail** `TestBenchTapeFileIsFrozen` |
| S10 | Change one weight in `tape.Bench` | **Fail** `…MatchesGenerator` with the "cut bench-v2" message |
| S11 | The driver skips every 100th command | **Fail** the guard (`b.Fatal`) |
| S12 | Driver capacity set to half the measured peak | **Fail** `TestBenchTapeShape` (OrderBookFull > 0) |
| S14 | `Portable` skips the STP draw instead of discarding it | **Fail** `TestPortableKeepsDrawCount` |
| S15 | The reader skips an unknown key / an unknown enum / an out-of-order header | **Fail** each rejection test |
| S16 | An event names an unseen ID; an error has no mapping | **Fail**: hard encoder error |
| S17 | `BENCHGATE_UPDATE=1` at the same `SemanticsVersion` | **Fail**: refuses to write |
| S18 | Comparator with base and head swapped, on S1 output | Reports `faster`, which proves the direction is wired |
| S19 | Comparator pairs runs across rounds instead of within one (replaces "ratio of medians", see §12) | **Fail** `TestJudgePairsWithinARound` |
| S20 | Benchmark output with two result lines (an unanchored regex) | Rejected |
| P1 (CI) | A/A′, 10 dispatch runs, enforce | **Pass** in every run |
| P2 (CI) | Comment-only change in `engine.go` | **Pass**; `Engine_*` reported `not compared: benchmark changed` only if a bench file moved |
| P3 (CI) | S1 inverted, about 2× faster | **Pass**, reported `faster` |
| P4 (CI) | `stress`-style ALU and memory hog during head's invocations only, identical code | **Inconclusive** (tainted pairs), not fail and not pass |
| P8 (CI) | Change confined to `pkg/marketdata` | **Pass** |

If S1 passes, the gate does not exist. If P1 fails even once, the gate stays report-only
and §12 records why.

---

## 11. How this can fail, stated in advance

1. **Runner noise is above the M4's ±2%.** The local evidence is 4 benchmarks, one trial each. If A/A′ medians reach 5%, 1.10 has under 2× headroom and slice B blocks enforcement.
2. **Pairs are not independent.** Bursty load correlates neighbouring invocations, so the 2.5% from the order statistic is optimistic. The magnitude condition and the tainted-pair rule are there for this. Each P1 failure is logged, never retried away.
3. **A frequency step in the middle of a job.** ABBA/BAAB cancels a linear drift but not a step. Probes run before every invocation and catch a large step; a small one leaks into the ratios.
4. **Code layout.** A real change moves function addresses. A/A′ measures this with padding of 64-512 bytes. If the layout spread exceeds about 3%, 1.10 has no headroom.
5. **The tape is too easy.** About 86% of targeted commands miss a live order, and the book only grows. A regression in cancel-of-live or in deep-level walking may be diluted. Deep-book and FIFO tapes are deferred.
6. **The digest is less portable than designed.** refmatch follows `engine.go` in known ways (REFERENCE-MATCHER.md §2.2), and one Go encoder serves both drivers. Only the shell-checked vector is independent of the Go encoder. The C++ and Rust work is the real test.
7. **Position assignment assumes Accepted or Rejected comes first** for every new order. The encoder refuses any unseen ID rather than guessing.
8. **Exact allocation equality fails on correct code** when inlining or escape analysis changes. In slice A that is a reported `fail`. Slice B resolves it through `Bench-Accept-Allocs` with a `-gcflags=-m` diff.
9. **The first push after this lands has no base gate.** The verdict is `not compared: no base gate`. That is expected and is not a pass.
10. **The reason table is a copy** of `tierOneRejections`. If the two drift, refmatch agreement still catches a wrong mapping on the tape, but not on errors the tape never reaches.
11. **The trailer becomes a habit (slice B).** If `Bench-Accept` appears in most changes to `pkg/matching`, the threshold is wrong. The summary prints the 90-day count.
12. **The 3.6 MB tape cannot be reviewed line by line.** It is reviewed through its header, the generator test and the shape test.

---

## 12. What building it found — written after the code

Written after the code, on 2026-10-06. Local figures are from an Apple M4 that was busy
throughout (load 7 to 22). CI figures are from `ubuntu-latest`: 13 dispatched runs
landed on three CPU models, the AMD EPYC 7763, 9V45 and 9V74.

### 12.1 The tape and the digest, measured

| Quantity | Value |
|---|---|
| `bench-v1.obt` | 50,000 commands, 3,611,312 bytes, sha256 `d6e01e40…0d2ef7` |
| Submits rejected | 1,117 of 30,231 (3.7%; floor 5%) |
| Targeted commands reaching a live order | 2,905 of 19,769 (14.7%; floor 10%) |
| Trades | 20,672, one per 2.4 commands |
| Resting at the end / peak | 3,380 / 3,388 |
| STP decisions, full-book refusals | 0, 0 |
| `go test -race ./internal/benchgate` | about 23 s on the M4 |
| `TapeReplay` allocations per replay | 9,060 to 9,063 on darwin; 9,067 in all 13 CI runs |
| Gate job wall time on CI | 2 min 38 s to 3 min 40 s |

Every existing tape profile generates exactly what it did before `Bench` and `Portable`
were added: 27 hashes of `Gen` output (Differential, Recovery and ProRata, seeds 1,
0x5EED1234 and 7, n 140, 200 and 5,000) were identical before and after, and are listed
in the commit that added them.

Both drivers produced the hand-written vector bytes on the first run, including the
engine answering "not found" (code 7) for a cancel of an order that had already filled.

### 12.2 Sabotage, as run

| # | Where | Planted | Result |
|---|---|---|---|
| S1 | local | 600-iteration loop in `Match` (`MatchInto` 423 → 2,890 ns) | `MatchInto` fail 6.71 (k-th 5.99), `TapeReplay` fail 4.01 (3.86); `make` exits 2 |
| S1 | CI | 200-iteration loop | `MatchInto` fail 2.09 (2.03), `CancelReplaceInto` fail 1.86 (1.85), `TapeReplay` fail 1.73 (1.69); job fails |
| S2 | CI | 30-iteration loop | `MatchInto` **fail 1.136 (1.074)**; `CancelReplaceInto` pass 1.086; `TapeReplay` pass 1.090 |
| S4 | local and CI | one allocation per `Match` | allocations fail on `Engine_Match` 4 → 6, `MatchInto` 0 → 2, `CancelReplaceInto` 0 → 1 (CI), `TapeReplay` 9,061 → 39,901 local and 9,067 → 39,906 on CI; job fails |
| S5 | local | trades print at price + 1 | `TestBenchTapeDigest` and refmatch agreement fail at block 1, core and full |
| S6 | local | encoder names makers by engine id | refmatch agreement fails |
| S7 | local | terminal book in reverse | digest fails: "the blocks agree, so the terminal record differs" |
| S8a | local | encoder drops A and D records | digest fails in `full` only; `core` unchanged |
| S8b | local | failed-cancel Replace reported as cancelled | fails in `core` and `full` |
| S9, S10, S12 | local | body byte flipped; one `Bench` weight changed; capacity 1,694 | frozen-file, generator ("cut bench-v2") and shape (3,931 full-book refusals) tests fail |
| S11 | local | timed loop skips every 100th command | guard: 3,343 resting / 20,502 trades against 3,380 / 20,672 |
| S14-S17 | local | `Portable` skips a draw; reader tolerances; unseen id; update at the same version | each fails its test (S17 only after the fix in 12.4) |
| S18, S19, S20 | local | ratio upside down; cross-round pairing; two result lines | each fails its judge test |
| P2 | local | comment-only change in `Match` | pass, medians 1.008 and 1.015 |

S2 is the first evidence of power: a slowdown of about 14% on `MatchInto` failed with its
lower bound at 1.074. The same plant diluted to 9% on the tape replay and passed. That is
the replay doing what §11.5 predicted, and it is why the microbenchmarks stay gated
beside it.

### 12.3 A/A′ on CI (P1)

Ten dispatched runs with `mode: aa`, the code layout moved by 100 to 488 bytes, and
`enforce: true`. **All ten passed.**

| Benchmark | Medians, 10 runs | Highest k-th ratio | Tainted pairs per run |
|---|---|---|---|
| `TapeReplay/sink=nil` | 0.981 – 1.032 | 0.989 | 0 – 6 |
| `Engine_MatchInto` | 0.962 – 1.018 | 0.991 | 0 – 8 |
| `Engine_CancelReplaceInto` | 0.990 – 1.010 | 1.002 | 0 – 6 |
| `OrderBook_CancelReplace` | 0.937 – 1.087 | 0.990 | 0 – 7 |
| `OrderBook_LevelChurn` | 0.992 – 1.044 | 1.027 | 0 – 4 |

The highest median, 1.087, came with a k-th ratio of 0.989: the lower bound is what kept
it a pass. Ten runs bound nothing at the precision slice B needs; the 60-run
calibration in §9.4 still decides enforcement. Every allocation count matched between
the arms in all ten runs.

### 12.4 Where this document was wrong about its own design

- **Allocation counts are not exact** (§4, §5.2, corrected in place). The rule is now
  medians with a measured slack on the tape replay.
- **"More than 2 of 20 tainted" made the gate blind on a busy machine** (§5.2,
  corrected in place). It is now "fewer than 12 clean".
- **S19 could not be built as written.** A ratio of arm medians and the median of paired
  ratios agree on every fixture, because medians commute with the per-pair scaling.
  What pairing buys is cancelling drift between rounds, so S19 became "pair across
  rounds of the same ABBA parity". Under drift of up to 4× with a steady 15% slowdown,
  that judge reports pass (median 1.16, k-th 0.77). An earlier version paired adjacent
  rounds, mismatched the arms, and failed tests for an unrelated reason. It proved
  nothing and was replaced.
- **`Latency_MassCancelBurst` at the package's 300,000x hung** until the 10-minute test
  timeout. The 200x override §5.2 already named is required, not a nicety.
- **Three tests were green against broken code until changed.** The digest-rules test
  had no row with an identical digest, so a sabotaged refusal stayed green. The tape
  reader ended with a whole-file comparison that caught every other rule's sabotage by
  itself. The first S19 sabotage did not compile.
- **Plumbing faults found by using it:** `make` expanded `$|` inside `BENCH_ONLY`, so a
  three-benchmark run silently ran one. The summary cut `-dirty` off a head revision.
  A shared concurrency group would have cancelled nine of ten A/A dispatches.
- **As predicted:** the first push reported "not compared: no base gate" (§11.9), and the
  vector check ran on `ubuntu-latest` with `perl` and `sha256sum`, which §13 had listed
  as unverified.

### 12.5 Not run in slice A

S3, S13, P3, P4 and P8, and the power study (S2 at 1.20× in 19 of 20 runs). They belong
with slice B's calibration and are not claimed here.

---

## 13. Commit plan, and documents to update

Commits are authored by Karthikeyan NG only, with **no co-author line**. Subjects use
conventional prefixes and state a finding. Bodies are prose: the problem, what changed,
and what the sabotage showed. The ordered list is in the implementation plan. Defects
found along the way get their own `fix(...)` commit and a changelog commit.

**Documents to update when it lands:**

- `docs/BENCHMARKS.md`: top blockquote (:6-15), "Reproduce" (add `make bench-check`), the CI paragraph, and the "neutral baseline" note.
- `docs/PERFORMANCE-ROADMAP.md`:
  - the M10 at-a-glance row (:95);
  - the M10 status blockquote;
  - the stale `snapshot.go:297-302` citation at :926, which becomes `:313-315`;
  - "Immediate next slice" item 5 (:1451): ✅ for the tape and digest, with stage attribution still open;
  - the "Research-ready" lines (:1405).
- `README.md:48`: reworded to what holds, which is "a report-only base-vs-head comparison on five benchmarks". Also a docs-index row for BENCH-GATE.md.
- `web/docs.html`: the docs table row. No figures or versions.
- `COMPATIBILITY.md`: `obtape 1` and `OBDG 1` rows in the version table (:109).
- `SEMANTICS-VERSION.md`: the bump procedure gains "regenerate `bench-v1.digest` with `BENCHGATE_UPDATE=1`" (§3.5). This is mandatory, not optional.
- `CHANGELOG.md` `[Unreleased]`: `### Added` for the tape, digest, benchmark, comparator, `make bench-check` and `bench-gate.yml`.
- `CONTRIBUTING.md`: run `make bench-check` before pushing a change to `pkg/matching` or `pkg/orderbook`.
- If it applies: `docs/SPEC.md:131` tree listing.

---

## 14. Slice B — the order it is built in, decided before the data

Written on 2026-10-06, after slice A and before any calibration run, so that what the
data is allowed to decide is fixed before it exists.

### 14.1 The order

1. **`benchgate calibrate`.** A subcommand that reads a directory of
   `bench-result.json` files and prints, per gated benchmark, the three §9.4 numbers:
   runs failed, runs inconclusive, and the p99 of |median − 1|. It works on data alone,
   so its tests are fixtures.
2. **60 A/A′ runs** on `ubuntu-latest` (`mode: aa`, `enforce: true`), dispatched
   together, with their artifacts downloaded and fed to `calibrate`.
3. **The decision**, by the rule in 14.2, recorded in §15 with the numbers.
4. **Power.** One branch plants a slowdown sized to about 1.20× on each gated benchmark
   that 14.2 enforces: a loop in `Engine.Match` reaches the matching benchmarks and the
   tape replay; the order-book benchmarks need their own plant, in `pkg/orderbook`.
   Twenty dispatched runs. Each enforced benchmark must fail in at least 19. The
   measured detection floor goes into the summary text.
5. **Enforcement.** Push runs get `-enforce` for the benchmarks 14.2 lets through. A
   second inconclusive run, after the automatic retry, becomes a failure under
   `-enforce`. The §5.3 trailers are read from every commit in the range, a malformed
   one fails the job, and the summary prints the 90-day count.
6. **`pull_request`**, with fork handling, once 5 has run on `main` for a week without a
   false failure.

### 14.2 The decision rule, per gated benchmark

Timing enforcement turns on for a benchmark only if all three hold over the 60 runs:

- it failed in **none** of them;
- it was inconclusive, after the retry, in **at most 6** of them;
- **1.10 ≥ 1 + 3 × p99(|median − 1|)**, where p99 is the nearest-rank 99th percentile of
  60 values, which is their maximum.

A benchmark that misses any of them keeps its timing **report-only**. It stays in the
summary, marked `report only (calibration)` with the criterion it missed, and nothing
about it is dropped silently. Allocation enforcement does not depend on this rule: the
allocation counts disagreed between arms in none of slice A's ten A/A runs, and §15
records whether that held for sixty.

**No threshold moves in slice B.** The rule decides which benchmarks the existing
thresholds may enforce, not what the thresholds are. If too few pass, raising 1.10 is a
change to this document, with its own calibration, in a later slice. Picking a
threshold after seeing which one the data allows is not calibration.

### 14.3 Deferred to slice C

Each is independent and none blocks enforcement: allocation classes for `pkg/wal`, the
`sink=count` replay variant, and moving obsoak onto a shared probe.

## 15. What slice B found

Written on 2026-10-06, after the calibration and power runs.

### 15.1 Calibration: 60 A/A′ runs

Dispatched together on `ubuntu-latest`, `mode: aa`, `enforce: true`. They landed on six
CPU models: AMD EPYC 7763 (25 runs), 9V45 (15), 9V74 (12), Intel Xeon Platinum 8573C
(5), 8370C (2) and Xeon 6973P-C (1). **All 60 passed.** No false failure in 60 bounds the
per-run false-positive rate below 5% at 95% confidence.

| Benchmark | Failed | Inconclusive | Median range | Max abs(median − 1) | 1 + 3× that | §14.2 decision |
|---|---:|---:|---|---:|---:|---|
| `Engine_CancelReplaceInto` | 0 | 0 | 0.980 – 1.017 | 0.0199 | 1.060 | **enforce** |
| `OrderBook_LevelChurn` | 0 | 1 | 0.980 – 1.037 | 0.0365 | 1.110 | report only |
| `Engine_MatchInto` | 0 | 0 | 0.958 – 1.047 | 0.0473 | 1.142 | report only |
| `TapeReplay/sink=nil` | 0 | 0 | 0.975 – 1.068 | 0.0675 | 1.203 | report only |
| `OrderBook_CancelReplace` | 0 | 0 | 0.906 – 1.058 | 0.0945 | 1.283 | report only |

Allocation counts disagreed between the arms in **none** of the 60 runs, so allocation
enforcement stays on for every gated benchmark.

**One thing the rule was not written to use, recorded for a later slice.** The headroom
criterion is about medians, but a timing failure needs the median above 1.10 **and** the
k-th ratio above 1.05. Across all 300 benchmark-runs, the highest k-th ratio was 1.011,
and between 0 and 8 pairs per benchmark were tainted (mean 2.4). The lower bound is the
part doing the protecting. A criterion stated on it would clear more benchmarks. Per
§14.2, that is a change to this document with its own calibration, not a reading of
this data.

### 15.2 Power

A 70-iteration loop at the top of `Engine.Match`, on a throwaway branch, compared against
`main` in 20 dispatched runs with `enforce: true`:

| Benchmark | Failed | Median range | k-th range |
|---|---:|---|---|
| `Engine_CancelReplaceInto` (enforced) | **20 / 20** | 1.196 – 1.306 | 1.160 – 1.295 |
| `Engine_MatchInto` | 20 / 20 | 1.236 – 1.447 | 1.209 – 1.413 |
| `TapeReplay/sink=nil` | 19 / 19 | 1.166 – 1.281 | 1.120 – 1.223 |
| `OrderBook_*` (not planted) | 0 | 0.976 – 1.052 | — |

§9.4 asked for at least 19 of 20 on each enforced benchmark. The detection floor this
measures, about 1.20× on `Engine_CancelReplaceInto`, is printed in every summary.

### 15.3 What changed on the strength of it

- Push runs pass `-enforce`. The judge, built from the base, fails a push on:
  - any allocation increase not covered by a trailer;
  - a timing failure on `Engine_CancelReplaceInto`;
  - a second inconclusive on that benchmark;
  - a malformed trailer;
  - a gate change without `Bench-Gate-Change:`.
- Timing failures on the other four still print, marked
  `fail (report only: not calibrated to enforce)`.
- **Not yet:** the `pull_request` trigger. §14.1 waits for a week of enforced pushes
  without a false failure. That week first started on 2026-10-06 and restarted on
  2026-10-07 after the false failure §16 records.

## 16. Slice C — written before its code

Three independent additions §14.3 deferred, none of which changes what fails a build
on timing.

- **`TapeReplay/sink=count`.** The same replay with a counting `EventSink` attached, so
  the engine builds and publishes its event stream, the configuration the documentation
  recommends embedding. Its guard compares the sink's count against the events the
  correctness driver recorded on the same tape. It is allocation-gated with the tape's
  slack of 8; its timing is reported, not gated, until a calibration says otherwise.
- **`pkg/wal` allocation classes.** Per-command benchmarks (the `Runner*` family and
  `RotationAppendTail`) repeat exactly and are gated exactly. Whole-log benchmarks
  (checkpoint, read, replay, snapshot write and restore, the three recoveries) repeat
  within a few allocations of totals in the millions. They are gated with a slack of
  80 + 0.01% of base, at one invocation per arm. *(The floor was first written as 16,
  from M4 runs. Ten A/A runs on CI then measured `WriteSnapshot` moving between 71 and
  106 on identical code, and 80 is at least twice the largest range any of them
  showed.)* `RestartWithRetention` is stable but
  costs about 200 s per invocation and is left out for that reason only. Timing is never
  gated on `pkg/wal`.
- **One probe.** `internal/probe` holds the ALU loop and the memory walk, and both
  `cmd/obsoak` and `cmd/benchgate` call it, instead of carrying two copies of the loop.

**The first false failure on `main` came from this slice.** The push of `d14f1ed`
failed: `WriteSnapshot` read 89 on base and 107 on head, on identical code, against the
slack of 16 in its base's judge. The floor of 80 had landed one commit earlier, but a
push is judged from its base, so it took effect one push late. The same commit passed a
dispatched comparison judged by the new floor (102 → 81). The calibrated timing check
was not involved (`Engine_CancelReplaceInto` 1.000). The cause was slice C's slack
shipping with M4 measurements before CI had measured the class. §14.1 waits for a week
of enforced pushes **without** a false failure before `pull_request`, so that week
restarts on 2026-10-07.

## 17. `bench-basic-v1.obt` — the subset every engine can replay

Written before its code; step 1.4 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md).

`bench-v1.obt` uses this engine's whole alphabet: reduce, replace, market orders,
IOC, FOK and post-only. Most engines in [`LANDSCAPE.md`](LANDSCAPE.md) cannot express
half of it, so a digest from them over that tape cannot exist. A second tape draws
only what all of them can express.

- **Commands.** Limit GTC submits and cancels, nothing else. Profile `tape.Basic`:
  submit 65, cancel 35, 64 accounts, 41 price levels from 1000, quantity 1–9, no
  exotic draws, Portable, so no account ever meets its own order.
- **Owner-only cancels.** The other profiles send one cancel in four from the wrong
  account on purpose, to sweep "not yours". An engine with no notion of ownership
  would carry out that cancel, and the tapes would part on a question it cannot ask.
  So `Basic` sets `OwnerOnly`. A cancel that names an order goes out from that
  order's account. A cancel that names a position where no order was made, or an
  order already gone, still goes out, and every engine refuses it. The flag follows
  `Portable`'s rule: every draw is still taken, and only the value written changes,
  so it cannot move any other profile's stream.
- **Size and seed.** 50,000 commands, seed `0x5EED1234`, capacity 1,000,000, the
  same as `bench-v1`.
- **Held to.** The file's SHA-256 is pinned, and the generator must reproduce it.
  Replaying it through `refmatch` must give the engine's digest, which is committed as
  `bench-basic-v1.digest`. A test checks that every command is a limit GTC submit or
  a cancel, and that every cancel naming an order comes from that order's account.
  The shape floors are §2.1's: no full-book refusal, no self-trade decision, under 5%
  of submits rejected, at least 1,000 resting at the end, at least one trade per four
  commands, and at least 10% of cancels reaching a live order.
- **Not gated.** Nothing in the bench gate reads it. It exists for step 1.5's
  cross-engine comparison, where the digest is the check that each engine did the
  same work before its time means anything.

## Appendix: Review points not taken

- **Statistics B6: skip `TestBenchTapeRefmatchAgrees` under `-race` through a build tag.** Measured instead. At n = 50 k, refmatch takes 2.5-3.4 s under race and the engine 0.3 s, which is small enough to run everywhere.
- **Statistics D1: a `ci.yml` test asserting that the comparator's constants equal the numbers quoted in this document.** Not taken. The judge is built from base, so head cannot change its own thresholds. Parsing prose for constants is brittle. Slice B's `Bench-Gate-Change:` trailer covers the deliberate case.
- **Statistics D7: a committed ledger of accepted trailers.** Not taken. §12 records the calibration and sabotage numbers, retention goes up to 90 days, and the summary prints a 90-day trailer count (slice B).
- **Statistics F1: record heap size because `Engine_MatchInto` preallocates 2×N orders.** Not taken. It is identical in both arms at a fixed N and is not a gate input.
- **Statistics B7: attach a `-gcflags=-m` diff to an allocation failure.** Deferred to slice B with the trailer it supports, not dropped.
- **Statistics A1: keep a rerun when the first run fails and the second is inconclusive.** Not taken. A fail stands on one run, because the order-statistic bound already accounts for noise. Only inconclusive runs are retried.
- **Scope G: defer `make bench-check`.** Not taken. Changes land by direct push (1 merge in 341), so a local run is the only check before a change reaches `main`. It reuses `cmd/benchgate` and costs one Makefile target.
- **Scope G: defer the anchor comparison.** Not taken. Without it, cumulative drift (statistics A4) is invisible. It reuses the compare path with a different base and stays report-only.
- **Scope A4: define and measure `AimLive` before freezing the target.** It was measured instead, and the knob was dropped (§2.1). The 50% target became a measured floor of 10%.
- **Draft: put the `compare` job in `bench.yml`.** Changed. Workflow triggers apply to every job, so the new triggers would also run the publishing job. The gate gets its own file.

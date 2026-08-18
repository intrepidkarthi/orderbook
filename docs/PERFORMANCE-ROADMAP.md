# Performance and production roadmap

Last surveyed against the code: **2026-08-17**. Every milestone below carries a status
block, and every claim in one names the file it was checked in.

> **How to read a status block.** A status block records what a survey found in the
> *code*, not what a spec said it intended. The distinction is not pedantry: the
> previous status pass reported M8's gap as "no trade id on the wire" because it read
> the problem statement at [`TRADE-BUST.md`](TRADE-BUST.md) §3.6 — accurate on the day
> it was written — as though it described the present. Wire v3 had closed it, the same
> file said so eight lines further down, and the pass never got there. So: a status
> block cites `path:line` in `pkg/`, `cmd/`, `internal/` or `examples/`, or it cites a
> test name. A status block that can only cite another document is not evidence.
>
> **"Out of scope by design" is a third state, not a soft "missing".** Consensus,
> quorum commit and split-brain *prevention* are refused with reasons at
> [`REPLICATION.md`](REPLICATION.md) §9 and [`THREAT-MODEL.md`](THREAT-MODEL.md)
> §5. Counting those as unfinished work would misreport the project's shape for as
> long as the refusal stands.

This is the implementation plan for turning `orderbook` into a high-performance, production-shaped matching component that can be compared honestly with serious C++ and Rust order books.

The goal is not to claim that Go is automatically faster than tuned native code. The goal is to build a deterministic, correct engine with a zero-allocation hot path, competitive throughput, controlled tail latency, reproducible recovery, and a measured end-to-end performance envelope.

This document covers two related tracks:

1. **Performance:** make the matching path and its surrounding pipeline as fast and predictable as the workload requires.
2. **Production readiness:** make ordering, durability, recovery, failover, security, reconciliation, and operations explicit and testable.

The matching core should remain narrow. Clearing, settlement, credentials, consensus, and venue-wide coordination belong in layers around it.

---

## Current position

The repository already has several sound foundations:

- one deterministic writer per symbol;
- integer ticks and lots;
- price-time matching;
- a `Runner` concurrency boundary;
- bounded queues and cancel-preserving overload behavior;
- WAL and snapshot recovery;
- event-stream reconstruction tests;
- replication seams and failure drills;
- fuzzing, race testing, and soak testing;
- a clear distinction between the raw engine and the durable gateway path.

The benchmark numbers were once primarily core or in-process measurements. Three of the
five gaps that sentence used to name are now closed: the **durable path**
(`pkg/wal/durable_bench_test.go:26-119`, published at [`BENCHMARKS.md`](BENCHMARKS.md)
§"The durable path"), the **network path** (`cmd/obsoak` and [`SOAK.md`](SOAK.md)), and
**connection scale** ([`SOAK.md`](SOAK.md) §"Connection scaling" — a 10/20/40/80
sweep, which corrected an earlier wrong "connection wall" conclusion). Two remain open
and are M13's: **multi-day behaviour** — nothing has run longer than four hours — and a
**production hardware envelope**, since every automated run is one shared CI runner.

The current architecture should grow **outward**, not by turning `pkg/matching` into the whole exchange:

```text
Ingress / authentication / risk
            |
            v
Sequencer / commit policy
            |
            v
One deterministic matcher per symbol
            |
            v
Journal / execution reports / market data / drop copy
            |
            v
Ledger / clearing / settlement / surveillance
```

---

## Milestone status at a glance

Surveyed against the code on 2026-08-17. "Partial" spans a wide range here, so the
one-line summary says *which part*.

| # | Milestone | Status | The short version |
|---|---|---|---|
| M0 | Freeze the production contract | **partial** | Lifecycle, phase, MD-sequence, degraded and compat contracts all exist and are tested. No ADRs, no durability vocabulary, no client-visible commit state, no execution→command trace. |
| M1 | Explicit ingress sequencing | **partial** | Four of five acceptance criteria have tests (replay-equals-live, cancel/new races, duplicate suppression, byte-identical tape). The sequenced command *record* does not exist, and the sequence is assigned inside the matcher, not ahead of the queue. |
| M2 | Durability and acknowledgement explicit | **partial** | Two commit modes ship and are tested; crash boundaries are covered exhaustively. Nothing is in telemetry — not one sequence gauge — and no client can tell durable from accepted. |
| M3 | WAL and snapshots operationally bounded | **mostly done** | Segmentation, rotation, retention, archival, disk thresholds and bounded recovery all shipped. The per-book-size capacity table and the `wal*` inspection tools do not exist. |
| M4 | Safe high availability | **partial, bounded by a refusal** | A drilled primary-backup reference with twelve CI drills and a client incarnation fence. Fencing itself is a declared non-goal; the eight-state HA model, a committed watermark, lag metrics and `obgw` wiring are genuinely missing. |
| M5 | Supported client protocol | **partial** | The spec, golden vectors, version refusal, size and rate limits, resume and reconciliation all exist. The codec is `internal/` and outside the compat promise, so there is no reference client library, no conformance harness and no protocol fuzzing. |
| M6 | Complete security lifecycle | **partial** | Authentication, hashed credentials, TLS, per-account DoS control and privilege separation are real and tested. Rotation, revocation, expiry, session invalidation, audit events, brute-force protection and any authorisation model are absent. |
| M7 | Independent reconciliation | **not started** | The word "ledger" appears in no Go file. Every divergence property the repo claims is proven by an in-process test and by no running consumer. |
| M8 | Market-data guarantees | **partial** | Commit point, dense sequence, retention, eviction, incarnation fence and full bust handling are built and proven over sockets. No subscriber-lag metric, no gap counter, no resnapshot limit, no drop copy. |
| M9 | Model-based matching tests | **done for the continuous session** | `internal/refmatch` is an independent reference matcher and `pkg/matching/differential_test.go` compares a whole `Observation` after every command of a generated tape. Twenty-one deliberate engine mutations are all caught, each shrinking to 1-4 commands. Time-based TIF, the exotics and the auction are a written-down tier 2, and building this found three engine defects. |
| M10 | Performance laboratory | **partial** | A substantial Go-only lab: seven latency scenarios to p99.99, durable-path and recovery benches, allocation pinned by direct measurement. No shared tape, no portable digest, no stage attribution, and the cross-language half does not exist in the repository. |
| M11 | Optimize matching data structures | **partial** | Profile-first discipline is followed and one measured index replacement landed (cancel 47.7 ns → 23.3 ns). Experiments 2, 3 and 5 are untouched, 4 is half done, and no alternative is kept behind an interface for A/B — which is what the milestone asks for. |
| M12 | Control runtime and hardware behavior | **not started** | Established by grep: `GOMAXPROCS`, `LockOSThread`, `GOGC`, `GOMEMLIMIT` and `PGO` each appear exactly once in the repository, in this file. |
| M13 | Capacity and operational evidence | **partial** | A real harness, a nightly hosted soak, and a 4 h × 3-book × 14.4 M-message run with flat goroutines and descriptors. No 24 h run, no reconnect storm, no slow reader, no clock drill, and the load carries only limit GTC and IOC. |
| M14 | Observability and incident operations | **partial** | Sixteen metric families, alert thresholds written down, every runbook entry drilled in CI, and a reference dashboard. The venue now counts what it REFUSED and times its own durability path, snapshots and recovery ([`LAG-AND-SHED.md`](LAG-AND-SHED.md)); replica, commit, feed and clock lag are still missing, as are structured logging, trace ids and machine-readable alert rules. |
| M15 | Venue functions outside the matcher | **out of scope by design** | Deliberately not built here; see [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) §"The financial stack — absent by design". |

Two things this table is careful not to do. It does not count a **declared refusal** as
missing work — M4's fencing and M15 in its entirety are decisions, not backlog. And it
does not let a milestone with strong foundations read as nearly done. M9 was the
standing example — good tests, near-zero on its central ask, which the "partial" label
alone would have hidden. It is now built for the continuous session, and its label says
which session, because "done" over a tier-1 alphabet is not "done".

---

## Performance definition

Every performance claim must name its workload and measurement conditions. Track at least:

- throughput at stable saturation;
- P50, P99, P99.9, and P99.99 latency;
- maximum observed latency;
- queueing delay;
- matching delay;
- WAL append and sync delay;
- publication delay;
- allocations and bytes per command;
- run-to-run jitter;
- recovery time;
- behavior at book depth and symbol count.

Do not publish one headline `ns/op` number as a venue capacity claim.

---

# Milestones

## M0 — Freeze the production contract

> **Status: partial.**
>
> **Built.** Order lifecycle states and their transitions (`pkg/types/order.go:56-65`,
> tested in `pkg/types/order_test.go`). The venue state model with six phases
> (`pkg/matching/engine.go:51-70`), transition-tested in `pkg/matching/phase_test.go`
> and `state_test.go`, exported operationally as `orderbook_phase`
> (`cmd/obgw/admin.go:326`). Every outbound message mapped to a state, with the
> solicited/unsolicited failure rules and a reason-code table
> ([`PROTOCOL.md`](PROTOCOL.md) §"Responses"), round-trip tested at
> `pkg/orderentry/reason_test.go:35`. "Your order is not live" is a defined state
> rather than a timing artefact — resolution happens at the front of the matching
> queue (`pkg/matching/engine_loop.go:226-246`), pinned by
> `TestTheResolverRunsAfterEveryEarlierCommand`. Market-data sequence and resnapshot
> rules (`pkg/marketdata/feed.go`, enforced at `cmd/obgw/mdserver.go:120-123`).
> Degraded behaviour: staged disk-full degradation and a latched sync failure
> (`cmd/obgw/server.go:696`, `:818`), each drilled in CI (`cmd/obgw/drills_test.go`).
> API and wire compatibility policy ([`COMPATIBILITY.md`](COMPATIBILITY.md)), enforced
> by `TestExportedSurfaceIsFrozen` against `internal/apicheck/testdata/surface.txt`
> plus golden wire vectors.
>
> **Missing.** *No ADRs* — `find` returns no ADR file and no `docs/adr/`; the closest
> artefact is [`SPEC.md`](SPEC.md) §6, which records five decisions with rationale but
> carries no status or superseded field and stops at v0.3.0, covering none of the WAL,
> rotation, recovery, replication, bust or multi-symbol decisions since. *The
> durability vocabulary below exists nowhere but this file* — `received` /
> `sequenced` / `durable` / `committed` name no identifier in any package. *A client
> cannot tell durable from accepted*: `wire.Accepted`
> (`internal/wire/wire.go:596-605`) carries ClOrdID, price, quantity and side, and no
> ack carries a commit state or a sequence. *An execution cannot be traced to one
> sequenced command*: `matching.Event.Seq`, `types.Trade.SequenceNum` and
> `wal.Entry.Seq` are three independent sequence spaces and nothing joins them.
>
> **Out of scope by design.** Consensus and quorum commit states
> ([`REPLICATION.md`](REPLICATION.md) §9). Ingress ordering *fairness* is assigned to
> the gateway layer rather than the matching core
> ([`THREAT-MODEL.md`](THREAT-MODEL.md) §5) — but this repository ships that gateway
> as `cmd/obgw`, so M1's ingress gap is real work rather than an exclusion.

Before changing the hot path, define the semantics that clients, operators, recovery, and replicas depend on.

### Deliverables

- Order lifecycle state model.
- Command sequencing contract.
- Acknowledgement and durability states.
- Replication and commit states.
- Market-data sequence and resnapshot rules.
- Failure and degraded-state behavior.
- API and wire compatibility policy.
- Versioned architecture decision records.

Use precise terms such as:

```text
received
validated
sequenced
accepted
applied
resting
executed
durable
replicated
committed
published
settled
```

### Acceptance criteria

- Every response maps to one defined state.
- Every state transition has a failure rule.
- A client can determine whether an order is durable or merely accepted by the process.
- An execution can be traced to one sequenced command.
- Tests cover the state transitions.

---

## M1 — Add explicit ingress sequencing

> **Status: partial — four of five acceptance criteria are met and tested; the
> milestone's central artefact does not exist.**
>
> **Built.** *The same sequenced tape produces byte-identical state and events* — met,
> and strongly: `TestCrashAtEveryBoundary` (`pkg/wal/boundary_test.go:54`) replays the
> first *k* records at all 401 boundaries of a 400-command tape and asserts both an
> identical book digest and an identical trade tape;
> `TestCrashAtEveryBoundaryWithSnapshot` (`:152`) does the same across the
> snapshot+tail join. *Cancel/new races are deterministic* — resolution moved onto the
> matching goroutine ahead of the journal (`pkg/matching/engine_loop.go:228-246`),
> tested by `TestResolutionHappensBeforeTheCommandIsJournalled`. *Duplicates cannot
> produce duplicate executions* — two layers, venue-wide live-ClOrdID admission
> (`pkg/orderentry/stream.go:500`) and the engine's bounded dedup ring
> (`pkg/matching/engine.go:217-231`), the latter surviving snapshot and replay
> (`pkg/matching/dedup_recovery_test.go`). *Replay uses the same record as live
> execution* — `wal.Entry` is both, and admission checks are not bypassed on replay
> (`pkg/matching/replay_admission_test.go`). Write-ahead ordering is pinned by
> `TestRunnerLogIsWriteAhead`.
>
> **Missing.** *The sequenced command record itself.* `wal.Entry`
> (`pkg/wal/wal.go:109-143`) carries none of: a venue sequence distinct from the log
> sequence, a session or gateway id, a received timestamp, a sequencer timestamp, or a
> venue incarnation. The account arrives only as `types.Order.UserID`, so a command
> with no order — a cancel, a halt — carries no session identity at all. *Sequence
> assignment happens after the queue, not before it*: `cmd/obgw/server.go:1181` hands
> the order to `Runner.TryEnqueue` unstamped, and the sequence is first assigned inside
> the matching goroutine at `pkg/matching/engine_loop.go:247`. The venue's total order
> is therefore Go channel arrival order among N connection goroutines. *Auditable* is
> half met in consequence: the order is reproducible but no record states when a
> command arrived at the socket versus when it was sequenced, so no post-hoc audit can
> show two gateways were ordered by arrival rather than by scheduling luck. *No stale
> old-incarnation rule on the inbound path* — the fence exists only for outbound
> resume. *No sequence continuation after promotion* —
> `examples/replication/follower.go:203-215` zeroes `snap.WALSeq` and opens a fresh
> log, recording nothing.

`Runner` preserves queue order, but concurrent producers do not currently have a venue-level ordering policy. A production venue must define ordering before the matcher sees a command.

### Repository changes

Add a sequenced command record containing at least:

- venue sequence;
- session or gateway ID;
- account ID;
- client order ID;
- received timestamp;
- sequencer timestamp;
- command type and payload;
- venue incarnation.

Define behavior for:

- new order versus cancel races;
- concurrent gateways;
- duplicate commands;
- retransmission;
- reconnects;
- stale commands from an old incarnation;
- sequence continuation after promotion.

The matcher should consume an ordered tape. It should not infer market priority from goroutine scheduling. That sentence is still an accurate description of the gap — it is the *arrival* half that is unbuilt, not the ordering half. The tape is already ordered, journalled and replayable; what nobody can state is the rule by which two gateways' commands entered it.

### Acceptance criteria

- ✅ The same sequenced tape produces byte-identical state and events. — `pkg/wal/boundary_test.go:54`, `:152`; `pkg/matching/digest_test.go:50`, `:63`.
- ⚠️ Concurrent ingress produces an auditable total order. — reproducible, not auditable: no received-versus-sequenced record exists, and `cmd/obgw/admin.go` has no ingress-timestamp or sequencer-delay metric.
- ✅ Cancel/new races have deterministic results. — `pkg/matching/resolve_test.go:20`, `:115`; contract at [`PROTOCOL.md`](PROTOCOL.md).
- ✅ Duplicate commands cannot produce duplicate executions, within a stated bound. — `pkg/matching/dedup_recovery_test.go:32`, `:60`; end to end at `cmd/obgw/bust_e2e_test.go:159`.
- ✅ Replay uses the same sequencing record as live execution. — `pkg/matching/replay_admission_test.go`.

---

## M2 — Make durability and acknowledgement explicit

> **Status: partial — the mechanisms are built and tested; none of them are visible.**
>
> **Built, and this section's own prose was out of date until this survey.** *Two
> commit modes ship.* The default is group commit on a 20 ms ticker
> (`cmd/obgw/server.go:554-560`, `syncLoop` at `:668`). The second is
> `-sync-every-command` (`cmd/obgw/main.go:116`), implemented as a `CommandLog`
> decorator (`cmd/obgw/synclog.go:29-104`) that fsyncs each record before returning,
> and asserted the only way that means anything — a second file descriptor reads the
> file immediately after the append — by
> `TestSyncEveryCommandIsDurableBeforeApply` (`cmd/obgw/synclog_test.go:21`),
> including a subtest covering all ten append kinds. Those are the `process-accepted`
> and `primary-durable` modes below, built. *The operations are separate in code*:
> append (`pkg/wal/wal.go:613`), buffer flush and fsync (`:1022`, `:1028`), engine
> apply (`pkg/matching/engine_loop.go:248-300`), replica applied
> (`examples/replication/follower.go:134`), primary written
> (`examples/replication/primary.go:113`), client acknowledgement
> (`pkg/orderentry/stream.go:663`). *Crash tests exist and are exhaustive rather than
> sampled* — see the amended list below. *Recovery never silently converts an
> acknowledged durable command into a missing one*: it refuses rather than serving a
> plausible book (`pkg/wal/validation_test.go:21`, `:57`, `:75`, `:131`;
> `integrity_test.go:79`), with a torn tail distinguished from corruption at
> `integrity_test.go:164`. *Sync failure is fail-stop, not a silent lie*:
> `failDurability` (`cmd/obgw/server.go:696`) halts the book, fails `/readyz` and
> latches until restart.
>
> **Missing.** *Commit state is in no metric.* `registerGauges`
> (`cmd/obgw/admin.go:252-407`) exports queue depth, resting orders, prices, phase,
> WAL bytes, WAL segments, disk free, connections, goroutines, heap and descriptors —
> and **not one sequence**. There is no gauge for the WAL written sequence, the
> last-applied sequence, the last-synced sequence, commit lag or replica lag, so the
> loss window [`REPLICATION.md`](REPLICATION.md) §4 says is "on a graph, at all times"
> is on no graph this gateway serves. *WAL append and fsync latency are not measured
> separately from anything*: the only histogram in the process is
> `obgw_message_apply_latency_ns` (`cmd/obgw/admin.go:496`), which times the whole
> inbound handler. *No reconciliation metadata reaches a client* — of the five named
> below only the incarnation does, as `LoginAccepted.Session`. `wal.Writer.Seq()`
> exists in-process and is exported to no metric, no report and no message. *The
> commit mode is venue-wide and invisible*: a process flag announced only to the
> operator's log, so a client integrating against two venues cannot discover its own
> recovery-point objective from the protocol. ~~*`Runner.SetPhase` is a mutating command
> that is not journalled*~~ — **closed**: `wal.KindSetPhase` is journalled and replayed,
> and the command alphabet is now guarded by construction. See
> [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §8 for what building it found.
>
> **Out of scope by design.** A deferred-ack commit pipeline — group commit that holds
> the acknowledgement until the covering sync lands — is declined with a reason at
> `cmd/obgw/synclog.go:20-25`: "that needs a deferred-ack stage the wire protocol here
> does not have, and building half of one would teach the wrong shape." Synchronous and
> quorum replication are declined at [`REPLICATION.md`](REPLICATION.md) §9 and belong
> to M4.

The WAL appends before apply and group-syncs later by default; `-sync-every-command` inverts that. Both are valid policies, and both are only meaningful because the client contract says which is in force — which today it does not, because the mode is announced to the operator's log and to nothing else.

### Repository changes

Separate these operations in code and telemetry:

- WAL append;
- buffer flush;
- filesystem sync;
- engine apply;
- primary durable;
- replica applied;
- replica durable;
- client acknowledgement;
- committed acknowledgement.

Add explicit commit modes, for example:

```text
process-accepted    ✅ built — the default group commit, cmd/obgw/server.go:554-560
primary-durable     ✅ built — -sync-every-command, cmd/obgw/synclog.go:29
replica-durable     ❌ not built — needs an acknowledged replication path (M4)
quorum-committed    ⛔ out of scope by design — REPLICATION.md §9
```

Two of the four exist. What none of them have is a **name in the protocol**: a mode is
a process flag, not a field in `LoginAccepted`, so the client that most needs to know
which one is in force is the one party never told.

Expose enough metadata for reconciliation:

- venue sequence;
- engine sequence;
- WAL sequence;
- commit state;
- incarnation.

### Crash tests

Test a process failure at each boundary. Four of the seven are covered, and not by
sampling — `TestCrashAtEveryBoundary` (`pkg/wal/boundary_test.go:54`) crashes at
**every** command boundary of a 400-command tape and compares both the book digest and
the trade tape; `:152` repeats it across the snapshot+tail join.

- ✅ before WAL append — `pkg/wal/boundary_test.go:54`, `:152`
- ✅ after append, before apply — same
- ✅ after apply, before response — same; replay is asserted to re-emit the same prints
- ✅ after response, before sync — asserted *negatively* at `cmd/obgw/synclog_test.go:41`: under group commit a second reader sees zero records. That is the loss window turned into a test.
- ✅ after sync, before response — `cmd/obgw/synclog_test.go:21`
- ❌ after primary sync, before replication
- ❌ after follower apply, before follower sync — `examples/replication` never kills a follower between applying a record and syncing its own state; `Follower.Promote` writes a base snapshot and opens a fresh log, and no test crosses that seam.

> **A gap in the tape, not in the test — now closed.** The boundary tapes *were* built
> from limit orders and cancels only (`buildTape`, `pkg/wal/boundary_test.go`). No phase
> transition was ever on one — which is exactly why nobody noticed that
> `Runner.SetPhase` is a frozen public mutating command that runs an auction, changes
> state the snapshot carries, and **was absent from `logCommand`'s switch** (falling
> into the `default: return` branch whose comment claims it holds only read-only
> commands). The strongest property in this repository was proven over a command
> alphabet that excluded the one command that escaped the journal.
> **Both are now closed**: the transition is journalled as `wal.KindSetPhase` and
> replayed by re-running the uncross, `buildTape` speaks phase transitions so the
> 401-boundary sweep exercises auctions, and every `cmdKind` and `wal.EntryKind` is
> enumerated by a guard. [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §8
> records the two places that document was wrong about its own guard.

### Acceptance criteria

- ⚠️ A client can tell whether an acknowledgement survived a primary crash. — the *venue* can (both modes ship and are tested); the client is told nothing, because no ack carries a commit state.
- ✅ The loss window is measurable and documented. — [`REPLICATION.md`](REPLICATION.md) §4, [`RUNBOOKS.md`](RUNBOOKS.md); the sequences needed to measure it are exported by `examples/replication` and drilled by D3 and D6.
- ✅ Recovery never silently converts an acknowledged durable command into a missing command. — `pkg/wal/validation_test.go`, `pkg/wal/integrity_test.go`.
- ❌ Commit state is visible in logs, metrics, and reports. — `cmd/obgw/admin.go:252-407` exports no sequence at all.

---

## M3 — Make WAL and snapshots operationally bounded

> **Status: mostly done — shipped in two slices (`e058f19`, `794fd3e`, `04af363`).**
>
> **Built.** Segmented WAL files and live rotation (`pkg/wal/segment.go`,
> `pkg/wal/rotation_test.go`), retention and archival (`pkg/wal/retention.go`), startup
> validation across a segment set, disk-space thresholds and staged disk-full
> behaviour (`cmd/obgw/server.go:818`, `cmd/obgw/diskfull_test.go:33`, `:134`), and
> bounded recovery that skips a snapshot-covered prefix without ceasing to verify it
> (`pkg/wal/bounded_recovery_test.go`). Flags: `-wal-segment-bytes`, `-wal-retain`,
> `-wal-archive` (`cmd/obgw/main.go:109-112`). Specs:
> [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md), [`LOG-ROTATION.md`](LOG-ROTATION.md),
> both of which record where the code disagreed with the design.
>
> **Missing.** The `walcat` / `walverify` / `walinspect` / `walreplay` / `walarchive`
> tools do not exist as binaries — `wal.ReadAll` is what [`RUNBOOKS.md`](RUNBOOKS.md)
> points an operator at instead. No format-migration tooling. On the snapshot side: no
> snapshot-age, snapshot-duration or recovery-duration metric, and a failed checkpoint
> writes a log line without moving a gauge or affecting `/readyz`
> (`cmd/obgw/server.go:748-750`). No copy-on-write or incremental snapshot experiment.
> **The acceptance criteria are not met**: the per-book-size table below — maximum
> snapshot duration, maximum restart duration, maximum WAL tail, disk required, RPO,
> RTO — does not exist as an artefact. Point measurements exist
> (`pkg/wal/restart_cost_test.go`, `recovery_bench_test.go`,
> [`BENCHMARKS.md`](BENCHMARKS.md) §"Recovery time"); the envelope does not, and that
> is shared with M13.

### WAL changes

Add:

- segmented WAL files;
- rotation during live trading;
- retention and archival policy;
- segment-level verification;
- disk-space thresholds;
- disk-full behavior;
- format migration tooling;
- startup validation;
- bounded recovery without reading an unbounded historical file.

Provide tools such as:

```text
walcat
walverify
walinspect
walreplay
walarchive
```

Keep the binary format separate from human-readable inspection output.

### Snapshot changes

Add:

- snapshot age metrics;
- snapshot duration metrics;
- failure alarms;
- copy-on-write or incremental snapshot experiments;
- recovery progress reporting;
- snapshot version migration;
- disk-full behavior;
- atomic replacement tests;
- snapshot/WAL sequence validation.

### Acceptance criteria

For each supported book size, publish:

- maximum snapshot duration;
- maximum restart duration;
- maximum WAL tail;
- disk space required;
- recovery point objective;
- recovery time objective.

Test empty, normal, and maximum-sized books, plus torn and corrupt files.

---

## M4 — Build safe high availability

> **Status: partial, and bounded by a refusal rather than by effort.**
>
> **Built.** A real primary-backup consumer, drilled in CI.
> `examples/replication/primary.go` ships WAL records over TCP off `wal.SetOnAppend`
> with per-peer bounded buffers and attributed shedding;
> `examples/replication/follower.go` bootstraps from a snapshot and never stops
> replaying; `Follower.Promote` (`:190`) turns the standby into a venue and **refuses
> to promote a book with a known gap** (`:199-201`). Twelve drill functions run under
> the plain `go test -race ./...` job: D1 books-agree, D2 mid-stream bootstrap, D3
> promotion preserves the applied prefix after a primary kill, D3-negative refuses a
> gapped promotion, D4 the incarnation fence, D5 refusals replay as refusals, D6 a slow
> follower shed with the lag gauge asserted on both sides, D7 a bust replicates, D8
> multi-symbol plus a wrong-shard negative control, and two D9 drills for reconnect
> across a rotation and bootstrap from below the retained floor
> (`examples/replication/main_test.go:95`–`:566`). The client-side fence is real
> outside the example too: `TestResumeFromAnotherIncarnationIsRejected`
> (`cmd/obgw/server_test.go:468`).
>
> **Missing.** The eight-state HA model below exists nowhere — grep for `PROMOTABLE`,
> `CATCHING_UP`, `FENCED`, `DEGRADED` across `*.go` returns nothing; the reference has
> an implicit two-state model plus an error latch. *Promotion does not check "not
> behind the committed sequence"* — it refuses only on a locally detected gap and
> never compares against a watermark, because nothing in the reference carries a
> committed watermark at all. *Replica lag and commit lag are not observable to an
> operator*: `Primary.LogSeq` and `Follower.Applied` are Go accessors read by drill D6
> only, `examples/replication` imports no `pkg/observability`, and `cmd/obgw` exports
> no such gauge. *`cmd/obgw` ships no replication wiring* — no follower or promote
> flags — so failover is exercisable only through the example binary, which
> [`RUNBOOKS.md`](RUNBOOKS.md) says outright. *No network-partition drill.* *No
> old-primary path*: there is no rejoin or demote entry point, so "the old primary
> cannot rejoin as a writer" is untested because rejoining is unimplemented rather
> than refused. *Promotion is not verified idempotent* — a second `Promote` returns
> "no snapshot ever arrived", a different error than "already promoted", and no test
> covers it.
>
> **Out of scope by design.** Consensus, quorum, leases, STONITH, automatic failover,
> synchronous replication and multi-follower fan-out are explicit non-goals
> ([`REPLICATION.md`](REPLICATION.md) §9,
> [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) §"High availability"). So the
> "choose a fencing mechanism" step below is a **deliberate refusal**, and the
> incarnation fence is documented as detection, not prevention. A reader should not
> count that as backlog while the refusal stands.

Asynchronous replication and an incarnation fence do not prevent an isolated old primary from continuing to trade. The seams are no longer merely seams — they have a drilled consumer — so the honest starting point for this milestone is *a manual-failover reference with a client fence*, and what it lacks is a watermark, a state model, and observability.

### Design decisions

**Declined, with a reason, rather than pending.** Choosing one of the mechanisms below
would force a topology on every embedder; [`REPLICATION.md`](REPLICATION.md) §9 and
[`EXCHANGE-ARCHITECTURE.md`](EXCHANGE-ARCHITECTURE.md) argue that bundling a consensus
answers a question that belongs to the deployment. The list stays here because a venue
still has to pick one — it is just not this repository's pick:

- consensus service;
- quorum election;
- lease service;
- STONITH;
- managed coordination system;
- deployment controller with fencing.

Do not invent an election protocol without a formal safety argument.

### Required state

```text
PRIMARY
FOLLOWER
CATCHING_UP
PROMOTABLE
PROMOTING
FENCED
DEGRADED
STOPPED
```

### Required behavior

- A primary loses write authority when its lease expires.
- Promotion cannot occur behind the committed sequence.
- A follower with a known gap cannot become primary.
- Stale sessions are rejected.
- Promotion is idempotent.
- The old primary cannot rejoin as a writer without explicit recovery.
- Replica lag and commit lag are observable.

### Acceptance criteria

Run drills for:

- ✅ primary crash — D3 (`examples/replication/main_test.go:151`)
- ❌ network partition — the partition-shaped case that exists is D4, run as a unit drill against an unloaded venue
- ✅ follower lag — D6 (`:225`), with the lag gauge asserted on both sides
- ✅ follower restart — D9 (`:529`, `:566`), across a rotation and from below the retained floor
- ❌ old-primary reconnect — no rejoin or demote entry point exists
- ✅ promotion during active flow — D3
- ✅ promotion with a known gap — D3-negative (`:314`), refused
- ✅ duplicate client reconnect — `TestResumeFromAnotherIncarnationIsRejected` (`cmd/obgw/server_test.go:468`)
- ✅ stale incarnation — D4 (`:185`)
- ✅ partial replication — D5 refusals replay as refusals (`:200`), D7 a bust replicates (`:341`), D8 wrong-shard negative control (`:480`)

---

## M5 — Establish a supported client protocol

> **Status: partial — much of the substance exists; none of it is a product surface.**
>
> The single fact that shapes this milestone: the codec is `internal/` and explicitly
> outside the compatibility promise ([`COMPATIBILITY.md`](COMPATIBILITY.md)), so an
> external client must re-implement the wire from prose. The premise sentence below is
> unchanged.

The current wire package is intentionally internal. Before external production adoption, define and test the protocol as a product surface.

### Deliverables

- ✅ public protocol specification — [`PROTOCOL.md`](PROTOCOL.md), 654 lines: session, resume, every message with field widths, reject and reason codes, durability, backpressure, and a reconciliation path for when resume is unavailable
- ⛔ version compatibility matrix — **out of scope by design**: [`COMPATIBILITY.md`](COMPATIBILITY.md) states the policy as "the gateway speaks exactly one version, and an upgrade is a coordinated one", so there is no matrix to build and no negotiation to test
- ⚠️ reference client — `cmd/obdash/feed.go` is a real market-data subscriber living outside the venue's test tree, but there is no importable client *library*
- ❌ reconnect state machine — the rules are prose plus server-side tests; no diagram, no client-side implementation to copy, and no test that drives a full disconnect/resume/reconcile cycle from a client's own state
- ✅ replay and resubmission rules — `Stream.Since` / `ErrSequenceEvicted` (`pkg/orderentry/stream.go:132-154`), gap-or-refuse rather than a silent hole
- ❌ conformance test suite — `internal/wire`'s tests round-trip this repository's own encoder and nothing else
- ✅ golden vectors — 37 files in `internal/wire/testdata/*.hex`, pinned by `TestGoldenVectors` and `TestNewMessagesDidNotDisturbExistingLayouts`, with `TestMessageTypesAreDistinct` added after two byte collisions
- ❌ client certification harness
- ❌ explicit durable and committed response types — `Accepted` is the only positive ack and carries no durability state; `-sync-every-command` changes the timing, not the vocabulary
- ❌ protocol fuzzing — the only fuzz targets in the repository are `FuzzEngine` and `FuzzExoticOrders`; nothing fuzzes `wire.ReadPacket` or the `Decode*` family
- ✅ message-size and rate limits — `wire.MaxPayload = 4096` with `TestPacketRejectsHostileLength`; per-account `gateway.RateGate` with `TestFlooderIsThrottled`

### Acceptance criteria

- ✅ A client can reconnect without ambiguity. — `TestResumeAfterDisconnect` (`cmd/obgw/server_test.go:424`)
- ✅ Duplicate requests are safe. — gateway live-ClOrdID check (`cmd/obgw/server.go:1207`) plus the engine dedup guard that survives snapshot and replay
- ✅ A client can reconcile every execution. — `Query`/`QueryEnd` carries a count and the stream position, across every book (`cmd/obgw/server.go:1747-1805`)
- ⛔ Protocol versions are tested against supported servers. — moot under the single-version policy above
- ✅ Old clients fail clearly rather than being silently misread. — a version byte leads every payload (`internal/wire/wire.go:58`, `Version = 4`) and `TestWrongProtocolVersionIsRefused` proves refusal

---

## M6 — Complete security lifecycle

> **Status: partial — the edge is hardened and tested; the lifecycle around it is not
> built.** The pattern is consistent: everything about *this login, right now* works;
> everything about *a credential over time* is absent.

Authentication is only one part of production security.

### Deliverables

- ✅ external identity-provider seam — `orderentry.Authenticator` (`pkg/orderentry/auth.go:27-29`), wired as `Config.Auth` and preferred over plaintext accounts, proven consumed by `TestTheAuthSeamIsUsed` (`cmd/obgw/tls_test.go:339`)
- ❌ secret-manager integration
- ❌ credential rotation — `Authenticate(account, secret) bool` has no rotation surface; nothing in the repository can make a credential stop working without a restart
- ❌ immediate revocation — same seam, same absence, and both built-ins document it
- ❌ expiration
- ❌ session invalidation — the admin listener mounts only `/metrics`, `/healthz`, `/readyz` and optional pprof; the session type exposes no kill entry point
- ❌ account scopes — no role or scope identifier exists in `pkg/orderentry`, `internal/wire` or `cmd/obgw`
- ❌ operator roles
- ⛔ market-data permissions — **out of scope by design**: the feed is anonymous, stated at `cmd/obgw/mdserver.go:17-20`
- ⚠️ administrative permissions — privilege separation holds *structurally*: the inbound message set (`internal/wire/wire.go:86-98`) contains no halt, resume, bust or force-trade, and a client cannot name another account's order (`TestCannotCancelAnotherAccountsOrder`). But the admin listener itself is unauthenticated, and `-pprof` mounts `net/http/pprof` on it — a heap dump of everything the venue holds, available to anyone who can reach the port. The only control is network separation, stated as an assumption at `cmd/obgw/admin.go:22-31` rather than enforced.
- ❌ brute-force protection — the rate gate is applied to order submission *after* login; nothing throttles, delays or locks out repeated login attempts per account or per source address
- ✅ per-account and per-session DoS controls — `RateGate`, bounded per-connection send queues with disconnection (`TestNonReadingClientIsDisconnected`), and a login deadline for unauthenticated peers
- ❌ security audit events — a failed login writes nothing and counts nothing (`cmd/obgw/server.go:1009-1015`), and `pkg/observability` exports no auth counter, so credential stuffing against a venue is invisible to both logs and dashboards
- ❌ penetration testing

Also built and worth naming, because they are the parts most often assumed missing:
digests at rest with `obgw -hash-secret` provisioning, tests for malformed digests *by
line number* and world-readable credential files, secrets kept out of the log
(`cmd/obgw/tls_test.go:167`), TLS on every listener with the handshake off the accept
loop, and authentication that denies by default without revealing which half was wrong.

Keep these concepts separate:

```text
human identity
organization
trading account
risk group
clearing account
session
credential
operator role
```

### Acceptance criteria

- Revoked credentials stop working immediately.
- A compromised session can be invalidated.
- Customers cannot invoke privileged engine operations.
- Operators cannot accidentally submit customer orders.
- Security events are retained and searchable.

---

## M7 — Add independent reconciliation

> **Status: not started.** The word "ledger" appears in `docs/PERFORMANCE-ROADMAP.md`
> and in **no Go file in the repository**. There is no drop-copy listener in
> `cmd/obgw` and no `cmd/` binary that reads the WAL.
>
> **The substrate exists and is strong**, which is exactly the trap this milestone
> guards against. A sequenced immutable event stream covering ten event classes
> (`pkg/matching/event.go:21-31`); the event stream provably reconstructs the book
> (`TestEventStreamReconstructsBook`); the L2 feed provably cannot drift from the book
> under a random tape (`TestL2FeedAgreesUnderARandomTape`); replica divergence caught
> by `EngineSnapshot.Digest` in drills D1, D2 and D7; CRC-32C-checked WAL records where
> a gap is terminal for a follower; a CAT-style audit sink demonstrated in
> `examples/gateway/main.go`; and a client that can reconcile its own open state via
> `Query`/`QueryEnd`. **Every one of those is a property asserted by a test, not a
> running consumer that could notice a divergence in production.** That distinction is
> the whole milestone, and this project's own record says a seam is not a capability
> until something consumes it — five phantom seams have been found that way.
>
> **Missing, specifically.** No process compares the five sources against each other:
> nothing reads WAL records *and* the market-data feed *and* an account's execution
> stream and checks that the same trade appears in all three with the same id, price
> and quantity. **No durable execution-report journal to reconcile from** — an
> account's outbound stream is an in-memory ring (default 8,192) and `Since` returns
> `ErrSequenceEvicted` past it, so a would-be ledger has no complete record of what
> clients were told. This is a hard prerequisite, not a detail. No drop-copy edge. No
> standalone sequence-gap detector an operator could point at a live venue. No runtime
> market-data-versus-book comparator. And one of the nine event classes below **cannot
> be consumed at all**: there is no session-disconnect record — `MsgKind` stops at
> `KindBusted` and `matching.EventKind` has no connection concept.

The matching engine must not be the only system capable of explaining what happened.

### Build an execution ledger

Consume immutable events for:

- accepted orders;
- rejected orders;
- resting orders;
- cancels;
- reductions;
- replacements;
- executions;
- busts;
- halts;
- session disconnects.

Independently compare:

- client requests;
- gateway responses;
- engine events;
- WAL records;
- replica state;
- execution reports;
- market data;
- fees and positions.

### Acceptance criteria

The ledger detects:

- missing executions;
- duplicate executions;
- incorrect quantity;
- incorrect price;
- incorrect account;
- sequence gaps;
- replica divergence;
- market-data divergence;
- unexplained order state.

---

## M8 — Strengthen market-data guarantees

> **Status: partial — the correctness half is built and proven over real sockets; the
> operability half is entirely absent.**
>
> ⚠️ **This is the milestone the previous status pass got wrong.** It reported the gap
> as "neither `wire.Executed` nor `wire.MDTrade` carries a trade id, so no client can
> be told about a bust." That was true when [`TRADE-BUST.md`](TRADE-BUST.md) §3.6 was
> written and is false now, and the same file says so in place eight lines later. Wire
> **v3** put `TradeID` on both payloads and added `Busted` (`U`) and `MDBust` (`u`);
> the wire is now at **v4**; and `TestBustReachesBothEdges`
> (`cmd/obgw/bust_e2e_test.go:21`) drives a bust across both edges over real sockets
> with two counterparties and a subscriber all agreeing on the trade id. Anyone
> surveying this milestone should check `internal/wire/wire.go` before quoting a spec's
> problem statement.

The market-data design should preserve the matcher’s isolation while making feed correctness independently verifiable.

### Deliverables

- ✅ feed commit point — `Feed.Snapshot` reads book and sequence under one lock (`pkg/marketdata/feed.go:335-345`); `OnEvents` aggregates and sequences under the same lock. Falsified by test rather than by doc: `TestSnapshotPlusDeltasEqualsTheBook` drives a 2,000-command random tape, snapshots every 97 commands, and asserts snapshot+deltas equals the engine's own book from every join point
- ❌ subscriber lag metrics — `registerGauges` registers nothing about market data: no subscriber count, no per-subscriber cursor lag, no publication lag. `obgw_publisher_dropped_total` is the *order-entry* publisher
- ❌ gap metrics — `mdRejectAndClose(conn, wire.MDRejectEvicted)` increments nothing, so an operator cannot tell how many subscribers fell off the retention ring — even though [`RUNBOOKS.md`](RUNBOOKS.md) has a runbook entry for exactly that event and names no metric to detect it
- ❌ resnapshot limits — `handleSubscriber` applies a 10 s subscribe grace and a 120 s idle timeout; nothing bounds how often an anonymous peer may take a full book snapshot, and the market-data listener has no rate gate at all (`gateway.RateGate` is per-account and fronts order entry only)
- ❌ drop-copy feed — no type, package or consumer implements one; `examples/eventfeed/main.go` demonstrates the `EventSink` seam a drop copy would sit on
- ❌ trade-feed reconciliation — nothing continuously compares the market-data trade tape against engine trades or per-account execution reports. Shared with M7
- ⛔ per-subscriber state — **out of scope by design**, recorded at `cmd/obgw/mdserver.go:173-178`: "Polling rather than subscribing keeps `Feed` free of per-listener state." Listed because the milestone asks for it; it is a decision, not an oversight
- ✅ retention policy — bounded ring set at `NewFeed(incarnation, retain)`, with an explicit `ErrSequenceEvicted` rather than a silently truncated slice, and `TestRetentionIsBounded`. Wired to the wire edge as `wire.MDRejectEvicted` on both resume and mid-stream, tested at `cmd/obgw/mdserver_test.go:330`, `:237`, `:268`
- ✅ explicit handling for bust events — `UpdateBust` published from `EventBusted` with the trade id and reason and **deliberately no book rewind** ([`TRADE-BUST.md`](TRADE-BUST.md) §2); `pkg/marketdata/bust_test.go:127` proves a subscriber resuming from *before* the bust still learns of it
- ✅ distinction between conflatable and non-conflatable — architecturally real, not prose. Market data is one anonymous broadcast whose answer to "too far behind" is a fresh snapshot; execution reporting is a per-account durable `Stream` with a resume ring that outlives the connection. Two listeners, two backpressure answers

Also built: a dense gap-free sequence (`TestSequenceIsDenseAndGapFree`) and an
incarnation fence on resume (`TestResumeAcrossIncarnationsIsRefused`,
`TestMarketDataRejectsAnotherIncarnation`).

A slow market-data subscriber may be disconnected or resnapshotted. Critical execution reporting must have stronger guarantees. **Conflation itself is not implemented** — a slow subscriber is disconnected, never conflated-and-resnapshotted — and that choice is stated and defended at `cmd/obgw/mdserver.go:36-40`.

---

## M9 — Add model-based matching tests

> **Status: done for the continuous session, with a written-down tier 2.** The
> reference matcher exists, it is independent, and a generated tape is compared
> against its answer after every command.
>
> **Built.** [`internal/refmatch`](../internal/refmatch) — a deliberately slow
> reference matcher, two sorted slices scanned linearly, written from the price-time
> and order-type rules rather than from the engine, and mechanically prevented from
> importing anything outside the standard library (`TestReferenceMatcherImportsNothing`).
> [`internal/tape`](../internal/tape) — one generator for the whole repository, with a
> deletion-closed command format and a delta-debugging shrinker that prints a shrunk
> tape as a pasteable Go literal. `pkg/matching/differential_test.go` — the adapter and
> `TestDifferentialTape`, which drives 3 profiles x 16 seeds (FIFO, pro-rata on a
> non-zero shard, and a reachable book-size cap) and compares a whole
> `refmatch.Observation` with `reflect.DeepEqual` after each of 2,240 commands: the
> verdict as a mapped enum, the command's trades field by field, the resting book as a
> RANKED L3 list, the L2 aggregate asymmetrically (the engine's maintained `TotalQty`
> and `count` against the model's summed L3), order and trade ids exactly including the
> shard field, the event stream as an ordered list, and the state, last trade price and
> both next-id counters. Per-command snapshot-restore-equals-live on the generated path.
> `FuzzDifferential` with a committed corpus and a nightly `-fuzz` job. Reflection
> guards on both alphabets — every `Config` field classified, every `cmdKind` given a
> tier, every modelled kind asserted GENERATED and every outcome asserted REACHED. The
> `pkg/wal` boundary sweeps now draw from the same generator over a wider alphabet.
>
> **Proven.** Twenty-one deliberate engine mutations (the eighteen of
> [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §7.1 plus three for the assertions
> below) are **all caught**, every shrunk reproduction is **1 to 4 commands**, and the
> twelve sabotage runs of §8 are recorded there including the two that did not behave
> as the spec expected. Deleting the L2 comparison makes two mutations pass against a
> broken engine; comparing the book as a set makes another pass. One seed catches 8 of
> 18; the sweep catches 18 of 18.
>
> **Found.** Three engine defects, two of them predicted from the code before anything
> was built: a rejected fill-or-kill still moves `LastTradePrice` (and therefore the
> price collar); a self-trade-prevented maker can leave the book with the only
> published event being the taker's `REJECTED`, which contradicts the event stream's
> reconstruction claim; and pro-rata's skip-self rule rests a taker across the spread
> and leaves the book crossed. All three are pinned by regression tests and none is
> fixed here — see [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §10.1.
>
> **Missing, and enumerated rather than implied.** DAY and GTD (a function of a clock,
> the cheapest tier-2 item), stops, OCO, icebergs, pegged and trailing orders, the
> auction uncross, and trade busts are **tier 2** — each with a written reason in the
> `commandTier` table, and each a failing test the day a `cmdKind` is added without
> one. Admission controls are never modelled and are held at their disabling values by
> `TestHarnessConfigMatchesItsClassification`. `TestSoak` still checks invariants every
> 50,000 ops of 500,000; a 500,000-op run with a per-op model comparison is a nightly
> job and belongs with M13.

Keep the optimized engine and add a deliberately simple reference matcher for testing.

Generate random command tapes and compare:

- trades;
- remaining orders;
- book depth;
- IDs;
- sequences;
- event streams;
- snapshots;
- replay state.

Cover:

- limit and market orders;
- partial fills;
- IOC/FOK/GTC/DAY/GTD;
- stops;
- icebergs;
- pegged orders;
- OCO;
- trailing stops;
- replace and reduce;
- self-trade prevention;
- pro-rata;
- auctions;
- halts;
- cancel-only mode;
- busts;
- replay and recovery.

Assert after every command:

- ✅ no negative quantity — `checkInvariants`, on generated tapes
- ✅ no duplicate order ID — `runDiff`, on every generated tape; caught mutation 19 by name
- ✅ no order in two states — filled+remaining equals quantity, `checkInvariants`
- ✅ aggregate depth equals resting orders — the engine's MAINTAINED `TotalQty`/`count` against the model's summed L3, after every command of a generated tape. Deleting this assertion makes two engine mutations pass (`REFERENCE-MATCHER.md` §10.3 row 2)
- ✅ trade quantities balance — every print positive, and no order prints more in total than it was submitted for; caught mutation 21 by name
- ✅ sequence is monotonic — `TestEventSequenceIsDenseAndMonotonic`, dense from 1 over a generated tape, plus `mirrorBook` on hand-written scenarios
- ✅ event stream reconstructs state — `TestEventStreamReconstructsBook` on hand-written scenarios, and the ordered event list compared elementwise on every generated command. See `REFERENCE-MATCHER.md` §10.1(b): the generated path found a case where it does NOT
- ✅ replay equals live — every boundary of a 400-command tape (`pkg/wal/boundary_test.go`), over the tier-1 alphabet plus reduce, replace, cancel-all and the control commands. This row was briefly marked ✅ while the recovery tape still drew nothing but GTC limit orders; `tape.Recovery` now carries the whole tier-1 payload and `TestRecoveryTapeSpeaksTheTierOneAlphabet` asserts it by outcome. See `REFERENCE-MATCHER.md` §3.5
- ✅ snapshot restore equals uninterrupted execution, *per command* — two assertions, because the first alone was not the property. `restoreMatchesLive` compares the **restored engine's visible book** against the model after every command of the generated tape (a digest round-trip does not: it is blind to everything `LoadSnapshot` rebuilds, and a restore bug doubling every level aggregate passed all 23 packages). `TestSnapshotRestoreEqualsUninterruptedExecution` then restores at three fork points per tape and drives the **remainder** of the tape through the restored engine, comparing every observation — 48 subtests. See `REFERENCE-MATCHER.md` §3.3(c) and §3.5

---

## M10 — Build the performance laboratory

> **Status: partial — a substantial Go-only lab exists; the two pieces that make it a
> *shared* laboratory do not.**
>
> **Built.** Workload scenarios with stated preloads and quantiles out to p99.99
> (`pkg/matching/latency_scenarios_test.go`): AddOnly, CancelOnly, AggressiveWalk,
> Mixed_70_20_10, MassCancelBurst, STPSweep, ThinBook, plus CancelHeavy. Core benches
> for the engine and the book. **The durable path is measured**
> (`pkg/wal/durable_bench_test.go:26-119`: bare, with sink, group commit, sync-every,
> checkpoint). Recovery, restart, retention and rotation benches
> (`pkg/wal/recovery_bench_test.go`, `restart_cost_test.go`,
> `retention_bench_test.go`). Allocation claims pinned by direct measurement rather
> than Go's rounded `allocs/op` (`pkg/orderbook/alloc_test.go`). End-to-end gateway
> load with a seed and a machine-share control — `cmd/obsoak` runs a fixed-work speed
> probe before and after each run and warns on drift, so two runs on different machine
> conditions are not tabled together. Methodology *and its corrections* are published
> in [`BENCHMARKS.md`](BENCHMARKS.md): book size as a parameter of the result, two
> stale allocation ratios corrected in place, a six-scenario tail table.
>
> **Missing.** *No shared command tape* — every benchmark synthesises its own flow
> inline with modular arithmetic, so there is no seed to record and no tape file
> another implementation could replay. *No portable output digest* —
> `EngineSnapshot.Digest` disqualifies itself in its own comment
> (`pkg/matching/snapshot.go:297-302`: "stable between processes running the same
> release and nothing stronger"), so it cannot certify that a C++ or Rust matcher
> produced the same result. *No stage attribution*: the milestone asks for queue,
> match, WAL and publication delay separately, and the only server-side histogram
> lumps decode, queue wait and dispatch together by its own admission
> (`cmd/obgw/admin.go:486-495`). *No replicated-path measurement, no multi-symbol
> throughput benchmark, no conditional-order workload* (stops, icebergs, pegged, OCO
> and trailing have correctness tests and no benchmark of any kind), *and no "one long
> FIFO queue" workload* — the preloads spread over 4,000 price levels, so a single
> level with a very deep time-priority queue is never measured. *Deep-book scaling is
> published but is not a fixture* — the 200 K-to-10 M table was produced by varying
> `-benchtime` by hand, so no committed benchmark reproduces it. *CI benchmarks record
> no machine conditions and no baseline*: `bench.yml` pipes raw output into the step
> summary, nothing compares against a previous run, and no threshold can fail. *No
> gateway end-to-end latency benchmark* — `soak.yml` states outright that a shared
> runner cannot publish timings, so the only automated end-to-end run deliberately
> produces no latency figure.
>
> **The cross-language half does not exist in the repository.** `find` for `*.rs`,
> `*.cpp`, `*.cc` and `*.hpp` returns nothing. The comparison this project *has* made —
> cancel roughly 3× faster than liquibook, parity with `geseq/orderbook`, which drove
> two real optimizations — survives only as prose in `CHANGELOG.md` (v0.13.0) and
> **cannot be re-run**. That is the single most load-bearing gap in this milestone,
> because it is the one that makes a published comparison unfalsifiable.

Create a shared benchmark harness for Go, C++, and Rust.

### Fair comparison rules

Use:

- identical command tapes;
- identical price and quantity distributions;
- identical book depths;
- identical order mix;
- identical output digest;
- release builds;
- the same CPU and OS;
- pinned cores;
- controlled background load;
- the same warmup and measurement period.

Benchmark both raw engines and complete paths. Do not compare a bare Go matcher with a native engine that has different semantics or additional work.

### Required workloads

- passive insert;
- cancel-heavy market making;
- aggressive sweeps;
- thin book;
- deep book;
- one long FIFO queue;
- many price levels;
- mixed order flow;
- conditional orders;
- multi-symbol throughput;
- durable path;
- replicated path;
- gateway end-to-end path.

### Required outputs

```text
throughput
p50
p99
p99.9
p99.99
maximum latency
allocations
bytes/op
queue delay
match delay
WAL delay
publication delay
```

Record CPU, kernel, Go/Rust/C++ version, compiler flags, GC settings, storage, NIC, NUMA layout, book size, and workload seed.

---

## M11 — Optimize the matching data structures

> **Status: partial — the discipline is followed and one measured win landed; five of
> the seven experiments are untouched, and the milestone's own method requirement (keep
> alternatives behind an interface for A/B) is unmet.**
>
> **Built.** Profile-first discipline is followed *and documented at the call site*:
> the injected clock is read once per command and cached, with the comment stating that
> reading it per fill "made wall-clock reads 46% of the match path in a CPU profile"
> (`pkg/matching/engine.go:586-592`) — and grep confirms no `time.Now()` on the hot
> path in either package. A measured index replacement landed: `orderIndex`
> (`pkg/orderbook/index.go`), Fibonacci hashing over power-of-two buckets with chained
> entries recycled through a free list, "~45 ns through Go's map and ~3.7 ns through
> this", covered by growth, collision and free-list-reuse tests. Account strings are
> interned to a dense `int32` cached on the book node so a cancel never re-hashes. The
> evidence trail is in `CHANGELOG.md` v0.13.0: cancel 47.7 ns → 23.3 ns, established by
> alternating baseline and patched builds with ten patched measurements all below ten
> baseline ones. Hot-path rules hold: no `unsafe` anywhere outside tests, no logging or
> filesystem I/O in the match loop, cancel and replace measured allocation-free.

Only do this after profiling the current implementation.

### Experiments

Benchmark:

1. ✅ Current map plus sorted price levels. — the shipped structure (`pkg/orderbook/orderbook.go`, [`SPEC.md`](SPEC.md) §6.2)
2. ❌ Dense tick array plus bitmap for bounded grids. — grep for "bitmap" or "tick array" returns nothing outside this file
3. ❌ Radix or adaptive radix tree for sparse integer prices. — **not attempted**; [`SPEC.md`](SPEC.md) §6.2 lists it as a deferred alternative and no code implements any of them
4. ⚠️ Index-based order storage instead of pointer-heavy structures. — half done: `orderIndex` removed a Go map but still stores `*node` pointers, and the level FIFO is still an intrusive doubly-linked list rather than slot indices into an arena
5. ❌ Separate hot and cold order fields. — `node` carries the whole `*types.Order` plus prev/next/level pointers; nothing splits what the match loop touches from what it does not
6. ✅ Fixed-size event records instead of interface-heavy hot-path callbacks. — `Event` is a fixed-size struct delivered as a reused batch slice, and a nil sink is zero-overhead
7. ✅ Specialized paths for common limit/cancel operations. — `Engine.Match` into a caller buffer against the ergonomic `Engine.Process`, plus `Reduce` and atomic `Replace`; the allocation difference is asserted by `pkg/orderbook/alloc_test.go` and tabulated in [`BENCHMARKS.md`](BENCHMARKS.md)

**No alternative is kept behind an internal interface for A/B benchmarking**, which is
what the closing rule below explicitly asks for. There is no `Book` interface and no
build-tagged variant — grep for `//go:build` finds only wasm, an ignored legacy file
and the `wal` diskfree split. Starting experiment 2 or 3 today would mean replacing the
structure rather than comparing against it.

Evaluate:

- ❌ cache misses — no `perf stat`, no hardware-counter harness anywhere in the repo
- ❌ branch misses — same; the two criteria at the top of this list have no instrument
- ⚠️ pointer chasing — reduced by `orderIndex`, never measured as such
- ✅ allocation behavior — `pkg/orderbook/alloc_test.go`, measured directly
- ✅ book-depth scaling — [`BENCHMARKS.md`](BENCHMARKS.md), 200 K to 10 M (though not as a committed fixture; see M10)
- ❌ price-range scaling — a sparse book over a very wide tick range is not benchmarked; preloads are confined to 4,000 contiguous levels
- ✅ cancel cost — the headline measurement of v0.13.0
- ✅ level creation cost — `BenchmarkOrderBook_LevelChurn`
- ❌ memory footprint per resting order — unmeasured; grep matches only this file
- ✅ tail latency — seven scenarios to p99.99

One hot-path rule below is still violated in the one place the milestone names:
`EventSink` is an interface and `MultiSink` adds a second indirection per sink.
Amortised per batch rather than per event, and never measured.

Do not replace the existing structure based on theory alone. Keep alternatives behind internal implementations until benchmark evidence selects one.

### Hot-path rules

The strict path should avoid:

- heap allocation;
- decimal arithmetic;
- blocking calls;
- system calls;
- logging;
- unpredictable external callbacks;
- network I/O;
- filesystem I/O;
- unnecessary interface dispatch;
- runtime timers.

---

## M12 — Control runtime and hardware behavior

> **Status: not started**, and this is a negative result established by grep rather
> than inferred from a document. Across all `*.go`, `*.md` and `*.yml`: `GOMAXPROCS`,
> `LockOSThread`, `GOGC`, `GOMEMLIMIT`, `SetGCPercent`, `SetMemoryLimit` and `PGO` each
> appear **exactly once — in this file**. A combined grep for "latency mode",
> "throughput mode", "deployment mode", "affinity", "numa", "isolcpus" and "core
> pinning" over the whole tree returns zero hits, including this file. There is no
> `default.pgo`. `cmd/obgw` exposes no runtime-tuning flag; the only
> performance-adjacent one is `-pprof`.
>
> **CI is a single OS and a single toolchain.** `ci.yml`, `bench.yml` and `soak.yml`
> are all `ubuntu-latest` with Go 1.23. The macOS figures in
> [`BENCHMARKS.md`](BENCHMARKS.md) come from the author's laptop — so the project has
> two platforms' numbers and has **never run the same benchmark under a controlled
> matrix**, and never two Go versions.
>
> **What does exist** is the seam this milestone would be driven from: `net/http/pprof`
> on the admin listener, off by default, already used by the soak workflow. And the
> milestone's one prohibition is honoured — no `unsafe` and no assembly in non-test
> code.
>
> One item here is nearly free and worth doing long before the rest: **record
> `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, the Go version and the kernel in every benchmark
> and soak report**, so the runtime configuration stops being an unrecorded variable in
> every number this project publishes.

Measure Go under controlled configurations:

- fixed `GOMAXPROCS`;
- `runtime.LockOSThread`;
- CPU affinity;
- isolated matcher cores;
- NUMA-aware symbol placement;
- different `GOGC` settings;
- `GOMEMLIMIT`;
- PGO;
- different Go versions;
- Linux and macOS separately.

Define two deployment modes:

```text
throughput mode
latency mode
```

Latency mode should prioritize:

- pinned matcher threads;
- no allocations in the hot path;
- limited runtime interference;
- isolated storage and publisher work;
- explicit backpressure.

Do not use `unsafe` or assembly until profiles prove the ordinary implementation is the bottleneck.

---

## M13 — Establish capacity and operational evidence

> **Status: partial — a real harness with a defensible methodology, and a load that
> exercises a fraction of the venue.**
>
> **The harness is the strong part.** `cmd/obsoak` measures client-observed end-to-end
> latency plus `/metrics` sampling, analyses steady state only after a warmup, judges
> growth on the *floor* rather than the trend, and runs a fixed-work CPU probe before
> and after each run so two runs on different machine conditions are not tabled
> together ([`SOAK.md`](SOAK.md)). A nightly hosted soak runs on cron
> (`.github/workflows/soak.yml`), default 4 h at 1,000 msg/s across three books,
> asserting the **structural** findings only — orphaned orders, client errors, and
> goroutine and descriptor trend equal to zero — and deliberately publishing no
> timings, because a shared runner cannot support them. Heap profiles kept 30 days.
>
> **Longest measured run: 4 hours.** 3 books, 14.4 M messages, 0 errors, goroutines,
> descriptors and book size flat, with an idle-heap follow-up separating cache fill
> from leak. Connection scaling measured to 80 at 5,000/s: 8× the connections for 17%
> more CPU, which corrected an earlier wrong "connection wall" conclusion.
>
> Also covered: disk-full (`cmd/obgw/diskfull_test.go:33`, `:134`, against the real
> `-wal-min-free` marks), snapshot under load, WAL rotation under load (~1.0 GB per
> book in the 4 h run, so segments rotated live), process restart with recovery cost
> benchmarked, slow followers (D6), follower reconnect across a rotation and below the
> floor (D9), and multiple symbols.
>
> **Missing.** *No 24-hour run and no multi-day run* — four hours is the maximum, and
> [`SOAK.md`](SOAK.md) says so: "Nothing has run for a day." *Hundreds or thousands of
> connections are untested under sustained load* — 80 is the highest count ever run and
> only as a CPU probe; the sustained runs use 8 or 25. *No reconnect storm* —
> `cmd/obsoak` has no churn mode and cannot even be re-run against a warm venue,
> because it always logs in asking for sequence 0. *No slow-reader scenario* — the
> market-data edge sheds a non-reading peer only via a write deadline and no test
> induces it. *No network-partition drill under load.* *No clock-change drill and no
> clock-offset signal at all.* *WAL retention has never run in a soak* — deletion is
> off unless `-wal-retain` is set and no soak sets it, so every run so far has the
> pre-retention disk behaviour. *Restart after a long run is untested*: the RTO figure
> is extrapolated from 2 s/GiB cold and 0.75 s/GiB warm rather than observed after a
> day of trading.
>
> **The load itself is the narrowest gap.** `cmd/obsoak` sends **only limit GTC and
> limit IOC** — no stops, icebergs, OCO, pegged, trailing, replace or reduce under
> sustained load. That is exactly the surface `FuzzExoticOrders` exists for, and the
> soak never touches it.

Run:

- ❌ 24-hour soak
- ❌ multi-day soak
- ❌ hundreds or thousands of connections
- ❌ reconnect storms
- ❌ slow readers — `cmd/obdash/main_test.go:171` sheds a slow SSE client, which is the sidecar, not the venue edge
- ✅ slow followers — D6
- ✅ WAL rotation under load
- ✅ disk-full conditions
- ✅ snapshot under load
- ✅ multiple symbols — three books for hours, with cross-book cancel routing by client id alone
- ❌ full order-type mix
- ✅ process restarts — `cmd/obgw/restart_checkpoint_test.go`; ❌ host restarts
- ❌ network partitions
- ❌ clock changes

Verify:

- ✅ no orphan orders — the run's participants-believe-versus-venue-reports check, asserted in CI
- ❌ no duplicate fills — the harness makes no such assertion
- ❌ no lost cancels — same
- ❌ no undetected sequence gaps — same; **the orphan count is the run's only reconciliation check**, which is the M7 dependency showing through
- ❌ no reconciliation divergence — needs M7
- ✅ no memory growth — floor-based, with an idle-heap follow-up
- ✅ no descriptor growth
- ✅ no goroutine growth
- ✅ correct degraded-state transitions — drilled in CI rather than in the soak
- ✅ bounded recovery — `pkg/wal/bounded_recovery_test.go`

The output must be a capacity envelope, not a single throughput claim. **It is not one
yet.** [`BENCHMARKS.md`](BENCHMARKS.md) and [`SOAK.md`](SOAK.md) publish point
measurements at one rate on one machine; the per-book-size table M3 also asks for — max
snapshot duration, max restart duration, max WAL tail, disk required, RPO, RTO — does
not exist as a single artefact.

---

## M14 — Complete observability and incident operations

> **Status: partial — the venue now counts what it refused and times what it waits on
> LOCALLY; the lags that need another party are still missing, and so is the whole
> logging and alerting layer.** The original diagnosis was one sentence — *this venue
> can tell you what it did, and cannot tell you how far behind it is* — and half of it
> has been answered. [`LAG-AND-SHED.md`](LAG-AND-SHED.md) built four signals: refusal
> and shed counters, WAL append and sync latency, snapshot age/duration/failure, and
> recovery duration, each with a threshold in [`RUNBOOKS.md`](RUNBOOKS.md) §"Alert
> thresholds" and a drill that induces the real condition.
>
> What is still missing is the lag between this process and something ELSE: replica,
> commit and feed lag all need a second party or a per-subscriber position this venue
> does not export, and clock offset needs an external reference a process cannot be its
> own source for. Those are named below and argued in
> [`LAG-AND-SHED.md`](LAG-AND-SHED.md) §10 rather than left as a bare cross.

Add metrics for:

- ✅ ingress rate — `orderbook_events_total`
- ✅ accepted and rejected rate — `orderbook_orders_accepted_total`, `orderbook_orders_rejected_total`
- ✅ reject reason — `orderbook_rejections_total{reason}`, with the exact label string pinned against the runbook by `TestDrillTheCeilingRejectionNamesItself` (`cmd/obgw/drills_test.go:355`)
- ✅ queue occupancy — `orderbook_queue_depth` / `orderbook_queue_capacity`
- ✅ queue-full events — `obgw_refused_total{reason}`, seventeen wire-reason series counted in `session.reject`, the single funnel every refusal ON AN ESTABLISHED SESSION passes through, so it is complete by construction over that population. Includes the rate gate's throttles, which never reach a queue at all. Refusals taken BEFORE a session exists — failed logins and refused resumes — speak a different vocabulary and are counted separately by `obgw_login_refused_total{reason}`; review found them counted nowhere, behind a permanently-zero `not_authorised` series that read as evidence of the opposite ([`LAG-AND-SHED.md`](LAG-AND-SHED.md) §4.6). Plus `obgw_shed_unreported_total{op}` for the one shed that produces no wire message: a cancel-on-disconnect sweep the queue would not take, which leaves orders resting for a client that asked for them to be pulled ([`LAG-AND-SHED.md`](LAG-AND-SHED.md) §4)
- ✅ apply latency — `obgw_message_apply_latency_ns`, buckets 100 ns–250 ms with quantiles
- ✅ WAL append latency — `obgw_wal_append_latency_ns`, from a `timedLog` `CommandLog` decorator in `cmd/obgw`. It never contains an fsync, which is the whole rule; the count is asserted equal to `Writer.Seq()` so a command kind cannot go silently untimed
- ✅ WAL sync latency — `obgw_wal_sync_latency_ns`, containing nothing but the fsync. Its p99 **is** the variable half of the published recovery point objective, which was stated as a constant until this existed, and its `_count` is the only heartbeat the group-commit goroutine has ([`LAG-AND-SHED.md`](LAG-AND-SHED.md) §5)
- ❌ commit lag — needs a durable sequence and an applied sequence that mean the same thing on both sides of a failover, which is M2 contract work. Deliberately deferred rather than guessed
- ❌ replica lag — readable only inside `examples/replication` (`primary.LogSeq()` versus `follower.Applied()`); `obgw` has no replication topology, so a lag gauge exported from it would read zero forever, and a metric that is always green is worse than an absent one
- ❌ feed lag — subscriber eviction is defined and drilled; the lag of a subscriber not yet evicted needs a per-subscriber position exported from `marketdata.Feed`, which is a per-connection cardinality question this page has not had to answer
- ✅ publisher drops — `obgw_publisher_dropped_total`
- ✅ snapshot age — `orderbook_snapshot_age_seconds{symbol}`, computed at scrape from the snapshot file's **mtime**, so it survives a restart and reports the true age of the base a venue just recovered from. `NaN` when the venue was not asked to checkpoint; negative, and not clamped, when the host's clock went backwards
- ✅ snapshot duration — `obgw_snapshot_duration_ns` over `wal.WriteSnapshot`, plus `obgw_snapshot_failures_total{symbol}` where the log line already was. A failed checkpoint now moves both a gauge and a counter and adds a degraded clause to `/readyz` — which still returns **200**, deliberately ([`LAG-AND-SHED.md`](LAG-AND-SHED.md) §7). The matching-goroutine pause is still untimed and §6.2 says why
- ✅ recovery duration — `obgw_recovery_duration_ns{symbol}`, spanning the recovery read, BOTH adoptions and opening the log, because the operator's question is how long the venue was down rather than how long `pkg/wal` took. Books recover serially, so `sum()` is the venue's downtime
- ✅ disk usage — `orderbook_wal_disk_free_bytes`
- ✅ WAL growth — `orderbook_wal_bytes`, `orderbook_wal_segments`, per-symbol series
- ❌ GC pauses — `obgw_heap_bytes` is exported via `runtime/metrics`; nothing about pauses
- ❌ clock offset — a process cannot measure its own clock's error; it needs an NTP or PTP reference, which is a deployment integration rather than a metric. The nearest thing this venue has is a NEGATIVE `orderbook_snapshot_age_seconds`, which is not offered as a substitute
- ✅ operation tail latency — quantiles off the apply histogram

Also built and not on the list above: a **matcher-stall signal**
(`orderbook_last_event_sequence` plus a `/readyz` derived from queue occupancy *and*
sequence progress, which is the only way a stalled matcher is distinguishable from a
quiet market), and the venue phase as `orderbook_phase`.

Add:

- ❌ structured logs — there is no `log/slog` anywhere in the repository; every emission is `log.Printf`
- ❌ trace IDs
- ❌ order/session/client correlation — the only correlation field is `types.Order.ClientOrderID`, which is per-order and client-supplied
- ❌ alert rules, machine-readable — there is no rules file; [`RUNBOOKS.md`](RUNBOOKS.md) §"Alert thresholds" is a human table, and it is a good one
- ⚠️ SLO dashboards — `cmd/obdash` is a reference dashboard (an ordinary market-data subscriber plus a `/metrics` reader, drawing the 75% queue threshold and refusing to show a stale number as a healthy one). It is a viewing surface, not an alerting system
- ❌ incident annotations
- ❌ audit-log retention — WAL retention and archival exist, but that is the command journal, not a security event log
- ✅ runbook links in alerts — the WAL-failure readiness string points at [`RUNBOOKS.md`](RUNBOOKS.md)

Define degraded behavior for WAL failure, disk pressure, matcher stalls, replica lag, publisher overload, snapshot failure, authentication backend failure, clock skew, feed lag, and stale-primary reconnection.

**Eight of the ten are defined and drilled in CI**: WAL failure (halts the book, fails
readiness first, latches), disk pressure (`diskfull_test.go:33`, `:134`), matcher
stalls (`drills_test.go` — and the mass-cancel drill proves a sweep is *not* one),
publisher overload, feed lag (subscriber eviction), stale-primary reconnection (D4),
replica lag (D6, though only inside the example), and now **snapshot failure**: the
venue keeps trading, keeps reporting ready, and says so in the readiness body, with the
age gauge and the failure counter carrying the signal and
[`RUNBOOKS.md`](RUNBOOKS.md) §"Checkpoints have stopped landing" carrying the
procedure. That asymmetry with the WAL path is argued in
[`LAG-AND-SHED.md`](LAG-AND-SHED.md) §7 and drilled by
`TestDrillCheckpointFailureKeepsTrading`, which exists so nobody "fixes" it back.

**Two are undefined**: authentication backend failure (`Authenticator` has no failure
mode plumbed to readiness or metrics), and clock skew, for which
[`RUNBOOKS.md`](RUNBOOKS.md) states there is no procedure.

One failure that had no signal at all now has one and is worth naming separately,
because nobody asked for it: **a group-commit loop that stopped**. `walFailed` latches
on a sync that FAILED; a sync that never HAPPENS moved nothing, and the venue would go
on acknowledging orders it was not making durable. A flat
`obgw_wal_sync_latency_ns_count` against a live `orderbook_last_event_sequence` is that
signal, and `TestDrillTheGroupCommitLoopHasStopped` kills the goroutine for real.

---

## M15 — Integrate venue functions outside the matcher

> **Status: out of scope by design, and it should stay that way.** None of the list
> below is built, and [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) §"The
> financial stack — absent by design" says so deliberately rather than as a gap. Two
> partial exceptions worth naming so nobody counts them twice: market-abuse
> surveillance exists (spoofing, layering, order-to-trade ratios, marking the close,
> ramping, pinging and cross-book patterns, each mapped to a real enforcement case in
> [`THREAT-MODEL.md`](THREAT-MODEL.md)), and **drop copy** appears here, in M7 and in
> M8 — it is one deliverable, tracked in M7.

These should remain outside `pkg/matching`, but a real venue also needs:

- fees and rebates;
- credit;
- margin;
- position limits;
- liquidation;
- clearing;
- settlement;
- custody;
- balances;
- corporate actions;
- trading calendars;
- instrument lifecycle;
- tick-size changes;
- regulatory reports;
- drop copy;
- surveillance case management;
- beneficial-owner identity;
- dispute handling;
- disaster recovery;
- retention and audit policy.

---

# Dependency order

Do not start with more order types or more UI work while the system contract is unsettled.

```text
M0 contract
  ↓
M1 sequencing
  ↓
M2 durability and acknowledgement
  ↓
M3 WAL and recovery lifecycle
  ↓
M4 HA fencing
  ↓
M7 independent reconciliation
  ↓
M10 benchmark laboratory
  ↓
M11/M12 hot-path and runtime optimization
  ↓
M13 soak and capacity evidence
  ↓
controlled pilot
```

Security, protocol, market-data, and operations work should proceed alongside these milestones, but their acceptance criteria must reference the same sequencing and commit contract.

**The chain has not been walked in order, and that is worth stating rather than
hiding.** M3 is essentially done while M0, M1 and M2 above it are partial, because M3
had a measurable wall in front of it — a venue left running became unrestartable — and
the three above it are contract work with no forcing function. The practical
consequence is visible in M13: the soak's only reconciliation check is an orphan count,
because M7 does not exist, and M7 cannot be built well until M2 exports a sequence.
Skipping ahead bought real capability and left the debt exactly where this diagram said
it would be.

# Growth rules

Before adding a feature, answer:

1. What state does it mutate?
2. Is it deterministic under replay?
3. Is it in the WAL?
4. Is it in snapshots?
5. Is it in the state digest?
6. Is it in market data?
7. Is it in drop copy?
8. Can it be reconciled independently?
9. Does it add hot-path work?
10. Does it change the protocol?
11. Does it affect cross-symbol behavior?
12. What happens during failover?
13. How is it disabled if it misbehaves?

The project should continue to scale **across symbols and layers**, not by adding parallel writers to one order book.

# Readiness gates

## Research-ready

- Matching correctness tests pass.
- Replay is deterministic.
- Benchmarks are reproducible.
- Results include workload and machine conditions.

## Component pilot-ready

- Durable acknowledgement semantics are explicit.
- WAL rotation and disk pressure are handled.
- Recovery has measured RTO/RPO.
- End-to-end capacity is known for the target workload.
- Security lifecycle is externally backed.
- Reconciliation detects divergence.
- Multi-day soak passes.

## Venue-ready

- Split-brain prevention is proven.
- Commit semantics are enforced.
- Independent ledger and settlement reconciliation exist.
- Failover is rehearsed by operators.
- Security review and penetration testing pass.
- Capacity plan includes failure and peak scenarios.
- Clearing, margin, settlement, regulatory, and audit systems are integrated.

# Immediate next slice

Re-cut on 2026-08-17. The previous list had been overtaken: items 4, 5 and 9 were
already delivered while still being listed as pending, which is how a roadmap stops
being read.

**Delivered since that list was written:**

- ~~Add WAL segmentation and rotation.~~ M3, shipped in `794fd3e` — `pkg/wal/segment.go`, `pkg/wal/retention.go`, [`LOG-ROTATION.md`](LOG-ROTATION.md), [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md).
- ~~Add crash tests at every acknowledgement boundary.~~ `pkg/wal/boundary_test.go:54`, `:152` — every boundary of a 400-command tape, book digest and trade tape both.
- ~~Profile the current hot path before changing data structures.~~ Done and acted on; `CHANGELOG.md` v0.13.0 and the cached-clock comment at `pkg/matching/engine.go:586-592`.
- ~~Add durable versus non-durable response states.~~ *Half* delivered: both commit **modes** ship and are tested (`cmd/obgw/synclog.go`), but no *response* carries the state, so this stays on the list below as reconciliation metadata.

**The next slice, in order:**

1. ✅ **Close the journal, and close the class.** `Runner.SetPhase` was a frozen public mutating command that runs an auction and was absent from `logCommand` — the third such escape after `Reduce` and `Halt`, each found by accident after shipping. The journal is now exhaustive *by construction*: every `cmdKind` is classified journalled-or-read-only, a read-only classification must leave `EngineSnapshot.Digest()` untouched, and every `wal.EntryKind` must have a replay arm. Spec and outcome: [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) (§8 for what it found, including two holes in the guard the spec itself specified). **Done.**
2. **Export the sequence trio.** WAL written, last applied, last synced, plus commit mode and replica lag, as gauges from `cmd/obgw` and `examples/replication`. M2 and M4 both block on it, and [`REPLICATION.md`](REPLICATION.md) §4 already promises a graph that does not exist.
3. ✅ **Build the reference matcher and the random-tape differential harness** (M9), including the four missing per-command invariants. `internal/refmatch` is an independent model, `internal/tape` is the one generator, and `TestDifferentialTape` compares a whole observation after every command of 2,240 generated commands. All four missing invariants are now asserted on the generated path — the fourth, snapshot-restore-equals-uninterrupted, needed two assertions rather than one, and adversarial review is what established that the version that shipped first was a digest round-trip blind to the state `LoadSnapshot` rebuilds. Twenty-one deliberate engine mutations are all caught, shrinking to 1-4 commands each; three engine defects were found doing it, and a fourth property (iceberg refill priority) turned out to be claimed rather than tested and now has a test. This unblocks M11 exactly as intended — the harness stays green across the pooling and price-container changes M11 will make, and red on every semantic one. Spec and outcome: [`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) (§10 for what it found, including eleven places the spec was wrong about its own design, six of them found by review after the first implementation; §2.2 for the half of the independence rule that is *not* achieved). **Done for the continuous session; DAY/GTD, the exotics and the auction are tier 2, enumerated in the `commandTier` table.**
4. **Add an independent reconciliation consumer** (M7), which needs a durable execution-report journal first — today the outbound stream is an in-memory ring.
5. **Extract a seeded, serialisable command tape and a portable output digest** (M10), then the stage-attribution histograms. Only after that is the cross-language comparison reproducible.
6. **Write the ADR set** (M0), seeded from [`SPEC.md`](SPEC.md) §6 plus the decisions already argued in [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md), [`LOG-ROTATION.md`](LOG-ROTATION.md), [`REPLICATION.md`](REPLICATION.md) and [`TRADE-BUST.md`](TRADE-BUST.md). Parallel documentation work; it blocks nothing and is blocked by nothing.

**Deliberately later.** M1's sequenced command record is a wire-format and
ingress-architecture change touching `internal/wire`, `wal.Entry` and every gateway
path. It should not be attempted while the durability contract it would stamp into each
record is still unnamed — item 2 first.

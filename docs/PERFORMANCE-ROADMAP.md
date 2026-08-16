# Performance and production roadmap

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

The current benchmark numbers are primarily core or in-process measurements. The durable path, network path, connection scale, multi-day behavior, and production hardware envelope still need to be measured as separate systems.

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

The matcher should consume an ordered tape. It should not infer market priority from goroutine scheduling.

### Acceptance criteria

- The same sequenced tape produces byte-identical state and events.
- Concurrent ingress produces an auditable total order.
- Cancel/new races have deterministic results.
- Duplicate commands cannot produce duplicate executions.
- Replay uses the same sequencing record as live execution.

---

## M2 — Make durability and acknowledgement explicit

The current WAL can append before apply while group sync happens later. That is a valid policy only if the client contract says so.

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
process-accepted
primary-durable
replica-durable
quorum-committed
```

Expose enough metadata for reconciliation:

- venue sequence;
- engine sequence;
- WAL sequence;
- commit state;
- incarnation.

### Crash tests

Test a process failure at each boundary:

- before WAL append;
- after append, before apply;
- after apply, before response;
- after response, before sync;
- after sync, before response;
- after primary sync, before replication;
- after follower apply, before follower sync.

### Acceptance criteria

- A client can tell whether an acknowledgement survived a primary crash.
- The loss window is measurable and documented.
- Recovery never silently converts an acknowledged durable command into a missing command.
- Commit state is visible in logs, metrics, and reports.

---

## M3 — Make WAL and snapshots operationally bounded

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

The existing replication seams are a useful foundation, but asynchronous replication and an incarnation fence do not prevent an isolated old primary from continuing to trade.

### Design decisions

Choose an external or integrated fencing mechanism:

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

- primary crash;
- network partition;
- follower lag;
- follower restart;
- old-primary reconnect;
- promotion during active flow;
- promotion with a known gap;
- duplicate client reconnect;
- stale incarnation;
- partial replication.

---

## M5 — Establish a supported client protocol

The current wire package is intentionally internal. Before external production adoption, define and test the protocol as a product surface.

### Deliverables

- public protocol specification;
- version compatibility matrix;
- reference client;
- reconnect state machine;
- replay and resubmission rules;
- conformance test suite;
- golden vectors;
- client certification harness;
- explicit durable and committed response types;
- protocol fuzzing;
- message-size and rate limits.

### Acceptance criteria

- A client can reconnect without ambiguity.
- Duplicate requests are safe.
- A client can reconcile every execution.
- Protocol versions are tested against supported servers.
- Old clients fail clearly rather than being silently misread.

---

## M6 — Complete security lifecycle

Authentication is only one part of production security.

### Deliverables

- external identity-provider seam;
- secret-manager integration;
- credential rotation;
- immediate revocation;
- expiration;
- session invalidation;
- account scopes;
- operator roles;
- market-data permissions;
- administrative permissions;
- brute-force protection;
- per-account and per-session DoS controls;
- security audit events;
- penetration testing.

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

The market-data design should preserve the matcher’s isolation while making feed correctness independently verifiable.

### Deliverables

- feed commit point;
- subscriber lag metrics;
- gap metrics;
- resnapshot limits;
- drop-copy feed;
- trade-feed reconciliation;
- per-subscriber state;
- retention policy;
- explicit handling for bust events;
- distinction between conflatable book updates and non-conflatable execution reports.

A slow market-data subscriber may be disconnected or resnapshotted. Critical execution reporting must have stronger guarantees.

---

## M9 — Add model-based matching tests

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

- no negative quantity;
- no duplicate order ID;
- no order in two states;
- aggregate depth equals resting orders;
- trade quantities balance;
- sequence is monotonic;
- event stream reconstructs state;
- replay equals live;
- snapshot restore equals uninterrupted execution.

---

## M10 — Build the performance laboratory

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

Only do this after profiling the current implementation.

### Experiments

Benchmark:

1. Current map plus sorted price levels.
2. Dense tick array plus bitmap for bounded grids.
3. Radix or adaptive radix tree for sparse integer prices.
4. Index-based order storage instead of pointer-heavy structures.
5. Separate hot and cold order fields.
6. Fixed-size event records instead of interface-heavy hot-path callbacks.
7. Specialized paths for common limit/cancel operations.

Evaluate:

- cache misses;
- branch misses;
- pointer chasing;
- allocation behavior;
- book-depth scaling;
- price-range scaling;
- cancel cost;
- level creation cost;
- memory footprint;
- tail latency.

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

Run:

- 24-hour soak;
- multi-day soak;
- hundreds or thousands of connections;
- reconnect storms;
- slow readers;
- slow followers;
- WAL rotation under load;
- disk-full conditions;
- snapshot under load;
- multiple symbols;
- full order-type mix;
- process and host restarts;
- network partitions;
- clock changes.

Verify:

- no orphan orders;
- no duplicate fills;
- no lost cancels;
- no undetected sequence gaps;
- no reconciliation divergence;
- no memory growth;
- no descriptor growth;
- no goroutine growth;
- correct degraded-state transitions;
- bounded recovery.

The output must be a capacity envelope, not a single throughput claim.

---

## M14 — Complete observability and incident operations

Add metrics for:

- ingress rate;
- accepted and rejected rate;
- reject reason;
- queue occupancy;
- queue-full events;
- apply latency;
- WAL append latency;
- WAL sync latency;
- commit lag;
- replica lag;
- feed lag;
- publisher drops;
- snapshot age;
- snapshot duration;
- recovery duration;
- disk usage;
- WAL growth;
- GC pauses;
- clock offset;
- operation tail latency.

Add:

- structured logs;
- trace IDs;
- order/session/client correlation;
- alert rules;
- SLO dashboards;
- incident annotations;
- audit-log retention;
- runbook links in alerts.

Define degraded behavior for WAL failure, disk pressure, matcher stalls, replica lag, publisher overload, snapshot failure, authentication backend failure, clock skew, feed lag, and stale-primary reconnection.

---

## M15 — Integrate venue functions outside the matcher

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

The next implementation slice should be:

1. Freeze the command and acknowledgement state model.
2. Add sequenced command records.
3. Add durable versus non-durable response states.
4. Add WAL segmentation and rotation.
5. Add crash tests at every acknowledgement boundary.
6. Add committed, applied, and replica sequence metrics.
7. Add an independent reconciliation consumer.
8. Build the shared Go/C++/Rust benchmark harness.
9. Profile the current hot path before changing data structures.
10. Publish the first fair baseline, including cases where Go loses.

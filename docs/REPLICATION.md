# Replication — Proving the HA Seams

Status: **implemented** — `examples/replication` + drills D1–D7 in CI; written as a
spec before any code existed, and §8 records what building it actually found ·
Author: Karthikeyan NG · Last updated: 2026-08-03

Companion documents:
- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) — where "high availability:
  seams only" is claimed, which is the claim this spec exists to test.
- [`EXCHANGE-ARCHITECTURE.md`](EXCHANGE-ARCHITECTURE.md) — why consensus is
  deliberately not bundled, including the venues that lost quorum getting it wrong.
- [`RUNBOOKS.md`](RUNBOOKS.md) — names "no failover procedure" as a gap; this spec's
  drill is that procedure.

---

## 1. Why this exists

[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) says: *"deterministic apply, an
ordered command log, replay mode and snapshot bootstrap are all here, which is what a
primary-backup topology needs."*

That sentence is a claim about seams, and this project's record with seams claimed
but never consumed is documented and bad: four of them, each perfectly plausible on
the page, turned out not to exist when something finally tried to build on them. The
HA seams are the largest remaining claim of that kind. Nothing in this repository
tails a live log, bootstraps a second engine from another's snapshot while commands
keep flowing, or promotes a follower and asks whether the books agree.

So the next milestone is not "add HA". It is **build the one consumer that would
notice if the sentence were wrong** — a reference primary-backup example, and a
drill that fails the build when the failover story regresses. Either outcome is a
result: the seams hold, or we find the fifth phantom before a user does.

## 2. What exists today (the four seams, by symbol)

Verified against the code at the time of writing, not quoted from other docs:

1. **Deterministic apply.** One goroutine owns the engine
   (`matching.Runner`, `pkg/matching/engine_loop.go`); identical command sequences
   produce identical books. This is the property the recovery tests already lean on
   (`TestCrashRecoveryMatchesUninterrupted`, `pkg/matching/runner_recovery_test.go`).
2. **Ordered command log.** `matching.CommandLog`
   (`pkg/matching/engine_loop.go:87`) receives every mutating command before it is
   applied; `wal.Writer` satisfies it with per-record CRC-32C and a monotonic
   sequence (`Writer.Seq`, `pkg/wal/wal.go`).
3. **Replay mode.** `RunnerConfig.Replaying` / `Runner.SetReplaying`
   (`pkg/matching/engine_loop.go:111,591`) — replay applies commands without
   re-appending them, and replay admission keeps rejecting what was rejected live
   (`pkg/matching/replay_admission_test.go`).
4. **Snapshot bootstrap.** `wal.WriteSnapshot` / `wal.ReadSnapshot` /
   `wal.Recover(config, snapPath, walPath)` (`pkg/wal/wal.go`) — CRC-checked
   snapshot plus log tail, already exercised by `cmd/obgw` checkpointing. `Recover`
   reads and verifies the whole file and decodes only the tail past the snapshot's
   sequence; a follower bootstrapping from a large primary log therefore pays the
   read but not the parse ([BOUNDED-RECOVERY.md](BOUNDED-RECOVERY.md)). The reference
   primary's own catch-up read (`examples/replication/primary.go`) does **not** get
   that: it calls `wal.ReadAll` and filters `Seq <= h.Have` itself, so a reconnecting
   follower still costs the primary a full parse of its log. Serving it means
   exporting the bounded reader, which means designing its contract for callers
   outside `pkg/wal`, and that is not done.

And the one seam this spec found missing on paper, since closed:

5. **A canonical state digest.** Divergence detection requires primary and follower
   to compare books cheaply. When this spec was first written the digest lived in
   test helpers only (`snapDigest`, `pkg/wal/runner_recovery_test.go`) — that was
   deliverable #1, and it has shipped: `EngineSnapshot.Digest`
   (`pkg/matching/snapshot.go`) fingerprints everything recovery must reproduce,
   normalises exactly what a legitimate replay may differ in, and the recovery
   suite now consumes it, so a regression in the digest fails crash-recovery tests.

## 3. The reference topology

One primary, one warm follower, asynchronous log shipping. Deliberately the
smallest topology that can be wrong in the interesting ways.

```
                    clients
                       │
                 ┌─────▼─────┐  fsync, then ack        ┌───────────┐
                 │  primary   │ ──── log records ─────▶ │ follower  │
                 │  (obgw)    │       (async)           │ (obha)    │
                 └───────────┘                          └───────────┘
                  WAL + snap                     snapshot bootstrap + live tail
```

- **The follower is a Runner in replay mode that never stops replaying.** Bootstrap
  is exactly today's recovery path — `wal.Recover`, then `NewRunnerFor` with
  `Replaying: true` — except the tail never ends: new records keep arriving and
  keep being applied. Recovery and replication become one code path, which is the
  point; a replication bug is then a recovery bug, and recovery has tests.
- **Transport is out of scope for the library.** The reference example ships
  records over a single TCP stream with the record framing the WAL already defines.
  No fan-out, no catch-up protocol beyond "start from sequence N", where N comes
  from the snapshot the follower bootstrapped from.

## 4. What "committed" means here — stated, not implied

The reference picks: **a command is committed when the primary's WAL has it
(group-commit fsync), and replication is asynchronous.** An acknowledged order can
be lost if the primary's disk dies before the record ships. The recovery point is
therefore non-zero and *measured* — the follower exports its applied sequence, the
primary its written sequence, and the gap is the loss window, on a graph, at all
times.

Synchronous replication (ack after the follower has the record) is deliberately not
the reference choice: it is where quorum, timeout and split-brain decisions start,
and [EXCHANGE-ARCHITECTURE.md](EXCHANGE-ARCHITECTURE.md) already argues why the
library must not make those for you. The example's job is to prove the seams
compose — not to smuggle in a consensus protocol through the back door of a demo.

## 5. Failover, and what fences the old primary

Failover in the reference is **manual** — an operator decision, executed by a
procedure, verified by a drill. Automatic failover is a liveness-vs-safety decision
the embedder must own.

The procedure (the runbook entry this spec commits to writing):

1. Confirm the primary is actually gone, and record the follower's applied
   sequence.
2. Stop the tail. The follower finishes applying what it holds, leaves replay mode
   (`SetReplaying(false)`), and opens its own WAL — whose first record notes the
   sequence it resumed from.
3. **Mint a new incarnation.** This is the fence, and the machinery already exists:
   `orderentry.Registry` sequence numbers are scoped to one incarnation, and a
   client resuming with a stale cursor is refused rather than served different
   content under numbers it believes it already has
   (`pkg/orderentry/stream.go:151`). Promotion reuses that property for exactly the
   failure it was built to make visible: every client must re-login and resume; a
   client still talking to the old primary is talking to a venue whose incarnation
   no longer exists.
4. Point clients at the follower (DNS, VIP, config push — the embedder's tooling,
   not ours).

What this does **not** fence: the old primary itself, if it was partitioned rather
than dead, still believes it is a venue and will keep matching for any client that
can still reach it. Split-brain prevention — leases, STONITH, a quorum — is
consensus by another name and stays out of scope. The reference's honest posture:
the drill demonstrates *detection* (two live incarnations are loudly
distinguishable), and prevention is listed in §8 as the embedder's first job.

## 6. The drill (pass criteria, CI-executable)

Following `cmd/obgw/drills_test.go` — each criterion verified to fail against
deliberately broken code before it counts:

- **D1 — The books agree.** Drive N commands through a primary while a follower
  tails. Digest of follower's book == digest of an uninterrupted single engine fed
  the same tape. (This is `TestCrashRecoveryMatchesUninterrupted` with the crash
  replaced by a wire.)
- **D2 — Mid-stream bootstrap.** Start the follower from a snapshot taken *while*
  the primary keeps trading; digest equality must still hold. This is the case
  today's tests do not cover — recovery always replays into a quiet engine — and
  the most likely home of the fifth phantom.
- **D3 — Promotion preserves what was acknowledged.** Kill the primary mid-flow,
  promote, replay the client's view: every order the primary acknowledged before
  the recorded loss window is in the promoted book; nothing after the follower's
  applied sequence is.
- **D4 — The fence holds.** A client resuming against the promoted venue with the
  old incarnation's cursor is refused. A drill that skips this rewrites §5 into
  fiction.
- **D5 — Replay admission still rejects.** A command the primary rejected does not
  become a fill on the follower (extends `replay_admission_test.go` across the
  wire).
- **D6 — Lag is visible.** Stall the follower; the exported sequence gap moves and
  an operator can alert on it. A loss window nobody can see is a loss window that
  does not exist until it happens.

  This drill was flaky at roughly one run in twelve for three releases, and the
  cause is worth more than the fix. A follower that actually applies commands is
  **slower than a wedged socket**, which merely fills a kernel buffer and costs the
  primary nothing until it is full — so driven flat out, the healthy follower's own
  ship buffer overflowed first and *it* was the one shed. `Shed()` is a bare
  counter, so the drill saw a non-zero value, assumed it meant the wedge, and
  reported "shedding the wedge broke the healthy follower": the opposite of what had
  happened. `Primary.ShedPeers` now attributes each cut to an address, the drill
  waits for the wedge specifically and asserts nobody else was cut, and the tape is
  paced against the healthy follower so there is one candidate rather than two.
  A drop counter that cannot name the dropped is the same gap for an operator —
  a client that stopped reading and a follower merely running behind produce the
  same increment and need opposite responses.
- **D7 — A bust replicates.** The primary annuls a print mid-stream and the
  follower ends up agreeing. Added with trade bust ([TRADE-BUST.md](TRADE-BUST.md))
  and it tests the digest rather than the wire: a bust changes no order, so a
  follower that dropped the record would have a byte-identical *book* and would
  disagree about what settled, forever and silently. What catches it is the bust
  registry being inside the digest.

## 7. Deliverables

1. ✅ `matching`: a public, documented **state digest** (§2.5) —
   `EngineSnapshot.Digest`.
2. ✅ `examples/replication`: primary-side log shipping (`wal.SetOnAppend` → a
   bounded per-follower buffer), follower tail, promotion — three files, read
   in one sitting.
3. ✅ The **failover runbook entry** in [RUNBOOKS.md](RUNBOOKS.md) — the §5
   procedure, replacing the "no failover procedure" gap line.
4. ✅ **Drills D1–D7** in CI (`examples/replication/main_test.go`), each
   verified to fail against deliberately broken code.
5. ✅ The PRODUCTION-READINESS edit: "High availability" is *seams proven —
   topology still yours*, and not one word further.

## 8. What building it found — prediction versus result

§1 argued the seams were the largest unconsumed claim and predicted the fifth
phantom, and §6 even named D2 (mid-stream bootstrap) as its most likely home. The
prediction was right in kind and wrong in place, which is worth recording
precisely because that is how these findings usually go:

- **The four seams held.** Deterministic apply, the ordered log, replay mode and
  snapshot bootstrap composed exactly as documented. Mid-stream bootstrap — the
  case no recovery test covered — worked on the first run and is now pinned by D2.
- **The fifth phantom was in the NEW seam, not the old ones.** The log-tail hook's
  first shape handed subscribers the `wal.Entry` — which holds a pointer to the
  order the engine keeps mutating as it fills. Every drill passed; the first
  `-race` run did not. The recovery tests could never have seen it, because
  recovery replays after the engine is done. The fix (hand the record's payload
  bytes, already produced to write the file) made the wire byte-identical to the
  log and the API impossible to misuse in that particular way.

One drill exists that the spec did not ask for: promotion refuses a book with a
known defect (a detected gap), because serving a book known to be wrong is
strictly worse than serving nothing.

## 9. Non-goals, so nobody discovers them as surprises

- **No consensus, no quorum, no automatic failover.** The embedder's first job if
  they need split-brain *prevention* rather than detection.
- **No synchronous replication mode.** See §4.
- **No multi-follower fan-out or catch-up service.** One follower proves the
  seams; a fleet is plumbing.
- **No cross-symbol coordination.** `matching.Shards` engines replicate
  independently; a venue-wide commit point is a venue decision.
- **No claim of zero data loss.** The loss window is measured and stated, which is
  more than most systems that claim zero actually deliver.

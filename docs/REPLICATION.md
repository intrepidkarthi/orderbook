# Replication — Proving the HA Seams

Status: **SPEC — no code exists.** This document precedes the implementation on
purpose · Author: Karthikeyan NG · Last updated: 2026-08-02

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
   snapshot plus log tail, already exercised by `cmd/obgw` checkpointing.

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

## 7. Deliverables

1. ✅ `matching`: a public, documented **state digest** (§2.5) —
   `EngineSnapshot.Digest`.
2. `examples/replication` (or `cmd/obha`): primary-side log shipping, follower
   tail, promotion — small enough to read in one sitting, like
   `examples/gateway`.
3. A **failover runbook entry** in [RUNBOOKS.md](RUNBOOKS.md), replacing the "no
   failover procedure" line with the §5 procedure.
4. **Drills D1–D6** in CI.
5. A PRODUCTION-READINESS edit moving "High availability" from *seams only* to
   *seams proven — topology still yours*, and not one word further.

## 8. Non-goals, so nobody discovers them as surprises

- **No consensus, no quorum, no automatic failover.** The embedder's first job if
  they need split-brain *prevention* rather than detection.
- **No synchronous replication mode.** See §4.
- **No multi-follower fan-out or catch-up service.** One follower proves the
  seams; a fleet is plumbing.
- **No cross-symbol coordination.** `matching.Shards` engines replicate
  independently; a venue-wide commit point is a venue decision.
- **No claim of zero data loss.** The loss window is measured and stated, which is
  more than most systems that claim zero actually deliver.

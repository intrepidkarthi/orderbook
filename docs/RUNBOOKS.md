# Runbooks

Procedures for the failures this venue can actually have, written from the code that
produces them. Each names the signal you will see first, what the code has already
done by the time you look, what to do, and what will make it worse.

**Every entry here is drilled in CI** — see `cmd/obgw/drills_test.go`. Each drill
induces the real failure rather than a simulation of its symptoms, asserts the signal
this page tells you to look for, and where a procedure is prescribed, runs it. The
corrupt-snapshot drill really deletes the file and restarts, and checks the replayed
book is identical and its orders still nameable.

That is not the same as being ready. A drill proves the page is not *stale* — that no
reason string was renamed out from under it, no status code changed, no fallback quietly
stopped falling back. It proves nothing about whether a human can follow it at three in
the morning on a venue that is losing money. **Nobody has done that**, which is why
[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) still counts operational readiness as
weak, and will until somebody has.

Each drill was itself verified to fail against deliberately broken code, because a drill
that cannot fail is decoration.

Two rules that apply to every entry below:

- **The book is the asset.** Almost every wrong action here destroys or diverges it.
  When in doubt, stop the venue rather than let it trade on state you cannot vouch for
  — a halted market is a bad day, a market trading against a wrong book is a bad year.
- **Never edit the log or the snapshot by hand, and never rename, move or delete a
  segment.** Both are checksummed. A file you have repaired is a file whose checksum
  you have recomputed over bytes you invented. The log is a SET — a stem plus
  `<stem>.<16 digits>` siblings, the digits being the first sequence that file holds —
  and each name is cross-checked against a header inside the file, so a renamed
  segment refuses to start the venue and a deleted one refuses just as loudly. That is
  the design working. Copying a segment INTO the set from an archive is the one
  directory operation that is safe, because an archived segment is byte-identical to
  the segment that was deleted.

---

## Alert thresholds

The numbers to watch, from `/metrics` on the admin listener.

| Signal | Threshold | Why |
|---|---|---|
| `orderbook_last_event_sequence` | not advancing for 2 s while `orderbook_queue_depth > 0` | The only way a stalled matcher is distinguishable from a quiet market: every rate metric reads zero for both |
| `orderbook_queue_depth / orderbook_queue_capacity` | > 0.75 | Past this the venue is behind and the next thing that happens is clients being refused |
| `obgw_publisher_dropped_total` | **any increase** | Each dropped batch is a lost execution report. Not a delay — a loss |
| `orderbook_rejections_total{reason="..."}` | any sustained rate on a reason that was previously zero | "Rejections are up" is not actionable; the reason is |
| `orderbook_resting_orders` | approaching the engine's `MaxOrders` | At the ceiling the venue refuses all new liquidity while looking healthy |
| `obgw_goroutines` | not tracking `obgw_connections` × 4 + a fixed set | A leaked session |
| `obgw_open_files` | rising with flat connections | Leaked descriptors end the day outright: accept fails, and everyone already connected stays perfectly healthy |
| `orderbook_phase` | ≠ 1 unexpectedly | The venue halted or went cancel-only without an operator doing it |

`/readyz` returns 503 on the first two. `/healthz` deliberately does not — see
[§ A stuck matching goroutine](#a-stuck-matching-goroutine).

---

## A torn log

**Signal.** The venue restarts after a crash and logs `recovered N resting orders`, with
N lower than you expected.

**What the code has done.** A crash mid-write leaves a short final record. The reader
stops at it cleanly and reports the complete records before it; no checksum is
involved, because there is nothing complete to check. This is normal and expected —
it is the ordinary shape of every unclean shutdown.

**What to do.** Confirm N against the last checkpoint and the market-data feed's last
published sequence. The orders in the torn record were never acknowledged to any
client, because the log is written *before* the command is applied — so a client whose
order is missing never received an ack for it and its own retry logic is correct to
resend.

**What makes it worse.** Truncating or "repairing" the file. The reader already stops
in the right place.

---

## A corrupt log record

**Signal.** The venue refuses to start: `wal: corrupt record`.

**What the code has done.** A *complete* record whose CRC-32C disagrees with its bytes
means the bytes changed after they were written — media, not a crash. Recovery refuses
to start the venue rather than truncating, because truncating silently discards every
record after the bad one and produces a book that is plausible and wrong.

The message names three things: the **segment**, the record's **ordinal within that
segment**, and the **sequence** the two imply —
`wal: corrupt record: BTC-USD.wal.0000000000610422 record 37 (sequence 610458)
checksum 1fe167eb, want d1517f35`. The ordinal counts from that segment's first
record, including the ones a snapshot already covers; it is not a count of records
recovery kept. Use the SEQUENCE for step 3: across a set of segments a bare per-file
ordinal is ambiguous, and the sequence is not. This matters because recovery no longer
decodes the covered prefix — it still reads and checksums every retained byte, so a
record damaged long behind the snapshot is still caught and still refuses to start.

One narrower case does not refuse, and it is worth knowing before you go looking for
it. A record whose bytes are intact and whose *content* is not a valid record — a
writer bug or a format mismatch, never media damage, since the checksum matches — is
only seen where recovery decodes the record. Behind the snapshot's boundary it is not
decoded, so the venue starts, on exactly the book it would have recovered anyway. If
you suspect that and want to know, `wal.ReadAll` decodes every record in the file and
still reports it, by the same file ordinal. See
[BOUNDED-RECOVERY.md](BOUNDED-RECOVERY.md) §5.2.

**What to do.**

1. Do not start this node again against this file.
2. Treat the storage as suspect: check SMART data, the filesystem, and whether other
   files on the same volume are affected.
3. Recover from the last good snapshot plus the log records *before* the corrupt one,
   on different storage. Accept that everything after the corruption point is lost and
   reconcile it from the market-data feed and participants' own records — the sequence
   numbers make the gap precise.
4. If a backup node has the same command stream, prefer its state over any
   reconstruction.

**What makes it worse.** Deleting the record and restarting. The venue will come up and
you will not know what it is missing. **Renaming, moving or deleting a segment file is
the same mistake with a bigger blast radius**: a segment's name declares which
sequences it holds, the venue cross-checks that name against the header inside it, and
a set with a hole in it is refused rather than recovered. Do not tidy that directory.

**One message that looks like media damage and is not.** `wal: corrupt record: ...
record 1 declares 1329747777 bytes (limit 8388608)` from a build older than v0.21.0
means a DOWNGRADE, not a disk. Those four bytes are the ASCII `OBWA` of a segment-set
marker being read as a length prefix by a build that predates the format. Check the
binary's version before you check SMART data.

---

## A gap between the snapshot and the log

**Signal.** The venue refuses to start:

```
wal: log gap: snapshot covers through sequence 412000 but the oldest retained
segment BTC-USD.wal.0000000000610422 starts at 610422 — sequences 412001..610421
are in no file this venue can read.
```

**What the code has done.** It has refused to recover a book that would be missing
those commands. The remaining records all verify perfectly; nothing downstream could
have told you they were incomplete. This check exists so that "retention deleted
something it should not have" is an outage with two numbers in it rather than a
plausible, wrong book.

**What to do.**

1. **Read the first number in the message.** If it is `0`, stop here — this is not a
   retention fault and steps 2 to 5 are the wrong procedure. `0` means the snapshot
   carries no log position at all, which is a different thing from a snapshot that is
   too old, and the book in it is very likely complete. See "A snapshot stamped
   sequence 0" below.
2. `ls` the segment set. The oldest segment's name is the retention floor, in decimal,
   with no tooling required. That number is most of the diagnosis.
3. If the archive directory has the missing segments, copy them back into the set —
   they are byte-identical to what was deleted — and restart.
4. If the snapshot is older than the floor because a NEWER snapshot exists and was not
   used, point `-snapshot` at it. A snapshot whose `WALSeq` is at or past `floor - 1`
   joins cleanly.
5. If neither, this node cannot reconstruct the book. Fail over to a node that has the
   state, or restore both the snapshot and the segments from a backup taken together.

**What makes it worse.** Deleting the remaining segments so the venue starts on the
snapshot alone. It will start, and it will be missing every command after the
snapshot's sequence, and the venue will not say so.

---

## A snapshot stamped sequence 0

**Signal.** The gap message above, with `0` as the sequence the snapshot covers:

```
wal: log gap: snapshot covers through sequence 0 but the oldest retained
segment BTC-USD.wal.0000000000000385 starts at 385 — sequences 1..384 are in
no file this venue can read.
```

**What the code has done.** Exactly what it says, and the refusal is arithmetically
correct — but the premise is wrong. A `WALSeq` of `0` means "this snapshot covers
nothing, replay the log from the beginning", and the beginning has been retained away.
The snapshot itself is almost certainly a complete, verified book.

**Which builds produce it.** A venue restarted, and then checkpointed before it applied
a single command — a restart into a quiet market, out of hours, before the open, or a
maintenance window with the flow pointed elsewhere. The Runner was handed a recovered
engine and not the position that engine stood at, so the first checkpoint stamped `0`
over a good snapshot. Fixed: `matching.RunnerConfig.LastApplied` now carries the
position across, and `cmd/obgw` seeds it from the recovery report. A snapshot written
by an earlier build can still be sitting on disk.

**What to do.**

1. Confirm the shape: the snapshot is recent and the number is `0`, not merely low.
   `0` is not a sequence any healthy running venue checkpoints at after its first
   command.
2. Upgrade to a build that carries the position, so the next checkpoint stamps the
   snapshot correctly and the condition cannot recur.
3. To start now: restore the archived segments from the floor down to 1 into the set,
   **and move the snapshot aside**, so the venue replays the whole log into a fresh
   engine. Expect a slow start proportional to the whole restored log.

   **Do not restore the segments and leave the snapshot in place.** Replaying a log
   from sequence 1 onto a snapshot that already contains it double-books every order
   in it, and the duplicate-client-order-id guard does not stop that. The guard is a
   bounded ring — 4,096 keys in `cmd/obgw` — so on any log longer than the ring, each
   key is evicted before the replay reaches it again: 4,097 orders recover as 8,194,
   and 20,000 recover as 40,000. It is not a partial failure at the margin, it is the
   whole book. Orders carrying no client order id are never deduped at all and double
   at any size. This section only applies once retention has fired, which needs about
   3 million records at the shipped defaults, so the log is always orders of magnitude
   past the ring. Dropping the snapshot costs a slow start; keeping it costs a book
   that is wrong in a way nothing downstream detects.
4. If the segments below the floor are gone and unarchived — the default, since
   `-wal-archive` is off unless set — the honest options are a fail-over to a node with
   the state, or accepting the snapshot's book and the loss of everything after it.
   Neither is good, which is the argument for setting `-wal-archive` alongside
   `-wal-retain`.

**What makes it worse.** Hand-editing the snapshot's `WALSeq`. It is inside the
checksum, so the file stops verifying and recovery refuses it outright — turning a
recoverable venue into "A corrupt snapshot".

---

## A corrupt snapshot

**Signal.** The venue refuses to start: `wal: corrupt record: snapshot <path> checksum
...`.

**What the code has done.** Snapshots are written atomically — a fully-synced temp file
renamed into place — so a crash cannot tear one. A checksum failure therefore means
media corruption, and it is refused for the same reason a corrupt log record is, only
more so: the snapshot is the base every log record after it is applied to, so starting
from a wrong one produces a book that nothing downstream can detect as wrong.

**What to do. Read the retention floor first — the procedure depends on it.**

```
ls -1 /var/lib/obgw/BTC-USD.wal.*      # the oldest name's 16 digits are the floor
```

1. **If the floor is 1** — a venue whose retention has never fired, which is every
   venue running the default configuration — the old procedure is still exactly right.
   Delete the snapshot and restart. Recovery replays the log from the beginning, which
   is slower — a 100,000-order replay takes roughly 174 ms, and a log that has run
   since the last checkpoint takes proportionally longer — but it is exact.
2. **If the floor is above 1**, DO NOT delete the snapshot. Retention has deleted the
   log below that sequence, so the snapshot is the only base the retained log can be
   joined to, and deleting it destroys the book. Restore the archived snapshot and the
   archived segments from the floor onward, or fail over to a node that has the state.
3. If neither exists, the honest answer is that this venue's recovery point is its
   last good snapshot and there is not one. That is a statement about the backup
   policy, not a procedure — a venue running `-wal-retain` without `-wal-archive` has
   a recovery point objective equal to its newest snapshot, and this is what that
   costs.

Then investigate the storage as above.

**What makes it worse.** Keeping the snapshot because replay is slow, on a venue whose
floor is 1 — replay time is the cheapest thing you will spend today. Or deleting it on
a venue whose floor is not, which is the most expensive.

> Snapshots written by a build from before the checksum existed (v0.16.0 and earlier)
> are read without one, so an upgraded venue cannot verify its existing snapshot until
> the next checkpoint rewrites it. If you want that guarantee immediately, force a
> checkpoint after upgrading.

---

## The disk filled up

**Signal.** `/readyz` returns 503 with `not ready: the write-ahead log cannot be
written`, and the log holds one line: `WAL SYNC FAILED — halting the book and failing
readiness`. Earlier, and more usefully: `disk low` and then `DISK NEARLY FULL ... Going
CANCEL-ONLY`, with `orderbook_wal_disk_free_bytes` falling and `orderbook_phase`
reading 2.

**What the code has done.** Three things, at three thresholds.

- Below `-wal-min-free` (2 GiB by default) it warned and ran retention immediately
  instead of waiting for the next checkpoint. If `-wal-retain` is unset, retention
  deletes nothing, and the warning says so rather than claiming to be reclaiming space.
- Below `-wal-min-free-stop` (256 MiB) it put every book into **cancel-only**: new
  orders are refused with `ReasonHalted`, cancels and reduces are accepted so
  participants can get flat, resting orders are untouched, market data is unaffected.
  Retention runs on every tick below this mark too.
- When a sync actually failed it **halted the book, latched the failure, and failed
  readiness**.

**Neither latch clears by itself, and cancel-only is a latch too.** This is the part
that surprises people. The sync failure clears on a restart, which is where you get to
decide whether the disk is actually fixed — that has always been written down. So does
**cancel-only**: `diskStopped` is set once and nothing anywhere clears it, there is no
admin endpoint that resumes trading, and free space returning above
`-wal-min-free-stop` does not bring the venue back. A book that dipped below the mark
for a single checkpoint tick — a log rotation on a shared volume, another process
briefly filling the disk — keeps refusing new orders with `ReasonHalted` until the
process is restarted, even though no sync ever failed.

That is deliberate: a venue oscillating in and out of cancel-only around a threshold is
worse for participants than one that stays out until a human has looked at it. But it
means **freeing space is not the end of the procedure** — the restart is.

**What to do.**

1. Take the node out of rotation if the orchestrator has not already — `/readyz` is
   telling it to.
2. Free space. `orderbook_wal_bytes` says how much of it is this venue's log.
3. If the log is the problem and `-wal-retain` is unset, set it, and set
   `-wal-archive` with it. Do not delete segments by hand: the venue refuses to start
   on a set with a hole, and it is right to.
4. Restart. Only a restart clears either latch — the halted book after a failed sync,
   and cancel-only after the stop-water mark. A venue that is merely cancel-only can be
   restarted at leisure, but it will not resume on its own.

**What makes it worse.** Restarting the process without freeing space, repeatedly. Each
attempt appends more log. Also: deleting the oldest segments by hand to buy room —
that is what `-wal-retain` does safely, gated on a snapshot that has been read back
and verified, and doing it by hand is doing it without the gate.

**The honest limit.** Commands acknowledged inside the 20 ms group-commit window that
ENDED in the failed sync were acknowledged before they were durable. That is the
existing durability window (`-sync-every-command` closes it); a full disk does not
widen it, it is just the moment it matters.

---

## A stuck matching goroutine

**Signal.** `/readyz` returns 503 with `stalled: N commands queued, event sequence stuck
at S for D`. Every rate metric reads zero.

**What the code has done.** Nothing. This is the one failure the engine cannot handle
itself: the single writer is not writing, so no command is applied and no event is
emitted.

**Why `/healthz` still returns 200, deliberately.** A failed liveness check means
"restart me", and an orchestrator that kills a venue holding a live book because a
probe was slow has turned a stall into an outage. Liveness says the process exists.
Readiness says it is keeping up. Only the second should pull a node out of rotation,
and only a human should decide to restart.

**What to do.**

1. Take a stack dump — `SIGQUIT` to the process, or `kill -QUIT` — *before* restarting.
   The goroutine parked in `Runner.loop` names what it is blocked on, and this is the
   only chance you will get to find out.
2. Check whether the log's storage is the answer. Every command is journalled before it
   is applied, so a write that never returns stops the matcher and looks exactly like
   this.
3. If the stack shows a resolver, a sink or a `CommandLog` implementation of your own,
   that is your bug and the stack names it. A caller-supplied resolver that *panics* is
   contained and reported; one that *blocks* is not, and cannot be.
4. Restart. Recovery is exact and the WAL is written write-ahead, so nothing
   acknowledged is lost.

**What makes it worse.** Restarting before the stack dump. The state that explains it
dies with the process.

---

## A mass cancel has paused the venue

**Signal.** A latency spike across all clients, correlated with
`orderbook_orders_canceled_total` jumping. Queue depth rises and drains.

**What the code has done.** Exactly what it should. A mass cancel is one command on the
single writer, and it holds the matching goroutine for the duration — roughly 872 µs
per 5,000 orders. Everything else waits behind it, which is what makes the venue's
ordering guarantee true.

**What to do.** Usually nothing; it is self-limiting. If a participant's mass cancels
are large enough to be felt venue-wide, that is a capacity and admission question, not
an incident: the per-account order cap (`MaxOrdersPerAccount`, off by default)
bounds the worst case, and it is the knob to reach for.

**What makes it worse.** Treating the latency spike as a stall and restarting. Check
`orderbook_last_event_sequence` first — during a mass cancel it is advancing, which is
precisely the distinction `/readyz` is built on.

---

## A subscriber has fallen off the retention ring

**Signal.** A market-data subscriber disconnects with `MDRejectEvicted`, or an
order-entry client's resume is refused with `ErrSequenceEvicted`.

**What the code has done.** Refused explicitly rather than quietly resynchronising. The
client asked a precise question — "everything after sequence N" — and the honest answer
is "that is gone". Serving a partial history that looks complete is how a client ends
up with a wrong position and no way to detect it.

**What to do.** The client resubscribes from zero and takes a fresh snapshot; for order
entry, it reconnects without a cursor and issues a `Query` to rebuild its open orders.
Both are normal client-side recovery, not operator actions.

If it is happening repeatedly, the ring is too small for how long your clients go away.
Raise `MDRetain` for market data or `StreamRing` for order entry. Both cost memory
proportional to retention, and that is the trade being made — an unbounded buffer turns
one client that never reconnects into a venue-wide memory leak.

**What makes it worse.** Raising retention to hide a client that is not reading. A slow
consumer is a client problem; the eviction is the venue protecting itself.

---

## The publisher is dropping batches

**Signal.** `obgw_publisher_dropped_total` increases at all.

**What the code has done.** Discarded its oldest queued batches to avoid blocking the
matching goroutine. That is the right call — blocking would stop the venue and growing
without limit would end it — but a dropped batch is a *lost* message, not a late one.

**Why this is severe.** Every consumer downstream of that stream is affected: execution
reports that will never be delivered, and clients whose positions are now wrong without
either side knowing.

**What to do.**

1. Treat it as a data-loss incident, not a performance one.
2. Identify the affected accounts from the sequence gaps in their streams — the gaps
   are what make this recoverable at all.
3. Reconcile those accounts against the engine's own state (`Query`, or
   `Runner.OpenOrdersFor`) rather than against the stream that dropped.
4. Then fix the cause, which is a pump that cannot keep up: too many subscribers, a
   slow consumer, or a machine that is doing something else.

**What makes it worse.** Reading it as a throughput metric. It is a correctness metric
with a throughput-sounding name.

---

## The book is at its order ceiling

**Signal.** `orderbook_resting_orders` at `MaxOrders` (100,000 by default) and
`orderbook_rejections_total{reason="order book has reached maximum capacity"}` climbing.
The venue looks healthy and accepts nothing.

**What to do.** Check first whether orders are being *orphaned* rather than legitimately
resting: compare what clients believe they hold against `orderbook_resting_orders`. A
large gap means orders in the book that no client is tracking, and no amount of raising
the ceiling fixes that — see [SOAK.md §1](SOAK.md) for the defect that produced exactly
this signature, and confirm you are not running a build from before it was fixed.

If the orders are real, the ceiling is doing its job and the question is admission
policy: `MaxOrdersPerAccount`, the rate gate, or a larger `MaxOrders` with the memory to
back it.

---

## Failover to a warm follower

> `cmd/obgw` does not ship replication wiring. This procedure is written against the
> reference primary-backup topology in `examples/replication` (specified in
> [REPLICATION.md](REPLICATION.md)) and applies to anything built on the same seams.
> Its drills live in `examples/replication/main_test.go` rather than
> `cmd/obgw/drills_test.go`.

**Signal.** Yours to define, and that is the first line on purpose: promotion is a
manual decision, because automatic failover is a liveness-versus-safety trade this
library refuses to make for you. What you will actually see: the primary's `/readyz`
dark or its host gone, and the follower's applied sequence no longer advancing because
the feed died with the primary.

**What the code has done.** The follower holds a verified prefix of the primary's log:
gapless by construction — it treats a gap as terminal rather than something later
entries repair — and digest-equal (`EngineSnapshot.Digest`) to an engine fed the same
commands. The distance between the primary's last written sequence and the follower's
applied sequence is the loss window, and it was on a gauge before the failure, not
discovered after it.

**What to do.**

1. Confirm the primary is actually gone, not partitioned. This procedure does not
   fence a primary that still runs — see *what makes it worse*.
2. Record the follower's applied sequence. It is the promoted venue's birth
   certificate: every command at or below it is preserved, everything after it is the
   loss window clients will reconcile.
3. Promote — stop the tail, write the promoted book as the new venue's base snapshot,
   open a fresh log, hand the engine to a Runner (`Follower.Promote` in the
   reference). A follower with a known defect refuses promotion; do not override it —
   serving a book known to be wrong is strictly worse than serving nothing.
4. Mint a **new incarnation id** for the new venue's Registry. This is the client
   fence: sequence numbers are scoped to one incarnation, so a cursor from the dead
   primary is refused rather than served different content under numbers the client
   believes it already has.
5. Repoint clients. They re-login and reconcile in-flight orders with Query — the same
   reconciliation an ordinary reconnect already requires.

**What makes it worse.** Reusing the old incarnation id, which turns the fence into
fiction. Promoting while the old primary may still be alive: two live incarnations are
*detectable* by clients but nothing here *prevents* them — split-brain prevention is
consensus, and it is yours by design ([REPLICATION.md](REPLICATION.md) §5, §8). And
skipping step 2, because a loss window nobody wrote down is a dispute with every
client whose order fell inside it.

---

## Debugging a live venue

The entries above are for failures with a known shape. This is for the other case:
something is wrong, you do not yet know what, and the venue is up. Work down the
list — each step is cheap and rules out a class before the next one.

**Everything here is read-only and safe on a live venue.** Nothing enqueues a
command, nothing takes the matching goroutine's lock, and none of it can change the
book. That is a property of the admin edge, not an accident: a diagnostic that could
stall the matcher would be worst exactly when it was needed. The one exception is
flagged below.

### 1. Is it the venue, or is it you?

```sh
curl -s localhost:9100/healthz          # process alive
curl -s localhost:9100/readyz           # matcher advancing
```

`/readyz` reports the queue and the event sequence. **Queue near zero and a sequence
that is not moving is a quiet market, not a stall** — the venue cannot tell you the
difference and does not try. A backing queue with a stuck sequence is the stall; that
has its own runbook above.

### 2. What does the book think it is?

```sh
curl -s localhost:9100/metrics | grep -E 'best_(bid|ask)|spread|last_trade|phase'
```

Per instrument, since v0.24.0. `NaN` on a side means that side is **empty**, not
zero — zero is a price, and the distinction is the whole reason those gauges report
NaN. `orderbook_phase` of 3 is halted, 2 is cancel-only: a venue that is "not
trading" may be doing exactly what it was told.

### 3. Is it refusing work, and whose fault is that?

```sh
curl -s localhost:9100/metrics | grep -E 'rejections_total|queue_depth|queue_capacity'
```

`orderbook_rejections_total` is labelled by reason, and the reason tells you where to
look. A duplicate client id or an unknown order is the client's model diverging from
the book. A price-band rejection is the venue working as configured.

> **Do not grep for `queue_full` — that label cannot appear, and this runbook used to
> tell you to.** A queue-full refusal is made at the gateway *before* the engine
> (`cmd/obgw/server.go:1181-1187`): it is turned straight into a wire reject and never
> becomes a `matching.EventRejected`, and every reason label is derived from that event
> (`pkg/observability/metrics.go:97-100`). The same is true of the rate-gate refusal.
> **Nothing anywhere counts a shed**, which is a real observability gap
> ([`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M14, "queue-full events"), not
> just a wrong label. Until it is closed, the ceiling case is visible two other ways:
> **depth approaching capacity** (below), and the engine error string an at-capacity
> venue returns, which is pinned against this runbook by
> `TestDrillTheCeilingRejectionNamesItself` (`cmd/obgw/drills_test.go:355`) precisely
> so this page and the code cannot drift again.

**Depth approaching capacity is the number to alert on**, not depth being non-zero —
by the time the queue is full, clients are already being refused.

### 4. Is anybody being dropped?

```sh
curl -s localhost:9100/metrics | grep -E 'publisher_dropped|obgw_connections'
```

`obgw_publisher_dropped_total` is the most important number on the page and the last
one that was added. A dropped batch is not a delayed message, it is a lost one, and
every consumer downstream derives from that stream — the index that lets a client
cancel its own order, and the execution reports for fills against it. **Non-zero
here means orders in the book that no client can cancel.** See "The publisher is
dropping batches".

### 5. Is it growing?

```sh
curl -s localhost:9100/metrics | grep -E 'obgw_(heap_bytes|goroutines|open_files)'
```

Sample it repeatedly and **watch the floor, not the instantaneous value** — heap
sawtooths between collections and a single reading tells you nothing. Goroutines and
descriptors should be flat; a trend in either is a leak and is worth waking someone
for. Descriptors are the one that ends a venue's day outright: at the limit, accept
fails and no new participant can connect while everyone already on stays perfectly
healthy.

### 6. What is it actually holding?

Metrics can tell you the heap grew. Only a profile can tell you what is holding it.

```sh
# obgw must have been started with -pprof; it is off by default.
go tool pprof -top -sample_index=inuse_space http://localhost:9100/debug/pprof/heap
go tool pprof -top http://localhost:9100/debug/pprof/goroutine
```

Take two profiles minutes apart and diff them with `-base`; a single profile shows
you what is allocated, not what is growing. This is how a heap floor that rose 28.8%
in half an hour was traced to four bounded caches filling for the first time rather
than a leak.

**The one unsafe endpoint**: `/debug/pprof/profile` takes 30 seconds of CPU by
default. On a venue at its throughput ceiling that is a real cost. `heap` and
`goroutine` are cheap snapshots.

### 7. What did the venue think it was doing?

The log is the record of every command that reached the engine, in order, and it is
readable. `wal.ReadAll` parses it; the records are JSON. That is the last resort and
also the most complete one: if a client and the venue disagree about an order, the
log settles it.

**Do not edit it.** See the two rules at the top of this page.

### What none of this covers

There is no tracing, so a slow request cannot be followed across the gateway, the
matcher and the publisher — you can see that the queue is deep and not which stage
put it there. There are no alert rules shipped, so every threshold above is one you
have to wire up yourself. Both are named in
[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) as gaps rather than described as
features.

---

## What has no runbook

Named because a gap you know about is worth more than a page that pretends otherwise.
- **A trade printed in error.** There is no bust or correction path. Once a trade is
  published there is no way to amend it, and that interacts badly with an append-only
  event stream. Design it before you need it.
- **A compromised credential.** There is no key rotation and no revocation. The
  gateway's table now holds digests rather than plaintext, which changes what a memory
  disclosure is worth and nothing about this gap: removing the account from the
  credential file and restarting is still the whole of the procedure, and it still
  drops every other session too. Replace `orderentry.Authenticator` before you need
  this.
- **Clock disagreement.** No clock-synchronisation attestation and no procedure for a
  host whose clock has jumped, which matters because time-in-force deadlines and the
  audit trail both read it.

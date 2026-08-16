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
- **Never edit the log or the snapshot by hand.** Both are checksummed. A file you have
  repaired is a file whose checksum you have recomputed over bytes you invented.

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

The `record N` in that message is the record's **ordinal in the file**, counting from
the first record, including the ones a snapshot already covers. It is not a count of
records recovery kept. This matters because recovery no longer decodes the covered
prefix — it still reads and checksums every byte of it, so a record damaged long
behind the snapshot is still caught and still refuses to start, and step 3 below
still lands on the right place in the file.

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
you will not know what it is missing.

---

## A corrupt snapshot

**Signal.** The venue refuses to start: `wal: corrupt record: snapshot <path> checksum
...`.

**What the code has done.** Snapshots are written atomically — a fully-synced temp file
renamed into place — so a crash cannot tear one. A checksum failure therefore means
media corruption, and it is refused for the same reason a corrupt log record is, only
more so: the snapshot is the base every log record after it is applied to, so starting
from a wrong one produces a book that nothing downstream can detect as wrong.

**What to do.** Delete the snapshot and restart. Recovery falls back to replaying the
log from the beginning, which is slower — a 100,000-order replay takes roughly 174 ms,
and a log that has run since the last checkpoint takes proportionally longer — but it
is exact. Then investigate the storage as above.

**What makes it worse.** Keeping the snapshot because replay is slow. Replay time is
the cheapest thing you will spend today.

> Snapshots written by a build from before the checksum existed (v0.16.0 and earlier)
> are read without one, so an upgraded venue cannot verify its existing snapshot until
> the next checkpoint rewrites it. If you want that guarantee immediately, force a
> checkpoint after upgrading.

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
look. `queue_full` is the venue behind its clients. A duplicate client id or an
unknown order is the client's model diverging from the book. A price-band rejection
is the venue working as configured.

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

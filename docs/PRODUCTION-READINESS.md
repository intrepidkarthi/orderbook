# Production readiness

This document exists because "production-ready" is the claim this repository is most
often measured against, and it is a claim that cannot be true of a library.

**Production readiness is a property of a deployment, not of code.** A matching engine
is production-ready when a named team runs it, on hardware they have capacity-planned,
behind controls they have tested, with runbooks for the failures they have rehearsed.
None of that can ship in a Go module. What a library can offer is that the pieces you
build on are correct, measured, and honest about their edges — and that is the claim
this project makes.

So this is not a badge. It is a checklist of what a venue actually needs, with an
honest status for each item: what this repository provides, what it deliberately does
not, and what you would have to build or buy. Where something is claimed, the evidence
is named so you can check it rather than trust it.

**The engine has never run a live market.** Everything below should be read with that
in front of it.

---

## Summary

| Area | Status |
|---|---|
| Matching correctness and determinism | **Strong** — machine-checked |
| Crash recovery and data integrity | **Strong** — measured, checksummed |
| Order-entry and market-data protocols | **Strong** — frozen, both edges served |
| Market-integrity controls | **Strong** — each mapped to a real case |
| Performance and its honesty | **Strong** — measured, published, corrected |
| Observability | **Partial** — Prometheus metrics, health endpoints and a reference dashboard ship; no tracing or alert rules |
| Operational readiness | **Weak** — endpoints, thresholds and runbooks; none of it rehearsed |
| Security at the edge | **Partial** — TLS, a credential seam and digests at rest ship; no rotation, revocation or expiry |
| High availability | **Seams proven** — a drilled reference example; topology and consensus deliberately yours |
| Sustained load / soak at venue scale | **Partial** — a harness, and an hour of evidence |
| Clearing, settlement, margin, fees | **Absent by design** |
| Independent review | **None** |

Three of those are the difference between "a correct engine" and "a venue you could
run": observability, operational readiness, and sustained load testing. The first is
now partly addressed and the third has a harness; the parts that remain are about your
deployment and cannot ship in a module.

The third one earned its place. Within the first hour of running under sustained load,
the venue was found to be refusing cancels for orders that were live in its own book,
filling to `MaxOrders` and then trading nothing — a defect that the whole test suite as
it then stood (480 functions), two fuzzers, the race detector and every benchmark had
missed, and that the venue reported itself healthy throughout. See [SOAK.md](SOAK.md).

---

## 1. What is genuinely solid

Each row names the evidence, because a checklist that only asserts is worth nothing.

| Claim | How it is checked |
|---|---|
| Matching is deterministic | Same command stream produces a byte-identical engine; gated in CI against a 2,000-command tape, checkpointing at five different points |
| The event stream reconstructs the book | Replayed into an L3 book identical to the engine's, across 23 scenarios covering every order class |
| Recovery is exact | Snapshot + log tail rebuilds a byte-identical engine, including all three sequence counters, the duplicate guard and conditional-order state |
| The log cannot silently corrupt | CRC-32C per record; a complete record failing its checksum refuses to start the venue rather than truncating |
| Neither can the snapshot | CRC-32C over the whole file, refused the same way. It is the base the log is replayed on top of, so a wrong one is worse than a wrong record — and it had no check at all until writing the runbook for it showed the procedure would have been "you cannot detect this" |
| Nothing leaks over an hour | 9,000,170 messages at 2,500/s: across 101 samples spanning 50 minutes the goroutine and descriptor counts did not move by one and the heap floor moved by 0.2 MiB |
| The hot path allocates nothing | Measured against `runtime.MemStats`, not the rounded `allocs/op` column: cancel 0.0002/op, maker churn 0.009/op |
| Level aggregates match the orders | Invariant asserted after full fills, after removals, and across a 2,000-operation churn |
| Market data cannot drift from the book | Derived L2 compared against the engine's own snapshot after every command of a random tape |
| A subscriber can join anywhere | Snapshot + everything after its sequence equals the book, asserted in-process and end to end over a socket |
| No data races | `go test -race -count=3` across all 16 packages |
| No panics on hostile input | 5.6M fuzz executions across two targets |

Test count: **over 600 test functions**, two fuzz targets, race and replay-recovery
in CI. Count them with `grep -rh '^func Test' --include='*_test.go' . | wc -l`.

A floor rather than a figure, and that is the second lesson this line has taught. It
read 480 for several releases after it stopped being true; corrected to an exact 584,
it was stale again within a day. A hand-maintained count goes stale by construction —
the same reason v0.19.0 deleted the hardcoded "latest version" from the docs page
rather than updating it. A floor can only ever become an understatement.

## 1a. What sustained load found that nothing else did

The engine has over 600 test functions, two fuzz targets at 5.6M executions, replay-recovery
and race detection in CI, and a benchmark suite that has twice been corrected against
itself. None of them found this:

> Under sustained load the reference gateway refused cancels for orders that were live
> in its own book. A client does not retry a definitive "no such order", so those
> orders stayed in the book, addressable by nobody, until the venue restarted. At
> 20,000 messages a second the book filled to its 100,000-order ceiling within thirty
> seconds and the venue stopped accepting liquidity — while reporting itself healthy
> the entire time.

It was not a race and not a wrong answer. It was a right answer arriving after the
question had stopped being asked, and that is a shape no unit test has a reason to
look for. Both causes and both fixes are in [SOAK.md](SOAK.md).

Two lessons, and they generalise past this repository:

- **A correct answer delivered late can be indistinguishable from a wrong one.** The
  venue's asynchronous publisher was carrying two things: a conversation with the
  client, which may lag, and the venue's answer to "which order do you mean?", which
  may not. They shared a queue because they were written at the same time.
- **A test suite tests the questions you thought to ask.** Sustained load asks
  different ones — not because it is cleverer, but because it puts the system in
  states no test constructs.

## 2. Performance, and what the numbers do and do not mean

Measured, published, and corrected when they were wrong — twice in this project's
history, both times in the direction of the documentation being flattering. See
[BENCHMARKS.md](BENCHMARKS.md).

What they cover: in-process calls into the matching core, with tail latency across six
named scenarios and recovery time at three book sizes.

What they still do not cover, and this is the gap that matters for a production claim:

- **Nothing has run for a day.** The longest run is an hour: 9,000,170 messages at
  2,500/s, 50 minutes of steady state across 101 samples, with the goroutine and
  descriptor counts not moving by one and the heap floor moving by 0.2 MiB
  ([SOAK.md §1c](SOAK.md)). That rules out the leaks that appear in an hour and says
  nothing about the ones that appear in a week. A trading day is longer and a
  deployment is months.
- **Hundreds of connections are still untested.** The soak runs use 25. The gateway's
  goroutine-per-connection model is fine in principle and unproven past a few dozen.
- **There is no capacity plan, and the first attempt at one was wrong.** The rates
  originally published here did not reproduce four hours later on the same machine and
  the same code — 7,000/s clean became 3,500/s clean — because the measurement never
  controlled for what else the host was doing. Ruled out as a code regression by an
  interleaved A/B; see [SOAK.md §1b](SOAK.md). What survives is the shape: the durable
  path through a socket and a protocol runs three orders of magnitude below the
  in-process benchmarks, and the command queue is what gives first. What also survives
  is everything structural — a bounded book, no orphaned orders, no leaked goroutines
  or descriptors, p50 of 5 ms below saturation — because correctness findings are a
  property of the code and timing figures are a property of the host.
- **The log costs more than anything else here, and it is the one figure that
  reproduces.** About 220 bytes of journal per client message, measured at two rates an
  evening apart and agreeing almost exactly: 1.22 GiB in twenty minutes at 5,000/s,
  1.83 GiB in an hour at 2,500/s — 44 GiB a day at the lower rate. The records are JSON,
  chosen for readability and never priced. Budget for it or change the encoding; the
  framing, checksums and recovery do not care what the payload is.

What the first soaks did establish is that microbenchmarks were measuring the wrong
thing to predict any of this. The engine's in-process figures are in the millions of
operations a second; the venue's sustained ceiling through a real socket, a real
protocol and a durable log is three orders of magnitude below that, and the first thing
that broke under sustained load was not throughput at all — it was correctness.

## 3. What a venue needs that this does not provide

### Observability — partial

What exists now: `pkg/observability` is a Prometheus collector that attaches to the
engine as an `EventSink`, so it counts what the book saw rather than what a gateway
believed it sent. `cmd/obgw` serves it on a separate admin port alongside `/healthz`
and `/readyz`. Counters cover order lifecycle, trades, phase transitions and
rejections broken down by reason; gauges cover queue depth, book size, top of book,
phase, connections, goroutines, heap and file descriptors. No new dependency — the
exposition format is written directly.

Three decisions in there are worth knowing before you rely on it:

- **Nothing a scrape touches goes through the command queue.** An endpoint that
  enqueued would answer promptly while the venue was healthy and hang exactly when the
  matcher stalled, losing the reading at the only moment anybody wanted it.
- **`/healthz` does not probe the matcher.** A failed liveness check means "restart
  me", and restarting a venue that is holding a book because a probe was slow is worse
  than the stall. `/readyz` is the one that should pull a node out of rotation, and it
  fires on queue occupancy past a high-water mark or on the event sequence standing
  still while commands wait — which is the only way a stalled matcher is
  distinguishable from a quiet market.
- **An empty book reports NaN, not zero.** Zero is a price.

What still does not exist: tracing, structured logging, and alert rules. A
reference dashboard now does — `cmd/obdash`, an ordinary market-data subscriber
plus a `/metrics` reader that draws the queue meter with the 75% threshold on it
and refuses to show a stale number as a healthy one. It is a viewing surface,
not an alerting system: the numbers are exposed and drawn; deciding what pages a
human is still yours.

*(An earlier version of this document claimed a `Metrics` seam existed, because the
README's contributing list mentions one. It did not exist and never had — the reference
was to something a contributor might build. Recorded here rather than quietly deleted,
because it was the fourth documented seam in this project that turned out not to be
there, and that pattern is worth more to a reader than a clean page.)*

### Operational readiness — weak

What exists: health and readiness endpoints an orchestrator can use, metrics to alert
from with the thresholds named, and [RUNBOOKS.md](RUNBOOKS.md) — procedures for a torn
log, a corrupt log record, a corrupt snapshot, a stuck matching goroutine, a mass
cancel that pauses the venue, an evicted subscriber, a publisher dropping batches, and
a book at its ceiling. Each is written from the code that produces the failure, and
each names what makes it worse.

Writing them was worth it for a reason beyond the pages: drafting the procedure for a
corrupt snapshot turned up that the honest answer was *"you cannot detect this"* — the
log was CRC-checked per record and the snapshot it is applied on top of had no
integrity check at all. That is fixed, and it is the second time in two weeks that
documenting something carefully found a defect in it.

The procedures are also executable. `cmd/obgw/drills_test.go` runs one drill per entry
on every CI run: it corrupts a real log record and asserts the venue refuses to start,
blocks a real journal write and asserts `/readyz` reports a stall while `/healthz` stays
up, overflows a real publisher and asserts the drop counter moves, fills a book to its
ceiling and asserts the rejection carries the exact label this page tells you to alert
on. Each was verified to fail against deliberately broken code.

What that buys is narrower than it sounds, and the distinction matters: **a drill proves
the runbook is not stale, not that anyone can follow it.** It catches a renamed reason
string or a fallback that stopped falling back. It says nothing about a human executing
the procedure under pressure on a venue that is losing money, and nobody has done that.
This stays **weak** until somebody has. There is also no credential revocation and no
clock-disagreement procedure — both named at the end of RUNBOOKS.md rather than left to
be discovered. The failover procedure closed in v0.18.0 and the trade-bust path in
v0.21.0; neither has a rehearsal by a human under pressure, which is the sentence above.

### Security at the edge — partial

What exists now: TLS on every listener (`-tls-cert`/`-tls-key`, TLS 1.2 floor,
handshake on the connection's own goroutine so a stalled peer cannot hold up the accept
loop). Credentials load from a permission-checked file rather than a command line, and
neither path ever logs a secret — a malformed entry is reported by line number, because
the obvious version of that parser printed the offending line and a log is kept, shipped
and indexed. `orderentry.Authenticator` is the seam for where credentials actually live;
both built-ins compare in constant time, deny by default, and refuse an account with a
blank secret.

Digests at rest: the gateway's credential table holds SHA-256 digests, not passwords —
`user:sha256:<hex>` file entries load as-is (`obgw -hash-secret` generates them), and a
plaintext entry is hashed at load, with the count logged so an operator can watch it
reach zero. Three boundaries of that claim, stated rather than implied: a plaintext
entry is still plaintext *on disk*; parsing leaves transient copies that are garbage to
the collector, not zeroed; and the hash is deliberately fast — right for the
machine-issued, high-entropy secrets a venue should be handing out, and no protection
for a human-chosen password an attacker can brute-force offline from a dumped digest
file. The doc comment on `HashedAccounts` defends the fast hash (a memory-hard one on
the pre-auth path is a DoS amplifier aimed at the accept loop); if your secrets are
human-chosen, the fix is a real credential system behind the seam, not a slower hash.

What does not exist: rotation, revocation, expiry, and any per-account authorisation
beyond authentication. `StaticAccounts`, the plaintext built-in, remains what its own
documentation says it is — a correct *default*, not a credential store. Market data is
anonymous by design.

What *is* handled: authentication defaults to deny, a client cannot name another
account's order because the wire has no field for it, payloads are fixed-width and
bounds-checked by inspection, the record length is bounded so a corrupt file cannot
force a huge allocation, and every queue is bounded so a slow consumer is disconnected
rather than allowed to back up into the venue.

What is not: transport security, secrets management, DoS resistance beyond per-account
rate limits and bounded queues, and any kind of penetration testing.

### High availability — seams proven, topology still yours

Deterministic apply, an ordered command log, replay mode and snapshot bootstrap are
what a primary-backup topology needs, and they now have the thing this project's
record says to demand before believing a seam: a consumer.
[REPLICATION.md](REPLICATION.md) specifies it and `examples/replication` is it — a
primary shipping its log over TCP, a follower that bootstraps from a snapshot taken
mid-stream and never stops replaying, and promotion into a live venue. Six drills run
on every CI pass: books digest-equal to an uninterrupted control, mid-stream
bootstrap, promotion preserving exactly the applied prefix, the incarnation fence
refusing a dead primary's cursor, refusals replaying as refusals, and a slow follower
shed without the matcher waiting.

Building the consumer promptly found the fifth phantom — the log-tail hook's first
shape handed subscribers a pointer into state the matcher keeps mutating, a data race
the recovery tests could never see and the drills' first `-race` run did. That is the
argument for the whole exercise, stated as a result instead of a thesis.

Still yours, by design: consensus, split-brain *prevention* (the incarnation fence
detects, it does not prevent), automatic failover, synchronous replication, and what
"committed" means to you. The reference picks async shipping with a measured loss
window and says so — see [EXCHANGE-ARCHITECTURE.md](EXCHANGE-ARCHITECTURE.md) for why
bundling a consensus would force a wrong answer on everybody.

### Multi-symbol — ships, with one thing deliberately absent

Order and trade ids are partitioned into a shard index and a per-shard counter, so a
single `int64` names an order across the whole venue; the mapping lives in a durable,
CRC-checked manifest, because losing it makes every id ever issued ambiguous. Each
symbol is an independent book with its own command log, market-data feed and rate
gate, so recovery and replication are the single-symbol code paths run N times.
`cmd/obgw` serves a set of instruments, and a market-data subscription names the one
it wants (wire v4). See [MULTI-SYMBOL.md](MULTI-SYMBOL.md).

This section previously said "a multi-symbol venue is a routing layer you write."
That was wrong in a way worth recording: `ShardsConfig` had no way to supply a
`CommandLog`, and durability, recovery and replication all hang off it — so a sharded
venue could not survive a restart at all. It was most of a venue, not a routing layer.

**Deliberately absent: any order of events across symbols.** No cross-symbol
atomicity, no spread or basket orders, no venue-wide "as of" instant, and event
sequences that are per symbol and not comparable between them. A venue-wide sequence
needs a serialisation point every command passes through, which is the bottleneck
sharding exists to remove. Per-symbol metric series ship: the price gauges carry a `symbol` label, one
series per book, and they do so even at a one-instrument venue — a metric whose
label set depends on how the venue happens to be configured is one no dashboard
can be written against.

### The financial stack — absent by design

No fees or rebates. No clearing or settlement. No positions or margin — liquidation
*hooks* exist (privileged orders, force-trade) but the risk system they would serve does
not.

**Trade bust ships; trade correction does not.** `Engine.Bust` annuls a published
print as an appended event, journalled, replicated and covered by the digest
([TRADE-BUST.md](TRADE-BUST.md)) — this section used to say there was no way to
amend a published trade, and that it should be designed early. It was, and the
design is that a bust does not rewind anything: not the book, not the stops the
print fired, not the reference price. Adjusting a trade to a different price
rather than voiding it is still absent, and needs the settlement layer above.

Both edges of the wire carry it. Protocol **v3** gives `Executed` and `MDTrade` a
`TradeID` and adds `Busted` (order entry, private to the two counterparties) and
`MDBust` (market data, public), proven end to end over real sockets in
`cmd/obgw/bust_e2e_test.go`. Routing turned out to be the hard part rather than
the encoding: by the time a bust arrives both orders have usually left the book, so
`orderentry.Registry` keeps a bounded memory of recent prints for it, and a bust
older than that memory is counted in `UnroutableBusts` rather than dropped. Size
that memory to your bust window.

### Regulatory — partial

An audit trail exists in `pkg/gateway`, and market-abuse surveillance covers spoofing,
layering, order-to-trade ratios, marking the close, ramping, pinging and cross-book
patterns, each mapped to a real enforcement case in [THREAT-MODEL.md](THREAT-MODEL.md).

Absent: CAT/MiFID reporting formats, clock-synchronisation attestation, formal record
retention, and anything jurisdiction-specific.

### Independent review — none

No external security audit, no third-party code review, no client conformance
certification programme. The most adversarial reviews this code has had are its own:
several releases were spent auditing earlier releases' claims, and those audits found
real defects. That is a good habit and it is not the same as someone else looking.

---

## 4. If you intend to run this

In the order that will actually help:

1. **Instrument it.** `pkg/observability` and the admin edge on `cmd/obgw` are the
   starting point and not the end of it: you still need alert thresholds and
   tracing, and `cmd/obdash` is the starting dashboard, not the end of one. Nothing else on this list is useful until you can see what the venue is
   doing — which is not a slogan. The defect in §1 was invisible from inside the
   process, and the metric that would have shown it did not exist until somebody went
   looking.
2. **Soak it at your volume.** `cmd/obsoak` will do it; see [SOAK.md](SOAK.md). Days,
   not minutes, with your order mix, and watch the floor of the heap rather than its
   trend. This is still where the unknowns are.
3. **Put real credentials at the edge.** TLS and digests-at-rest are in the reference
   gateway now; rotation, revocation, expiry and where your secrets actually live are
   not, and the `Authenticator` seam is where your answer goes.
4. **Decide your HA story and rehearse the failover.** The seams are proven, the
   reference topology exists and its failover procedure is drilled
   ([REPLICATION.md](REPLICATION.md), [RUNBOOKS.md](RUNBOOKS.md)); the topology
   decision, split-brain prevention, and the rehearsal on *your* deployment are still
   yours.
5. **Write the runbooks** for: recovery from a torn log, a corrupt snapshot, a stuck
   matching goroutine, a mass cancel that pauses the venue, and a subscriber that has
   fallen off the retention ring.
6. **Get it reviewed** by someone who did not write it.

## 5. What this project will and will not say about itself

It says: *a production-grade embeddable matching core with a demonstrated network
seam*. Both qualifiers are load-bearing — production-grade describes the core,
embeddable concedes the venue is not here.

It does not say, and will not: that a venue built on it is production-ready. That is
something you establish about your deployment, with evidence of your own.

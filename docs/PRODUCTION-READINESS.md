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
| Observability | **Weak** — seams exist, nothing ships |
| Operational readiness | **Absent** — no runbooks, no health checks |
| Security at the edge | **Weak by design** — no TLS, secrets in the clear |
| High availability | **Seams only** — deliberately no consensus |
| Sustained load / soak at venue scale | **Absent** — microbenchmarks only |
| Clearing, settlement, margin, fees | **Absent by design** |
| Independent review | **None** |

Three of those are the difference between "a correct engine" and "a venue you could
run": observability, operational readiness, and sustained load testing. They are also
the three that no amount of further library work fixes, because they are about your
deployment.

---

## 1. What is genuinely solid

Each row names the evidence, because a checklist that only asserts is worth nothing.

| Claim | How it is checked |
|---|---|
| Matching is deterministic | Same command stream produces a byte-identical engine; gated in CI against a 2,000-command tape, checkpointing at five different points |
| The event stream reconstructs the book | Replayed into an L3 book identical to the engine's, across 22 scenarios covering every order class |
| Recovery is exact | Snapshot + log tail rebuilds a byte-identical engine, including all three sequence counters, the duplicate guard and conditional-order state |
| The log cannot silently corrupt | CRC-32C per record; a complete record failing its checksum refuses to start the venue rather than truncating |
| The hot path allocates nothing | Measured against `runtime.MemStats`, not the rounded `allocs/op` column: cancel 0.0002/op, maker churn 0.009/op |
| Level aggregates match the orders | Invariant asserted after full fills, after removals, and across a 2,000-operation churn |
| Market data cannot drift from the book | Derived L2 compared against the engine's own snapshot after every command of a random tape |
| A subscriber can join anywhere | Snapshot + everything after its sequence equals the book, asserted in-process and end to end over a socket |
| No data races | `go test -race -count=3` across all 16 packages |
| No panics on hostile input | 5.6M fuzz executions across two targets |

Test count: **480 test functions**, two fuzz targets, race and replay-recovery in CI.

## 2. Performance, and what the numbers do and do not mean

Measured, published, and corrected when they were wrong — twice in this project's
history, both times in the direction of the documentation being flattering. See
[BENCHMARKS.md](BENCHMARKS.md).

What they cover: in-process calls into the matching core, with tail latency across six
named scenarios and recovery time at three book sizes.

What they do not cover, and this is the gap that matters for a production claim:

- **No sustained load test.** Every figure is a microbenchmark over seconds. Nobody has
  run this at target volume for a day, let alone a week. Memory growth, GC behaviour
  under sustained pressure, and file-descriptor and goroutine leaks across a long
  session are all unmeasured.
- **No multi-client load test.** The gateway has been tested with a handful of
  connections, not hundreds. Its per-connection goroutine model is fine in principle
  and unproven in practice.
- **No capacity plan.** The benchmarks tell you what one operation costs. They do not
  tell you how many participants, orders per second, or symbols a given machine will
  carry, because nobody has measured that.

## 3. What a venue needs that this does not provide

### Observability — weak

There is a `Metrics` seam and `QueueLen`/`QueueCap` gauges, and the event stream can
drive anything. **Nothing ships**: no Prometheus exporter, no tracing, no structured
logging, no health or readiness endpoint. You will build this, and you need it before
you need almost anything else here, because an unobservable venue is one you cannot
operate even when it is behaving.

### Operational readiness — absent

No runbooks. No documented recovery procedure beyond "the code recovers". No rehearsed
failure drills. No alerting thresholds — and note that the numbers you would alert on
*are* now published (mass cancel blocks the matching goroutine for ~872 µs per 5,000
orders; a 100,000-order restart takes ~174 ms), but knowing them is not the same as
having an on-call engineer who has practised the response.

### Security at the edge — weak, and deliberately so

The reference gateway sends a shared secret in the clear and says so. No TLS, no
credential storage, no key rotation, no per-account authorisation beyond authentication.
Market data is anonymous by design.

What *is* handled: authentication defaults to deny, a client cannot name another
account's order because the wire has no field for it, payloads are fixed-width and
bounds-checked by inspection, the record length is bounded so a corrupt file cannot
force a huge allocation, and every queue is bounded so a slow consumer is disconnected
rather than allowed to back up into the venue.

What is not: transport security, secrets management, DoS resistance beyond per-account
rate limits and bounded queues, and any kind of penetration testing.

### High availability — seams only

Deterministic apply, an ordered command log, replay mode and snapshot bootstrap are all
here, which is what a primary-backup topology needs. The consensus is deliberately not,
because bundling one forces a wrong answer on everybody — see
[EXCHANGE-ARCHITECTURE.md](EXCHANGE-ARCHITECTURE.md), including the venues that lost
quorum getting it wrong.

You will need: a replication mechanism, a tested failover procedure, split-brain
protection, and a decision about what "committed" means to you.

### Multi-symbol — partial

`matching.Shards` routes by symbol across independent engines. But order ids and event
sequences are per-engine, so there is no venue-wide identifier space, and a client
crossing symbols sees independent sequence spaces. The reference gateway serves one
instrument. A multi-symbol venue is a routing layer you write.

### The financial stack — absent by design

No fees or rebates. No clearing or settlement. No positions or margin — liquidation
*hooks* exist (privileged orders, force-trade) but the risk system they would serve does
not. No trade bust or correction: once a trade is published there is no way to amend it,
and that interacts badly with an append-only event stream, so design it early if you
need it.

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

1. **Instrument it.** Metrics off the event stream, the queue-depth gauges, and
   latency histograms. Nothing else on this list is useful until you can see what the
   venue is doing.
2. **Soak it at your volume.** Days, not seconds, with your order mix. Watch memory,
   goroutines, file descriptors and GC. This is where the unknowns are.
3. **Put TLS and real credentials at the edge.** The reference gateway is explicit that
   it is not suitable as-is.
4. **Decide your HA story and rehearse the failover.** The seams are here; the decision
   and the drill are yours.
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

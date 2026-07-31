# Soak testing

[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) names three things that separate
"a correct engine" from "a venue you could run". This document is about the third:
sustained load. It was, until this was written, the largest unmeasured claim in the
repository — every published figure came from a microbenchmark over seconds, and the
failures that end a trading day do not appear in seconds.

`cmd/obsoak` is the harness. What follows is what it measures, what it found, and
what is still unknown.

**It found a defect within its first hour that 480 tests, two fuzzers, the race
detector and every benchmark had missed.** That is the argument for the whole
exercise, so it is first.

---

## 1. What it found

### Orders that no client could cancel

Under sustained load the reference gateway would refuse a cancel for an order that
was live in its own book, telling the client no such order existed. A client does
not retry that — it was given a definitive answer — so the order stayed in the book,
addressable by nobody, until the venue restarted. The book filled to `MaxOrders` and
the venue stopped accepting new liquidity.

Measured on 25 connections over 30 seconds, before the fix:

| Rate | Orders resting | Clients believed | Orphaned |
|---:|---:|---:|---:|
| 1,000/s | 1,900 | 2,488 | none |
| 4,000/s | 1,968 | 2,485 | none |
| 10,000/s | 15,332 | 2,489 | **12,843** |
| 20,000/s | 99,999 | 2,486 | **97,513** |

99,999 is `MaxOrders`. At 20,000 messages a second the venue had stopped trading in
any meaningful sense within half a minute, and reported itself healthy throughout.

There were two causes, and the first fix made the second visible.

**The naming index was maintained by the publisher.** A client names its order by its
own identifier; the venue maps that to an engine order id when the order is accepted.
That mapping was written by the publisher's pump goroutine, along with the outbound
message stream. Both may be behind — but only one of them may be behind *safely*. A
late acknowledgement is still an acknowledgement. A late answer to "which order do
you mean?" is a wrong answer, because the question is not asked twice.

Fixed by [`pkg/orderentry/nameindex.go`](../pkg/orderentry/nameindex.go): naming
happens on the matching goroutine, in one map write, the instant the engine accepts
the order. Everything expensive stays on the pump.

**The lookup happened before the queue.** With the first fix in, orphaning got
*worse*, which ruled out the pump as the whole story. The gateway resolved the
identifier on its read loop and then enqueued a cancel carrying the resolved id — so
under a queue backlog it was asking "does this order exist?" while the `Enter` that
creates it was still queued ahead of the cancel.

The enqueue order had always been right; the reference gateway is careful to enqueue
on its read loop precisely so a cancel cannot overtake its own `Enter`. But the lookup
happened before the queue, so that guarantee was protecting the wrong step.

Fixed by `command.resolve` in [`pkg/matching`](../pkg/matching/queue.go): the target is
named on the matching goroutine when the command reaches the front of the queue, after
every earlier command has been applied, and before the command is journalled so the log
records the engine id rather than a name that only existed inside a gateway process.

After both fixes, 25 connections over 30 seconds:

| Rate | Orders resting | Clients believed | Orphaned |
|---:|---:|---:|---:|
| 10,000/s | 1,645 | 2,491 | none |
| 20,000/s | 1,272 | 2,488 | none |
| 5,000/s, 20 min | 1,963 | 2,487 | none |

The venue now holds slightly *fewer* orders than the clients think, which is the
correct direction: the difference is cancels in flight.

### A concurrent map write, thirty seconds later

Splitting the naming index onto its own lock meant finding every writer of that map.
One was missed — `fill()`, which drops a name when a fill exhausts the order — and it
kept writing under the old mutex. The race detector did not reach it across the whole
test suite. The soak crashed the process on `fatal error: concurrent map writes`
within thirty seconds of the fix landing.

Both are regression-tested now
([`nameindex_test.go`](../pkg/orderentry/nameindex_test.go),
[`resolve_test.go`](../pkg/matching/resolve_test.go)), and both tests fail against the
code as it was.

### Nothing reported the publisher dropping events

The publisher discards its oldest batches when its queue overflows. That is the right
call — blocking would stop the venue and growing without limit would end it — and it
is documented. But a dropped batch is not a delayed message, it is a lost one: every
consumer downstream derives from that stream. Until `obgw_publisher_dropped_total`
existed there was nothing anywhere that said it had happened.

It reads zero in every run recorded here. That is worth knowing too, and it was not
knowable before.

---

## 1a. The first clean run

20 minutes, 25 connections over 25 accounts, 5,000 messages a second, durable (WAL and
30-second checkpoints), on an Apple M4 (10 cores), Go 1.23.5, loopback. Both the venue
and the load generator on the same machine.

```
throughput
  sent             6,000,042   (5,000/s, target 5,000/s)
  fills            1,298,701
  rejected           669,207   (11.153%)
  errors                   0
  unanswered              26   (0.000%)
    command unknown order             652,222
    command queue full                 16,985

client-observed latency, socket write to first response
  p50                    5ms
  p90                   25ms
  p99                  250ms
  mean           20.651826ms

steady state: 61 samples over 15m0s, after a 5m0s warmup
  heap            floor   39.8 MiB ->   39.9 MiB   trend +16.0 MiB/hour
  goroutines      floor        111 ->        111   trend -0/hour
  descriptors     floor         36 ->         33   trend -1/hour
  resting orders  floor      1,868 ->      1,859   trend +18/hour

VERDICT: no growth in heap, goroutines or descriptors over 15m0s at 5,000 msg/s.
This is evidence for 15m0s, and for nothing longer.
```

Reading it honestly:

- **Nothing leaked over fifteen minutes.** Heap floor flat to within 0.1 MiB across
  61 samples, goroutines pinned at 111, descriptors unchanged. The `+16.0 MiB/hour`
  trend is the saw-tooth, which is why the verdict is not computed from it.
- **The book held its shape.** 1,963 resting against 2,487 the clients believed —
  the difference is cancels in flight, and it is the correct direction. Before the fix
  in §1 this figure climbed to the 100,000-order ceiling.
- **11% rejections is the workload, not a fault.** 652,222 of them are `unknown order`:
  cancels of orders that had already been filled, which is what happens when a fifth of
  the flow is marketable and a participant cancels its oldest quote rather than
  checking first. A real client would have the fill first.
- **16,985 `queue full` — 0.28% — is the venue at its limit,** briefly. The command
  queue sat near zero for the run and spiked past 2,000 twice. 5,000/s durable is
  comfortable here; 7,000/s durable still runs clean; 10,000/s durable saturates.
- **p99 of 250 ms is the top bucket, and the load generator is on the same machine.**
  Treat the tail as an upper bound with the harness's own scheduling in it, not as a
  venue figure. p50 of 5 ms through a real socket, a real protocol and an fsync-ing log
  is the number to reason from.

### The log is the capacity constraint nobody had costed

The write-ahead log reached **1.22 GiB in twenty minutes** — about 218 bytes a record,
**3.7 GiB an hour, 88 GiB a day** at this rate. The records are JSON.

That is not a defect; it is a design choice made for readability and never priced. But
it is the first thing a deployment will hit that no benchmark on this repository
mentions, and it changes what you provision, how often you checkpoint, and how long you
can retain. If you run this for real, either budget for it or replace the record
encoding — the framing, checksums and recovery do not care what the payload is.

### What a second pass over the same code found

Re-reading the fixes rather than re-running them turned up four more things. None
would have been found by adding tests to what was already there; each came from asking
what the *change itself* had made possible.

- **A name could outlive its order.** Naming became synchronous while forgetting stayed
  on the pump, whose queue drops batches — so an order could be named and never
  tracked, and `forget` had nothing to look the name up by. `forget` now takes the
  identifier from the event, which cannot go missing.
- **A resolver that panicked killed the venue.** Moving name resolution onto the
  matching goroutine put caller-supplied code on the one goroutine whose death stops
  trading. Contained now, and argued: it is the only place the runner recovers, because
  a resolver runs before the command is journalled or applied, so a panic in it says
  nothing about the book.
- **Naming allocated.** A composite `account+"\x00"+clOrdID` key, built on every write —
  16 B and one allocation per accepted order, on the hot path of an engine that claims
  to allocate nothing. A nested map removed it: 45 ns and one allocation became 26 ns
  and none, and the account scoping became a property of the data rather than a
  convention about how a string was built.
- **The harness could not report on time.** Client sends had no deadline, so a
  saturated venue delayed the report by six minutes on a seven-minute run; adding the
  deadline then exposed a deadlock on the error path that nothing could previously
  reach. Both fixed. 90 seconds for a 90-second run at 12,000 msg/s with the queue full
  throughout.

The pattern is worth naming: **every one of these was created by the previous fix.**
A change to locking, or to which goroutine owns what, does not have a blast radius you
can reason about from the diff.

### Saturation is now survivable rather than terminal

The clearest evidence the fix worked is not a clean run — it is a saturated one. At
12,000 messages a second across 40 connections, with the command queue full in every
sample, the venue held 1,753 orders against 3,978 the clients believed. Nothing
orphaned. Before, that same condition filled the book to its 100,000-order ceiling and
the venue stopped accepting liquidity for good.

Saturation should mean backpressure and rejections. It used to mean permanent damage.

---

## 2. What the harness measures

Two vantage points, because they answer different questions.

**From the client.** End-to-end latency, from writing an order onto a socket to
reading its first response back off one. That is what a participant experiences, and
the only figure the server cannot produce: the server's own histogram starts after
the kernel has already handed it the bytes.

**From the venue's `/metrics`.** Heap, goroutines, file descriptors, resting orders
and queue depth, on a fixed interval. Growth in any of them, across a run that has
reached steady state, is the finding.

### Steady state is the methodology

A load generator that only adds orders grows the book without limit, and then "memory
grew" measures the workload rather than the venue. So each participant holds at most
`-resting` orders and cancels its oldest to make room, and a share of the flow is
marketable and never rests. The book plateaus; after that, growth is the server's.

The warmup is excluded from the growth analysis for the same reason. Heap climbing
while caches fill and the book fills is not a leak, and including it would produce a
positive slope on every healthy run.

### Three things it reports that a throughput number would not

- **Rejections by reason.** "22,937 rejects" says something is wrong and nothing about
  what. The first run produced exactly that number with no way to read it; the
  breakdown identified `order book has reached maximum capacity` immediately.
- **Saturation.** A run whose command queue sat full measured the slowest thing in the
  pipeline, not the venue's behaviour over time. Every other figure is still true and
  none of them means what it looks like it means, so this is printed before them
  rather than after.
- **The book as the clients see it, against the book as the venue does.** Neither
  number alone shows anything. The comparison is what found the orphaning.

### Growth is judged on the floor, not the trend

Live heap saw-tooths under Go's GC pacing. A sample at a peak and a sample at a trough
differ by more than any leak would in the same window, so a least-squares fit through
raw samples reports whatever the scrape schedule happened to catch. What a leak moves
is the *floor* — the low-water mark is data that survived a collection — so the verdict
compares the floor of the first half of the steady window against the floor of the
second.

Below five minutes of steady state the harness declines to conclude at all. "No growth
over four minutes" would be the most misleading sentence it could print, because it is
the one somebody would quote.

### Two things about the invocation that are not incidental

**One account per connection.** Order entry is per-account and so is self-trade
prevention, so running every connection as the same participant makes the engine
correctly refuse to cross any of them with any other. The run prints no trades at all
and measures the book path with the matching path switched off. The first version of
this harness did exactly that — 482,394 orders, zero fills — and its numbers looked
entirely plausible. `obsoak -print-accounts` emits the credential list `obgw` needs.

**The venue's per-account rate limit lifted**, or the soak measures the rate limiter.
Leaving it in place is a different and also worthwhile test; conflating the two makes
both unreadable.

---

## 3. Running it

```sh
ACCTS=$(obsoak -conns 25 -print-accounts)
obgw  -addr :9000 -admin :9100 -accounts "$ACCTS" -rate 1e9 -burst 1e9 \
      -wal /tmp/soak.wal -snapshot /tmp/soak.snap
obsoak -addr :9000 -admin :9100 -conns 25 -rate 5000 -duration 30m -warmup 5m
```

---

## 4. What is still unknown

Everything a longer run would tell you. The runs here are minutes; a trading day is
hours and a deployment is months. In particular:

- **Nothing has run for a day.** Fragmentation, log growth, snapshot cadence over a
  full session, and the behaviour of a book that has been continuously churned for
  hours are all unmeasured.
- **Hundreds of connections are untested.** These runs use 25. The gateway's
  goroutine-per-connection model is fine in principle and unproven past a few dozen.
- **One machine, one instrument, loopback.** No network, no NIC, no other tenant, no
  contention with anything a real deployment would have next to it.
- **The client is Go and shares the machine.** At high rates the harness is competing
  with the venue for the same cores, so the client-observed latency includes something
  that would not exist in a real deployment — and the capacity figures are, if
  anything, pessimistic for the venue and optimistic about nothing.

A soak that finds nothing is evidence for the length of the soak, and for nothing
longer. That sentence is printed in the harness's own verdict for the same reason it
is written here.

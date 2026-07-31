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

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
  queue sat near zero for the run and spiked past 2,000 twice. **See §1b: the rate this
  run sustained is not a property of the code, and it did not reproduce.**
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

## 1b. A correction: the capacity figures did not reproduce

The first version of this document said 5,000 messages a second was comfortable,
7,000 ran clean and 10,000 saturated. Four hours later, on the same machine and the
same code, 3,500 ran clean and 5,000 saturated — a little under half.

The code was ruled out first, by the method this repository already uses for
performance claims and which I did not apply here: an interleaved A/B against a
worktree at the earlier commit, alternating arms so drift lands on both. Three rounds,
25 connections, 5,000/s:

| | queue-full rejections |
|---|---|
| before the second round of fixes | 14,198 · 18,502 · 15,764 |
| after | 19,919 · 18,660 · 12,582 |

Indistinguishable. The difference was the machine: a desktop that had been idle in the
morning and by the evening was running a window server, browsers and several other
processes. The measurement never controlled for that and never recorded it.

**So the honest statement is that this repository does not know what rate the venue
sustains.** It knows the shape — the durable path through a socket and a protocol is
three orders of magnitude below the in-process benchmarks, and the command queue is
what gives first — and it knows two numbers measured under conditions it failed to
write down.

This is the third published figure in this project's history to be corrected against
itself, and the second where the documentation was flattering.

### What did hold

Everything structural, at every rate tried, on both arms of the A/B:

- The book stayed bounded — 1,917 to 1,940 orders resting against ~2,485 the clients
  believed, at 1,500/s, 2,500/s, 3,500/s, 5,000/s and 12,000/s.
- No orphaned orders, no dropped publisher batches, no leaked goroutines or descriptors.
- p50 of 5 ms below saturation.

That distinction is the useful one. **Timing figures are a property of the host;
correctness findings are a property of the code.** A soak is worth running for the
second kind even when it cannot pin down the first.

### The methodology fix

`obsoak` now runs a fixed-work arithmetic probe before and after every run and prints
it first:

```
machine   fixed-work probe 57ms before, 58ms after  — stable
```

It measures the thing that actually matters — how much CPU this process can get —
rather than a number the kernel keeps about everybody, and it needs no per-platform
mechanism. Within a run, a large gap between the two means the run's own figures are
internally incomparable. Between runs, the value is the key: two runs whose probes
disagree were not measuring the same machine, and their throughput numbers should not
be put in the same table. Every capacity figure quoted from here on carries it.

### And the connection-count question, answered

An earlier run appeared to show 40 connections saturating at 6,000/s where 25 had run
clean at 7,000 — suggesting per-connection cost, not message rate, was the binding
constraint, and therefore that "hundreds of connections" was a wall rather than a
scaling question.

It is not. Holding the rate at 5,000/s and varying only the connection count:

| connections | obgw CPU | goroutines |
|---:|---:|---:|
| 10 | 123% | 51 |
| 20 | 120% | 91 |
| 40 | 128% | 171 |
| 80 | 144% | 331 |

Eight times the connections for 17% more CPU. Per-connection cost is real — four
goroutines each, and a 2 ms poll on every session's outbound stream, which is a
documented shortcut in `followStream` — but it is not what binds at these scales. The
apparent connection wall was the same machine drift as everything else in this section.

---

## 1c. The long run

An hour at 2,500 messages a second, 25 connections over 25 accounts, durable (WAL and
30-second checkpoints), Apple M4, Go 1.23.5, loopback — and this time the machine's
share of itself is recorded, so the run can be compared with another.

```
machine   fixed-work probe 57ms before, 57ms after  — stable

  sent             9,000,170   (2,500/s, target 2,500/s)
  fills            1,950,390
  rejected         1,000,724   (11.119%)
  errors                   0
  unanswered              25   (0.000%)
  p50 5ms   p90 25ms   p99 250ms

  participants believe   2,486 resting
  venue reports          1,958 resting

steady state: 101 samples over 49m59s, after a 10m0s warmup
  heap            floor   38.7 MiB ->   38.9 MiB   trend +4.6 MiB/hour
  goroutines      floor        112 ->        112   trend -0/hour
  descriptors     floor         37 ->         37   trend +0/hour
  resting orders  floor      1,874 ->      1,873   trend -4/hour

VERDICT: no growth in heap, goroutines or descriptors over 49m59s at 2,500 msg/s.
This is evidence for 49m59s, and for nothing longer.
```

Nine million messages, two million fills, zero errors, twenty-five unanswered sends out
of nine million. Across 101 samples spanning fifty minutes the goroutine count and the
descriptor count did not move by one, the heap floor moved by 0.2 MiB, and the book
held its shape. `obgw_publisher_dropped_total` stayed at zero throughout.

This is the run the harness was built for, and it is worth being precise about what it
licenses: fifty minutes of steady state at one rate, on one machine, with one workload
shape. A trading day is longer, a real order mix is not this one, and 25 connections is
not 500. It rules out the leaks that show up in an hour. It says nothing about the ones
that show up in a week.

### The log, again, and this time reproducibly

1.83 GiB in an hour at 2,500 messages a second — about 218 bytes a record, matching the
earlier measurement at twice the rate almost exactly. So unlike the throughput figures,
this one reproduces: **roughly 220 bytes of journal per client message, whatever the
rate.** At 2,500/s that is 44 GiB a day.

It is the most predictable number this document contains and the one most likely to
decide what a deployment can afford. The records are JSON, which was chosen for
readability and never priced; the framing, checksums and recovery do not care what the
payload is.

---

## 1d. The first multi-symbol run

Three books on one venue, 1,200 messages a second, 8 connections over 4 accounts,
durable, with every connection trading all three instruments and every cancel naming
its order by client id alone. That last part is the point: the wire does not carry a
symbol on a cancel, so the venue has to know which book each order is on, and this is
the first time that routing has run for longer than a test.

```
machine   fixed-work probe 61ms before, 60ms after  — stable

  sent               432,082   (1,200/s, target 1,200/s)
  acked              815,142
  fills              145,265
  rejected            36,196   (8.377%)
  errors                   0
  unanswered               8   (0.002%)

steady state: 23 samples over 5m29s, after a 30s warmup
  heap            floor   50.4 MiB ->   54.0 MiB   trend +257.0 MiB/hour
  goroutines      floor         45 ->         45   trend -0/hour
  descriptors     floor         21 ->         21   trend -0/hour
  resting orders  floor        659 ->        660   trend -26/hour

VERDICT: no growth in heap, goroutines or descriptors over 5m29s of steady state.
```

Goroutines and descriptors did not move by one. The heap floor rose 3.6 MiB across
the window, which the harness accepts as pacing rather than growth; a run long enough
to argue about that has not been done.

**The number that mattered was measured against a control.** Multi-symbol added three
things to every order: a venue-wide client-id check, a session-scoped record of which
book each id went to, and a bounded fill memory for routing busts. All three are new
per-order state with eviction, which is the shape that survives tests and fails under
load. So the run was paired with an identical one against a single book:

| | one book | three books |
|---|---:|---:|
| sent | 108,080 | 108,076 |
| rejected | 9,084 (8.405%) | 9,021 (8.347%) |
| of which "unknown order" | 9,084 | 9,021 |
| errors | 0 | 0 |
| goroutines | 41, flat | 45, flat |

The refusal rate is the same to within noise, and all of it is the harness's own
optimistic bookkeeping — it asks to cancel orders that have already filled. **A cancel
routed to the wrong book would land in exactly that counter**, and it did not move.

Two things this run is not. It is 1,200 messages a second, not the 2,500 of §1c, and
not the 20,000 that found the orphaned-order defect. And it is five and a half
minutes: long enough for the harness to stop saying "inconclusive", far short of a
trading day.

### Under saturation, and the detector that had quietly stopped working

The run above kept the queue at zero. Pushed to 4,000 msg/s the venue saturates, and
saturation is where this project's one serious defect lived — the orphaned orders of
§1 appeared within thirty seconds at high rate and were invisible below about 4,000.
Multi-symbol adds a mutex and a map write to every order and another to every fill,
which is contention that a quiet queue never exercises.

Both configurations were driven to saturation. Goroutines and descriptors did not
move by one in either (43 and 19 on one book, 45 and 21 on three), no errors, no
orphans. The rejection rates are not comparable between them and no conclusion is
drawn from the difference: three books have three command queues, so the same client
rate is three times the buffer.

What the run did find was in the harness. **The believed-resting count came back at
2,180 against a hard ceiling of 800**, which is not a number the client's own
bookkeeping can produce.

The cause: a refused command was put back on the resting list whether it was a cancel
or an enter. A refused *cancel* leaves its order resting and belongs back on the
list. A refused *enter* never rested, and `act` had already added it optimistically,
so putting it back files the same id twice. Below saturation almost nothing is
refused and the drift is invisible; at 42% rejection it compounds every second.

That number is the orphan detector's baseline, and inflating it disables the detector
twice over: it shrinks the venue-minus-believed gap, and it raises the `believed/10`
threshold that gap is tested against. **The check gets less sensitive exactly under
the load it exists to watch.** Fixed by recording what was sent alongside when, so the
reject path can tell the two apart; the same saturated run now reports 797 against the
ceiling of 800.

Nothing was wrong with the engine. The instrument was wrong, and it was wrong in the
direction that hides failures rather than inventing them — see
[TESTING.md](TESTING.md) for why that is the direction worth checking first.

## 1e. Four hours, three books

The run §1d asked for. Four hours at 1,000 messages a second across three
instruments, durable, 8 connections over 4 accounts, every connection trading all
three books and every cancel naming its order by client id alone.

```
  sent            14,400,199   (1,000/s, target 1,000/s)
  acked           27,166,126
  fills            4,832,122
  rejected         1,207,872   (8.388%)
  errors                   0
  unanswered               8   (0.000%)

steady state: 240 samples over 3h58m59s, after a 1m0s warmup
  heap            floor   55.1 MiB ->   69.8 MiB   trend +4.3 MiB/hour
  goroutines      floor         45 ->         45   trend +0/hour
  descriptors     floor         21 ->         21   trend +0/hour
  resting orders  floor        636 ->        647   trend +0/hour

  participants believe            797 resting
  venue reports                   682 resting
```

**Fourteen million messages, zero errors, and the three load-insensitive signals did
not move.** Goroutines, descriptors and the book's own size are flat across four
hours — those are the figures a busy machine cannot distort, and they are the ones
this run was for. No orphans: participants believed more than the venue held, which
is the harmless direction, and the orphan check stayed quiet.

Storage: one log per book, 1.0 GB each and even across all three, which is its own
small confirmation that flow spread across the shards rather than favouring one.
Snapshots rewritten every 60s at ~165 KB, so replay stays bounded however large the
logs get. Latency p50 5ms, p99 250ms, on a machine also running a desktop.

### The heap floor, and what four hours could and could not settle

The harness reports growth: floor up 26.7%. The trend across three runs of the same
workload is the more useful number.

| run | trend |
|---|---:|
| 34 minutes | +50.9 MiB/hour |
| 2h12m (see below) | +6.0 MiB/hour |
| **4 hours** | **+4.3 MiB/hour** |

An order of magnitude of decay, which is caches filling and flattening rather than a
leak. The idle measurement makes it concrete: left running with no clients for three
and a half hours afterwards, the heap sat at **59.6 MiB against a 69.8 MiB
end-of-run floor**, and a profile diff showed `session.entered` down 8.3 MB and the
wire decode paths negative — memory handed back when the sessions closed. What
remains held is the fill memory, which is a deliberately retained 65,536-entry cache
and is [tested to stay bounded](../pkg/orderentry/fillmemory_test.go).

**What four hours could not settle.** The machine's fixed-work probe read 191ms at
the start and 47ms at the end — 307% apart, a busy evening against an idle night. GC
pacing follows CPU availability, so some of that 26.7% is the machine getting
quieter rather than the venue getting fatter, and this run cannot separate the two.
At +4.3 MiB/hour a trading day costs about 28 MiB and a week about 700 MiB: small,
not nothing, and the thing a day-long run on a quiet host would answer.

### Two false starts, and what they cost

The first attempt at this run was killed by the harness supervising it at 34 minutes.
The second reported growth over 2h12m and was worthless, for a reason worth writing
down: **the laptop was asleep for 172 of its 302 elapsed minutes.** macOS was putting
it into Maintenance Sleep on battery, roughly 9.3 minutes in every 10 from 17:55
onwards, and the venue only ran in the gaps.

It is not obvious from inside. `obsoak -duration` is measured with Go's monotonic
clock, which does not advance while the system sleeps — so a four-hour run silently
becomes a four-hour-*awake* run that never ends, and the throughput figures collapse
while every structural number stays perfectly healthy. The tell was the trade rate:
85/s while awake, 12/s averaged across the sleeping.

The fix is one word, and it is now in §3: run the harness under `caffeinate`. Note
that `caffeinate -s` only takes effect on AC power; on battery only `-i` applies, and
the battery becomes the binding constraint on how long a soak can last.

### What it turned up on the way

Neither of these is a defect in the engine, and both cost time to work out, so they
are here for whoever runs the harness next.

**obsoak cannot be re-run against a warm venue.** It logs in asking for sequence 0,
meaning "replay my whole stream". After a previous run has filled an account's
8,192-message ring, the venue tries to deliver all of it into a 1,024-deep outbound
queue, cannot, and sheds the connection — which is the documented behaviour for a
consumer that will not read. The client reports it as `write: broken pipe` and eight
connections that die on their first order. Start a fresh venue per run, or teach the
harness to resume.

**The gateway logged one instrument while serving three.** `obgw: serving BTC-USD`
at a venue running three books, because the startup line printed the single-symbol
config field. Cosmetic, and exactly the sort of thing that makes an operator distrust
everything else in the log. Fixed.

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

Multi-symbol needs a directory rather than two file paths, and `-symbols` on both
sides so every connection trades every book:

```sh
obgw  -addr :9000 -admin :9100 -accounts "$ACCTS" -rate 1e9 -burst 1e9 \
      -symbols BTC-USD,ETH-USD,SOL-USD -datadir /tmp/venue -checkpoint 60s
obsoak -addr :9000 -admin :9100 -symbols BTC-USD,ETH-USD,SOL-USD \
       -conns 8 -rate 1000 -duration 4h -warmup 60s -sample 60s
```

**On a laptop, wrap the client in `caffeinate`:**

```sh
caffeinate -ims obsoak ...
```

`-duration` is measured with Go's monotonic clock, which does not advance while the
system sleeps. Without this, a machine that dozes turns a four-hour run into a
four-hour-*awake* run that never ends, with throughput collapsing while every
structural figure stays healthy — see §1e. `-s` requires AC power; on battery only
`-i` applies and the battery itself bounds the run.

Add `-pprof` to `obgw` when the heap is the question. Metrics can tell you the heap
grew; only a profile can tell you what is holding it.

---

## 4. What is still unknown

Everything a longer run would tell you. The runs here are minutes; a trading day is
hours and a deployment is months. In particular:

- **Nothing has run for a day.** Four hours is the longest (§1e) and a trading day is
  six to eight. Fragmentation and the behaviour of a book churned for a full session
  are still unmeasured, and the heap floor's +4.3 MiB/hour is exactly the figure a
  day-long run on a quiet host would settle.
- **Hundreds of connections are untested.** These runs use 25, and §1e used 8. The gateway's
  goroutine-per-connection model is fine in principle and unproven past a few dozen.
- **One machine, one instrument, loopback.** No network, no NIC, no other tenant, no
  contention with anything a real deployment would have next to it.
- **The client is Go and shares the machine**, and so does everything else on it. See
  §1b: this is not a caveat, it is the thing that invalidated the first set of capacity
  figures. Run a soak on a host you control, and quote the probe value with any number
  you take from it.

A soak that finds nothing is evidence for the length of the soak, and for nothing
longer. That sentence is printed in the harness's own verdict for the same reason it
is written here.

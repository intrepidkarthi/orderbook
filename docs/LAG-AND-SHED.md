# Lag and Shed — Counting What the Venue Refused, and Timing What It Waits On

Status: **built** — a slice of
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M14, written before the code as this
repository does it · Author: Karthikeyan NG · Last updated: 2026-08-18

> **Built as specified**, with two departures from the text below, both stated here
> rather than left for a reader to find:
>
> - §3's surface change landed as exactly the four lines listed there. An earlier
>   attempt also exported a `Collector.CounterValue` reader for tests' convenience; it
>   was removed, because §3 rule 4 already says a test holds the handle or reads the
>   exposition, and a fifth line to save three in a test is how a frozen surface grows.
> - §9's figures were measured on this machine rather than inherited. The measurement
>   itself costs a median of **82 ns** and **0 allocations**, which is §9's ~80 ns
>   exactly. The append it wraps allocates **6** objects per command here rather than
>   the 9 §9 originally quoted — §9 now says 6 — and the timing adds **0** to that;
>   against a 1,282 ns append the overhead is ~5%, which is §9's number too.
>   [`BENCHMARKS.md`](BENCHMARKS.md) carries both tables.
> - §5.2's decorator is written out per method rather than using a `defer`. A deferred
>   call carrying an argument allocates under the race detector, and deliverable 2's
>   allocation assertion would then have had to be skipped on the build most likely to
>   be running in CI.
>
> **Six things review found after it shipped, all fixed in place, each recorded in the
> section that was wrong rather than only here.** Listed because a spec that quietly
> agrees with whatever the code ended up doing is not a spec:
>
> 1. **A paging threshold that could never fire** (§5.5, §5.6). The runbook pages when
>    `obgw_wal_sync_latency_ns` p99 exceeds one second and the shared histogram's top
>    bound was 250 ms, so a venue whose every fsync took two seconds reported a p99 of
>    250 ms and woke nobody. The bounds were widened rather than the alert trimmed, and
>    `TestSyncLatencyThresholdsAreReachable` keeps them that way. This is the defect
>    class this document is written against and it shipped inside it.
> 2. **A freeze test that froze nothing** (§4.2). `TestEveryReasonCodeHasAMetricName`
>    compared two hand-written tables in the same file; adding a `Reason*` constant to
>    `pkg/orderentry` passed. It now parses that package's source.
> 3. **Refusals taken before a session exists were counted nowhere** (§4.6), while a
>    permanently-zero `not_authorised` series sat on the page implying the opposite.
> 4. **§2.4 justified unlabelled histograms with a per-book signal that does not exist.**
>    Both metrics it named are venue-wide. The claim is gone and the gap is named in §10
>    and in the runbook.
> 5. **`/readyz` had started doing filesystem I/O** (§7.5), which is the outcome §7
>    exists to avoid, arrived at through the probe rather than the status code.
> 6. **Four smaller ones**: the `ErrNoResolver` mapping fix reached one call site of four
>    (§4.3); `obgw_shed_unreported_total` counted a clean drain as a shed (§4.4); the
>    snapshot-duration threshold's formula did not compute the quantity it thresholded
>    and its `_count` threshold never reached the runbook table (§6.4); and §14 still
>    declared a drill gap the implementation had closed.

> **File:line citations were reduced to file plus symbol.** This document was written
> before the code, so its line numbers pointed into the tree as it was then; the slice's
> own ~120 added lines moved most of them, and review caught one — §5.5's citation for
> `histogramBounds` — pointing at unrelated code. A wrong line number costs a reader more
> than an absent one, and nothing in this repository keeps them true.

> All thirteen sabotage runs in §13 were performed. Every one behaved as §13 says,
> including the two that sabotage a decision rather than a mechanism (run 1's nesting
> and run 8's readiness), and including run 6's asymmetry — the process-local timer
> passes the failure test and fails only the restart test — and run 12's, where
> reverting the mapping fix breaks deliverable 7 while deliverable 3 keeps passing.

> This document specifies four signals and nothing else. M14 lists sixteen metric
> families that exist and about a dozen that do not; §10 says which of the missing ones
> this slice deliberately leaves missing, and why each is a different piece of work
> rather than a smaller version of this one.

Companion documents:
- [`RUNBOOKS.md`](RUNBOOKS.md) §"Alert thresholds" — the table every signal here has to
  earn a row in. §11 is the diff to that table, and a signal that cannot be given a row
  does not ship.
- [`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M14 — the ticks and crosses this
  slice turns over, and the three degraded behaviours it lists as undefined. This one
  defines one of them (snapshot failure) and says why it does not define the other two.
- [`BENCHMARKS.md`](BENCHMARKS.md) — every threshold in §11 is derived from a number on
  that page rather than chosen. Where there is no lab number, §11 says so.
- [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) and [`LOG-ROTATION.md`](LOG-ROTATION.md)
  — the two slices that built the machinery §6 and §8 measure. Both were measured in a
  benchmark and neither reports anything from a running venue.
- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) §"Running continuously" — where
  the recovery-duration gauge's threshold comes from, and where the honest recovery
  point objective in §5.4 has to be written down.

---

## 1. Why this exists

M14's status line is one sentence and it is the whole diagnosis:

> this venue can tell you what it did, and cannot tell you how far behind it is.

The shape of the gap is sharper than "some metrics are missing". Every counter this
venue has is a count of **outcomes it produced**: orders accepted, trades printed,
rejections by reason, batches dropped. All of them come from the engine's event stream,
which is the one place the venue already had to write down what happened. Nothing counts
work the venue **refused before the stream existed**, and nothing times anything the
venue **waits on**.

Four consequences, each of which is a number an operator needs during an incident and
cannot get:

1. **A shed is invisible.** `matching.Runner.TryEnqueue` returns `ErrQueueFull`
   (`pkg/matching/engine_loop.go`); `cmd/obgw` maps it to a wire reject and sends it
   (`cmd/obgw/server.go`). It never becomes a `matching.EventRejected`, and
   `pkg/observability`'s collector counts reject reasons only off that event
   (`pkg/observability/metrics.go`). The rate gate's refusal
   (`pkg/gateway/gateway.go`) is invisible the same way. So the venue's queue-depth
   threshold warns you that clients are *about* to be refused, and there is no number
   anywhere that says how many were.
2. **The durability path is untimed.** The only `Observe` call sites in non-test code
   are the message-apply histogram (`cmd/obgw/admin.go`) and the client side of
   `cmd/obsoak`. The append and the fsync — the two operations that decide whether an
   acknowledged order survives — are measured in a benchmark on a laptop and nowhere
   else. §5.4 draws the consequence: the venue publishes a 20 ms recovery point
   objective it cannot verify.
3. **A failed checkpoint writes a log line.** `cmd/obgw/server.go` logs
   `obgw: %s checkpoint: %v` and continues. No gauge moves, no counter increments,
   `/readyz` is unaffected. M14 lists snapshot failure as one of three degraded
   behaviours that are undefined, and it is undefined in the specific sense that
   *nothing observable changes*.
4. **Recovery reports nothing.** Bounded recovery and log rotation were built to make a
   restart cheap and were measured in `BenchmarkRestartWithRetention`. A venue that has
   just restarted knows exactly how long its own recovery took and throws the number
   away.

None of these is a hard problem. That is the point: each is one measurement at one call
site, and the reason they have accumulated is that a metric is easy to add and a
*threshold* is not. Which brings us to the rule the rest of this document is written
against.

## 2. Rules this slice is written against

### 2.1 A metric nobody has a threshold for is a metric nobody looks at

Every signal in §4–§8 states three things or it does not ship: what value is **normal**,
what value means **trouble**, and what the operator **does about it**. Each lands as a
row in [`RUNBOOKS.md`](RUNBOOKS.md) §"Alert thresholds" (§11), and where the runbook
needs a whole procedure rather than a row, this slice writes it — and drills it, because
that page's opening claim is that every entry on it is drilled in CI.

One signal in this document (recovery duration, §8) has a threshold that is a policy
number rather than a measured one, and §8.3 argues for it explicitly rather than
pretending it fell out of a benchmark.

### 2.2 The instrumentation lives in `cmd/obgw`, not in `pkg/`

`pkg/wal` gets no timing hook. `pkg/matching` gets no shed counter. `pkg/gateway` gets
no denial counter. Every measurement in this slice is taken in `cmd/obgw`, at a seam the
gateway already owns — a `CommandLog` decorator (the shape `cmd/obgw/synclog.go`
already established), a loop the gateway runs, or a call site it makes.

Three reasons, in order of weight:

- A library that reaches for a metrics collector has chosen its embedder's monitoring
  stack. `pkg/wal` is embeddable without `pkg/observability` today and should stay that
  way; the alternative is a `Writer` that takes a metrics interface, which is a
  dependency in the exported surface for a facility only one embedder uses.
- It keeps the measurement out of critical sections that are hard to reason about.
  `Writer.append` runs entirely under `w.mu` (`pkg/wal/wal.go`), and that lock is
  load-bearing for rotation correctness; a timing hook inside it is a place a future
  change can deadlock. Timing the call from outside measures the same interval and
  cannot.
- `internal/apicheck` freezes `pkg/`. Instrumenting three packages to export four
  numbers would grow the frozen surface for observability's sake.

The one exception is `pkg/observability` itself, which gains one primitive (§3). That is
the metrics package; growing it is what it is for.

**Read wrong, this looks like a gap.** Somebody embedding `pkg/wal` in their own server
will find it exports no latency and conclude the instrumentation was forgotten. It was
declined. The seam they want is the same one `cmd/obgw` uses: wrap the `CommandLog`,
time the call, and feed whatever collector they already run.

### 2.3 Naming: `orderbook_` is about the venue, `obgw_` is about this process

The existing page already splits this way and nothing has written it down.
`orderbook_wal_bytes` is a fact about the venue's log on disk, readable by anything that
can see the directory. `obgw_message_apply_latency_ns` is a fact only this process knows,
because this process is what timed it.

So: **snapshot age is `orderbook_`** (it is read off the file's mtime and any tool with
the path can compute it); **snapshot duration, recovery duration, WAL append and sync
latency, and every refusal counter are `obgw_`** (this process timed or counted itself).

### 2.4 Counters and gauges carry the symbol; histograms do not

`observability.GaugeFamily` supports labels and `observability.Histogram` does not — the
exposition writer composes `name + "_bucket{le=…}"` (`pkg/observability/metrics.go`),
so there is nowhere for a second label to go without changing the format writer. This
slice does not change it.

The consequence is deliberate and worth stating rather than tolerating: per-book
questions are answered by a gauge or a counter, and per-device questions by a histogram.
Which book stopped checkpointing is `orderbook_snapshot_age_seconds{symbol}`. How long a
checkpoint takes is `obgw_snapshot_duration_ns`, merged, because a checkpoint's cost is
mostly the device's and every book on this node shares the device.

**Read wrong, this looks like an oversight.** A dashboard showing per-symbol prices next
to an unlabelled latency histogram reads like somebody forgot a label. What is actually
lost: one slow book inside a healthy set is invisible in the merged histogram.

**And nothing compensates for it, which this section originally claimed otherwise.**
The first version of this paragraph said the gap was covered because "a book whose log
is slow stalls its own matcher, and `orderbook_queue_depth` plus
`orderbook_last_event_sequence` catch that per book today." Both are venue-wide.
`orderbook_queue_depth` is registered as `sum()` across books
(`cmd/obgw/admin.go`, help text "summed across books") and
`orderbook_last_event_sequence` is one bare scalar
(`pkg/observability/metrics.go`, no labels). A three-book venue with one slow device
has, on this page, no series that says which book. The design decision stands on the
format writer alone; it does not stand on a compensating signal, and it was accepted
on one that does not exist. The gap is named in §10 and carries a row in
[`RUNBOOKS.md`](RUNBOOKS.md) next to both WAL histograms, so an operator reads it
beside the threshold rather than discovering it during the incident.

### 2.5 Nanoseconds for what this process timed, seconds for an age

`obgw_message_apply_latency_ns` set the precedent and this slice does not start a second
convention for latency. The one exception is snapshot age, which is `_seconds`: its
natural magnitude is minutes to hours, and `orderbook_snapshot_age_ns` reading
`4.2e+12` is a number nobody reads correctly at three in the morning.

## 3. The one new primitive: a counter that is a counter

Everything in §4 and §6 is a count of something that is not an engine event, and the
collector has no way to express one. It has counters, but they are private fields fed
from `OnEvents`; the only escape hatch used so far is `obgw_publisher_dropped_total`,
which is registered through `Collector.Gauge` and is therefore exported as
`# TYPE obgw_publisher_dropped_total gauge` while being named and used as a counter
(`cmd/obgw/admin.go`).

That works — `rate()` applies counter-reset correction to whatever series it is given,
regardless of the declared type — and it is still the wrong thing to copy four more
times. The `# TYPE` line is the only machine-readable statement this page makes about
what a series *is*, and a page where half the `_total`s claim to be gauges is one an
operator learns to ignore.

So `pkg/observability` gains one primitive:

```go
// Counter registers (or returns) a monotone counter series.
func (c *Collector) Counter(name, help string, labels ...Label) *Counter

type Counter struct{ /* one atomic.Int64 */ }
func (ctr *Counter) Add(n int64)
func (ctr *Counter) Value() int64
```

Four rules on it:

1. **Registration resolves the label set; increment does not.** `Counter` returns a
   handle the caller holds. The increment is one atomic add on a pointer the caller
   already has — no map, no hash, no lock, no allocation. This matters at exactly one
   place and it is the place that decides it: the shed counter is incremented while the
   venue is shedding, which is the moment it has least to spare. A `map[string]` lookup
   under an `RWMutex` per refusal — the shape `countReason` uses at
   `pkg/observability/metrics.go` — is defensible there, because that path runs on
   the matching goroutine at engine-event rate and the map is already warm. It is not
   what a flood path should pay.
2. **Same name plus same labels returns the same handle**, exactly as
   `Collector.Histogram(name)` does. Registering twice is idempotent, not a duplicate
   series.
3. **Rendered like the rejection family**: `# HELP` and `# TYPE …  counter` once per
   name, one line per label set, series sorted so a scrape is byte-stable. The existing
   `renderLabels` and sorting are reused unchanged.
4. **Not added to `Snapshot`.** `observability.Snapshot` is the fixed struct of engine
   counters; adding a `map[string]map[string]int64` to it would grow the frozen surface
   for a convenience tests do not need — a test holds the handle, or reads the
   exposition, which is what the drills already do.

**Surface change, stated so the regeneration is deliberate.** `internal/apicheck`'s
golden file gains four lines and loses none:

```
observability.(*Collector).Counter(name, help string, labels ...Label) *Counter
observability.(*Counter).Add(n int64)
observability.(*Counter).Value() int64
observability.Counter struct
```

Additive, so nothing that compiles today stops compiling. `internal/semcheck` must not
fire at all: no file under `pkg/matching` is touched by this slice, and if the semantics
fingerprint moves, the change is not the one this document specifies.

---

## 4. Signal 1 — refusals and drops

### 4.1 The two numbers, and which one an operator wants

A shed can be counted in two places and they are different numbers.

**At the queue** — inside `Runner.tryEnqueue`'s `default:` arm
(`pkg/matching/engine_loop.go`), where `ErrQueueFull` is produced. This counts every
refusal of every command kind from every caller: submits, cancels, the async replace and
reduce paths, mass cancels, and anything a future embedder enqueues. It is the number
that describes **the queue**.

**At the gateway** — where the error becomes a wire reject and is sent to the client
(the `sess.reject` call sites in `cmd/obgw/server.go`). This
counts refusals the venue **told somebody about**, in the vocabulary the client received,
including the ones that never reached a queue at all: rate-gate throttles, malformed
messages, duplicate client order ids, unauthorised requests.

**The operator wants the gateway number.** During the incident the queue-depth threshold
exists to warn about, the question is "how many clients did we refuse, and for what",
and the answer has to be in the same vocabulary the clients are complaining in. A
queue-level count cannot be reconciled against a client's own reject count — it includes
refusals nobody was told about and excludes refusals that never reached the queue — so
the first thing it produces in an incident is an argument about whose number is right.

There is a second reason and it is structural. Every client-visible refusal in this
gateway already passes through one function:

```go
func (sess *session) reject(clOrdID string, reason uint16)   // cmd/obgw/server.go
```

Fifty call sites, one funnel. Counting there is **complete by construction**: a new
ingress path cannot forget to count, because it cannot refuse without calling `reject`.
Counting at seven `if err != nil` sites is a list that goes stale the first time someone
adds an eighth — which is the exact failure mode `JOURNAL-COMPLETENESS.md` §4.2 spends a
section on for the command log.

### 4.2 The metric

```
obgw_refused_total{reason="…"}      counter
```

One increment per reject message the venue decided to send, labelled with the wire
reason. The label vocabulary is the frozen wire one (`pkg/orderentry/reason.go`),
rendered as lowercase snake case: `other`, `unknown_order`, `duplicate_clord`,
`too_small`, `too_large`, `price_band`, `self_trade`, `post_only_cross`,
`fok_cannot_fill`, `halted`, `throttled`, `overloaded`, `not_authorised`, `malformed`,
`shutting_down`, `invalid_quantity`, `too_soon`. Seventeen series, bounded, no
per-account dimension (§10).

Three rules:

1. **Count the decision, not the successful send.** `reject` can fail to encode
   (`server.go` returns early) and `send` drops the connection if the client's
   outbound queue is full. A refusal the venue could not deliver is still a
   refusal, and the client is *worse* off, not better. So the increment goes before the
   encode.
2. **The reason string is derived from the code by one mapping function**, frozen by a
   test that fails when a `Reason*` constant is added without a name. A metric label an
   operator greps for is part of the interface — the same argument
   `TestDrillTheCeilingRejectionNamesItself` (`cmd/obgw/drills_test.go`) already
   makes for the engine's reason strings.
3. **It is not the same population as `orderbook_rejections_total`, and the two must
   never be added.** `orderbook_rejections_total{reason}` counts `EventRejected` — what
   the *book* refused, in engine-error strings, for orders that reached the matcher.
   `obgw_refused_total{reason}` counts what the *gateway sent*, in wire codes. They
   overlap where the gateway relays an engine error onto the wire (the reduce and
   replace paths in `server.go` map an engine error through
   `orderentry.ReasonFor`), and neither is a total.

**Read wrong, this looks like double counting.** It is two counts of two populations
that intersect. The rule to write on the dashboard: `orderbook_rejections_total` answers
"what did the book refuse", `obgw_refused_total` answers "what did we tell clients". If
they diverge in the direction of the gateway, the venue is refusing work before the book
sees it, which is what overload looks like.

**One label increments on the matching goroutine**, and it is worth knowing which. The
cancel path resolves the client's order id inside a closure that runs on the matching
goroutine, and rejects from there when the order is unknown (`server.go`). So
`{reason="unknown_order"}` takes one atomic add on the hot goroutine. That is why §3
rule 1 exists.

### 4.3 One existing mapping bug this counter exposes

`cmd/obgw/server.go`:

```go
	if err != nil {
		sess.reject(m.ClOrdID, orderentry.ReasonOverloaded)
	}
```

Every other enqueue site distinguishes `ErrShuttingDown` from a full queue; this one does
not, so a cancel refused during a clean drain is reported to the client as **overload**.
Harmless today, in the sense that nothing counts it. The moment `obgw_refused_total`
exists, every planned restart adds to `{reason="overloaded"}` — the one series in this
document that pages.

**This slice fixes it**, and adds the assertion that was missing: a cancel refused while
the runner is shutting down reports `ReasonShuttingDown`. That changes a client-visible
byte in one case, from a code meaning "we are overloaded, back off" to one meaning "we
are going away" — which is strictly more accurate and is what a client needs to decide
whether to retry here or reconnect elsewhere. `TryEnqueueCancelBy` can also return
`ErrNoResolver`, which is a caller bug rather than either condition; it maps to `other`
and never to `overloaded`.

**Corrected after review: the fix landed as one function, not as a shape copied at each
site.** The first implementation wrote the `errors.Is` switch out at the cancel site and
left `TryReduceAsyncBy`, `TryReplaceAsyncBy` and `TryCancelAllAsync` with the original
two-branch version — so `ErrNoResolver` on any of those three still mapped to
`overloaded`, the one series in this family that pages on any increase. Latent rather
than live, since every resolver closure is non-nil today, and latent is how it would have
stayed until the day somebody passed nil and got an overload page for a nil pointer.
All four sites now call one `enqueueRefusalReason(err)`. A rule stated in a comment at
one of four call sites is not a rule.

### 4.4 The one refusal nobody is told about

`cmd/obgw/server.go`, cancel-on-disconnect:

```go
		if _, err := b.runner.TryCancelAllAsync(sess.account); err != nil {
			log.Printf("obgw: cancel-on-disconnect for %s on %s: %v", sess.account, b.symbol, err)
		}
```

There is no client to reject: the session is already gone. So this is the one shed in the
gateway that produces no wire message, and it is also the most consequential one. A
client disconnected, the venue undertook to pull its resting orders, the queue was full,
and the orders **stay in the book** — owned by a session that no longer exists, still
able to trade, cancellable only by an operator or a restart. That is the same class of
harm as `obgw_publisher_dropped_total`: not a delay, a loss.

It gets its own counter, because folding it into `obgw_refused_total` would put a
silent, unbounded-consequence drop in the same series as an ordinary throttle:

```
obgw_shed_unreported_total{op="cancel_on_disconnect"}      counter
```

One label value today. The label exists so the second such site — and there will be one —
lands as a series rather than as a new metric name.

**It counts a shed and not every error**, which the first implementation did not. The
runbook row says any increase means orders were left resting and must be reconciled by
hand; `ErrShuttingDown` also leaves them resting and is not that — the venue is closing
for the day and those orders come back after the restart. The `quit` check at the top of
`pullBookIfRequested` closes most of that window, but a session whose read loop returned
microseconds before `quit` closed still reaches the loop, and sending the on-call to
reconcile a book on a clean deploy is how a page-on-any-increase counter stops being
read. Everything else increments; the log line names the error either way.

### 4.5 Thresholds

- **`obgw_refused_total{reason="overloaded"}` — normal: zero.** Trouble: any increase.
  Action: check `orderbook_queue_depth / orderbook_queue_capacity`, which should have
  crossed 0.75 first. If it did, this is the documented overload path and the question is
  capacity. **If it did not**, the queue went from healthy to full inside one scrape
  interval, which means either a burst faster than 8,192 commands in 15 seconds or a
  matcher that stalled — check `orderbook_last_event_sequence` and go to
  [`RUNBOOKS.md`](RUNBOOKS.md) §"A stuck matching goroutine".
- **`obgw_refused_total{reason="throttled"}` — normal: non-zero at any venue with
  algorithmic clients.** The gate ships at 1,000 orders/s per account with a burst of
  200, and a market maker will hit it. Alert on a **10× step change against the trailing
  hour**, not on a level. A level threshold pages every time a client is onboarded, and
  an alert that fires on ordinary business is one that gets silenced before it ever fires
  on an attack.
- **`obgw_refused_total{reason="shutting_down"}` — normal: non-zero only during a
  planned drain.** Trouble: any increase when nobody is deploying. Something is stopping
  this process and it is not you.
- **`obgw_refused_total{reason="halted"}` — trouble: any increase while
  `orderbook_phase` reads 1 (open).** A refusal for a halted book against a book that
  says it is open means the disk stop-water mark took the venue cancel-only between
  scrapes ([`RUNBOOKS.md`](RUNBOOKS.md) §"The disk filled up").
- **`obgw_refused_total{reason="malformed"|"duplicate_clord"}` — no paging threshold,
  deliberately.** These are triage series: they name a client bug or a probe, they are
  read *after* something else pages, and a threshold on them would be a guess. Stated
  rather than dropped, because they cost nothing — they are series in a family the venue
  is already exporting.
- **`obgw_refused_total{reason="not_authorised"}` — no threshold, and it will read zero
  forever.** No command path sends this code: authorisation is decided at login, before
  a session exists. It stays registered because the vocabulary is frozen and a future
  per-command authorisation check would use it, and §4.6 is where the number an operator
  actually wants lives. This is written down because the series reading zero is exactly
  what made the gap in §4.6 invisible.
- **`obgw_shed_unreported_total` — normal: zero. Trouble: any increase, ever.** Action:
  the accounts that disconnected in that window may have orders resting that they asked
  to have pulled. Reconcile against the market-data feed, and cancel by hand. New runbook
  section (§11).

### 4.6 The refusals taken before a session exists

**Found by review, after the rest of this section shipped.** §4.1 argues that counting in
`session.reject` is complete by construction, and it is — over the population
`session.reject` sees. That population starts at login. A peer that fails to authenticate
never gets a session, so the refusal is written straight to the socket:

```go
	if !s.auth.Authenticate(req.Username, req.Password) {
		_ = wire.WritePacket(conn, wire.PacketLoginRejected, []byte{wire.RejectNotAuthorised})
		return
	}
```

Twenty-five rejected logins moved no counter anywhere on the page. What sat there
instead was `obgw_refused_total{reason="not_authorised"}` reading **zero**, registered at
startup by the rule in §4.2 that every reason gets a series whether or not it fires.

That is the worst shape a metric can have, and it is worth naming precisely because the
rest of this document is about adding metrics. **A zero on a page reads as evidence.**
During credential stuffing — the one incident where an operator most wants a refusal
count — the venue would refuse thousands of logins, every counter would stay flat, and a
series explicitly labelled `not_authorised` would sit at zero saying nobody is being
refused for authorisation. An absent metric makes an operator go and look. A metric that
reads healthy while the condition is happening stops them looking.

```
obgw_login_refused_total{reason="not_authorised"|"no_session"|"bad_sequence"}   counter
```

**A separate metric rather than three more series on `obgw_refused_total`**, because the
two speak different vocabularies. `obgw_refused_total`'s label is a wire reason code from
`pkg/orderentry`, sent in a `CmdReject` to a client that has logged in. These are soup
reject *bytes* (`internal/wire/soup.go`) sent in a `LoginRejected` to a peer that has
not. Mapping `'A'` onto `orderentry.ReasonNotAuthorised` would put two populations under
one label and make "how many command refusals did we send" wrong by however many people
mistyped a password. The two are disjoint by construction and the HELP strings on both
say so.

Counted through one `rejectLogin(conn, code)` funnel for §4.1's reason, and frozen the
same way: `TestEveryLoginRejectCodeHasAMetricName` parses `internal/wire/soup.go` and
fails if a reject code is added without a label.

**What is deliberately still uncounted**, so the HELP string is true: a pre-login peer
that sends something other than a `LoginRequest` is dropped without a reply. It gets no
information about the venue, and that includes not getting a series. A port scanner would
otherwise be indistinguishable on this page from a client with a protocol bug, and the
venue has nothing to say about either — `obgw_connections` already shows the sockets.

**Thresholds.** `not_authorised`: any sustained rate, or 10× the trailing hour — the same
step-change shape as `throttled`, because a venue with human operators has a background
rate of mistyped passwords and a level threshold would be silenced.
`no_session`/`bad_sequence`: any increase outside a restart window, where it is expected
and self-clearing; at other times a client is asking to resume from a sequence retention
has already dropped.

---

## 5. Signal 2 — WAL append and sync latency

### 5.1 The boundary, which is the whole question

"Append" and "sync" are separated by a group commit, and timing the wrong side of it
measures nothing useful.

What actually happens, from `pkg/wal/wal.go`:

- **`Writer.append`** takes `w.mu`, assigns a sequence, marshals the record to
  JSON, decides and possibly performs a **rotation**, writes a frame header and
  a payload into a `bufio.Writer`, and returns. **No disk write is guaranteed and no
  fsync happens.** Cost is CPU plus an occasional page-cache write, except on the append
  that rotates, which pays two fsyncs, a link, an unlink and a directory fsync —
  measured at 12.4 ms mean and 21.2 ms worst ([`BENCHMARKS.md`](BENCHMARKS.md)).
- **`Writer.Sync`** takes `w.mu`, flushes the buffer and fsyncs the file
  descriptor. Called from `cmd/obgw`'s `syncLoop` every 20 ms
  (`cmd/obgw/server.go`), covering however many records accumulated, or — under
  `-sync-every-command` — from inside each append by the `syncingLog` decorator
  (`cmd/obgw/synclog.go`).

So:

```
obgw_wal_append_latency_ns    histogram    one observation per journalled command
obgw_wal_sync_latency_ns      histogram    one observation per Sync call
```

**The rule: the append histogram never contains an fsync, and the sync histogram
contains nothing but one.** Everything else follows from it.

**Read wrong, this looks broken in `-sync-every-command` mode.** There, a command is not
durable until its own fsync completes, and the append histogram will read ~1.6 µs while
the venue is moving at 3.8 ms per command. That is not the metric lying; it is the
metric refusing to average two costs that have different causes and different fixes. The
append number is the venue's own work, the sync number is the storage device, and a
per-command venue's true cost is the sum of the two — which the page gives you, one
term each.

### 5.2 Where the decorator sits, and why it sits *inside* the syncing one

A `timedLog` decorator in `cmd/obgw`, structurally identical to `syncingLog`: one
`CommandLog` implementation forwarding all sixteen methods, timing each call.

The composition order is load-bearing:

```
default:                 Runner → timedLog{ *wal.Writer }
                                  syncLoop times Writer.Sync directly

-sync-every-command:     Runner → syncingLog{ timedLog{ *wal.Writer } }
                                  syncingLog times its own Sync call
```

`timedLog` goes **inside** `syncingLog`, never outside. Outside, its measurement of
`AppendSubmit` would include `syncingLog`'s fsync, and the append histogram in the mode
where durability matters most would be a copy of the sync histogram with a different
name.

The wrapping must be **exhaustive**. Sixteen methods, and a decorator that forgets one
silently stops timing an entire command kind — `AppendSetPhase` being the obvious
candidate, since it is the method that was added last and the one
`JOURNAL-COMPLETENESS.md` §4.2 exists because of. The assertion that catches this is not
"the count is greater than zero"; it is:

> after a tape containing every mutating command kind, `obgw_wal_append_latency_ns_count`
> equals `Writer.Seq()`.

The log's own sequence counts exactly the records that were appended. If the histogram
and the sequence disagree, a method is unwrapped. Self-checking, against the one number
that cannot be wrong.

### 5.3 The sync count is a liveness signal for a goroutine nothing else watches

`syncLoop` is a bare goroutine on a 20 ms ticker. If it stops — panics, is never started
by a future refactor of `Serve`, or is skipped by a configuration nobody re-read — the
venue keeps accepting and acknowledging orders and stops making them durable. **There is
no signal for that today.** `walFailed` latches on a sync that *fails*; a sync that never
*happens* moves nothing.

`obgw_wal_sync_latency_ns_count` advancing at ~50/s is that heartbeat, and it is the same
argument `orderbook_last_event_sequence` already makes for the matching goroutine: the
absence of work and the absence of a worker look identical in every rate metric.

**Read wrong, this looks like the venue doing work while idle.** The count advances at 50
per second on a completely quiet venue, because the ticker fires whether or not there is
anything buffered. That is the point — a heartbeat that stopped when the market went
quiet would be a heartbeat that stops exactly when nobody is watching.

Under `-sync-every-command` there is no loop to watch: `Serve` does not start it, the
fsync happens inline with the command, and the count advances once per command and not
at all on an idle venue. The alert is unaffected because it is a conjunction — flat
count *while* `orderbook_last_event_sequence` advances — and in that mode a command that
advanced the sequence must have synced. Only the "50 per second" figure changes, and
[`RUNBOOKS.md`](RUNBOOKS.md) says so beside it.

### 5.4 The honest recovery point objective

`PROTOCOL.md` and `pkg/wal`'s package comment state the durability window as the 20 ms
group commit. That is the *ticker interval*, not the window. The window is 20 ms **plus
the time the fsync itself takes**, because the syncLoop is a single goroutine: a sync
that takes 200 ms delays the next tick by 200 ms, and the venue's real recovery point
objective quietly becomes 220 ms with nothing saying so.

`obgw_wal_sync_latency_ns` is therefore not only a latency metric. Its p99 **is** the
variable half of the published RPO, and that sentence belongs in
[`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) next to the RPO figure, where today
there is a constant.

### 5.5 Buckets, and where they saturate

Both histograms use the collector's shared `histogramBounds`
(`pkg/observability/metrics.go`). This section originally said that range was 100 ns to
250 ms, that it covered the measured values "with room on both sides", and that only
§6.2 had a saturation problem. **The third claim was wrong and it was wrong about the
most important signal this slice adds.**

The measured range is not the range an alert has to cover. §5.6 pages when this
histogram's p99 goes above **one second**, and the top finite bound was 250 ms. A
bucketed quantile cannot report a value above its top bound: a thousand observations of
two seconds each yield `Quantile(0.99) = 250000000`, with every one of them in the
`+Inf` bucket and `le="250000000"` at zero. Prometheus's `histogram_quantile` saturates
identically — it returns the highest finite bound when the quantile lands in `+Inf` — so
the exported page was wrong in the same way the local reader was.

The consequence, spelled out because it is the failure this whole document is written
against: on a venue whose every fsync took two seconds, the real recovery point objective
would have been ~2.02 s, the 100 ms warn tier would have fired, **the page tier could
never have fired**, and an operator opening the dashboard during the incident would have
read a p99 of 250 ms and concluded the durability window was a quarter of a second. A
metric that reads healthy while the thing it measures is the thing that is slow is worse
than no metric, because it is trusted.

**So the range was extended rather than the alert trimmed**: `histogramBounds` gains
`1_000_000_000` and `5_000_000_000`. Two more bounds on a shared type, which §10
originally ruled out — that entry is corrected there. The cost is two `atomic.Int64` per
histogram and two exposition lines; `sort.Search` over nineteen bounds instead of
seventeen is the same number of comparisons in practice, and
`TestAppendTimingAllocatesNothing` and `BenchmarkTimingOverhead` both still hold.
Five seconds is where an fsync stops being slow and starts being a failed device.

`TestSyncLatencyThresholdsAreReachable` is what stops this coming back. It asserts the
reading an operator would take at each tier under the condition that tier exists for: a
4 ms fsync must not cross the warn tier, a 200 ms fsync must cross the warn tier and not
the page tier, a 2 s fsync must cross the page tier, and the exposition must carry a
bound at the page threshold so `histogram_quantile` can reach it too.

**It still saturates, at 5 s, and that is now written next to the threshold** in
[`RUNBOOKS.md`](RUNBOOKS.md) rather than left to be discovered: a p99 reading exactly
`5000000000` means "at least that", and `_sum`/`_count` is exact at any magnitude.
§6.2 has the same problem at the same place and says so there.

### 5.6 Thresholds

- **`obgw_wal_append_latency_ns` p99 — normal: ~10 µs.** Trouble: **p99 > 1 ms**
  sustained over five minutes. Action: the append path is CPU and page cache; a p99 two
  orders of magnitude above the measured one means the page cache is not absorbing writes
  — check `orderbook_wal_disk_free_bytes` and the device's own queue.
  Why 1 ms and not tighter: a rotation costs 12.4 ms *on the appending goroutine*, and at
  the shipped 128 MiB segments and 2,500 msg/s that is one append in ~610,000, which
  cannot move a p99. At small segments it can, and that is a configuration
  [`BENCHMARKS.md`](BENCHMARKS.md) already calls a test fixture rather than a deployment.
  Alert on p99 rather than max, for the same reason.
- **`obgw_wal_sync_latency_ns` p99 — normal: single-digit milliseconds.** Trouble:
  **p99 > 100 ms**; page at > 1 s. Both tiers are representable since §5.5's bucket
  change; before it the page tier could not fire at all. Action: this is the storage
  device, and the number is the venue's recovery point objective (§5.4). Above 100 ms,
  tell clients the window widened; above 1 s, the next fsync failure is a `walFailed`
  latch and [`RUNBOOKS.md`](RUNBOOKS.md) §"The disk filled up" is already the procedure.
  **Read `_sum`/`_count` whenever the p99 reads exactly `5000000000`**: that is the top
  bound and it means "at least five seconds", not "five seconds".
- **`obgw_wal_sync_latency_ns_count` — trouble: not advancing for 5 s while
  `orderbook_last_event_sequence` is advancing.** Action: the group-commit loop is gone
  and the venue is acknowledging orders it is not making durable. Take the node out of
  rotation and restart it. New runbook section (§11). This is the highest-severity row
  this slice adds and it is the one nobody would have asked for.

---

## 6. Signal 3 — snapshot age, duration and failure

### 6.1 Age comes from the file, not from a timer

```
orderbook_snapshot_age_seconds{symbol="…"}     gauge family
```

Computed at scrape time as `time.Now().Sub(os.Stat(snapPath).ModTime())`, registered
alongside the existing per-symbol WAL gauges in `registerGauges`
(`cmd/obgw/admin.go`), which already `wal.Stat` twice per scrape per book.

Reading the **file** rather than a process-local "last successful checkpoint" timestamp
is the decision, and there are three reasons:

1. It cannot claim a snapshot is fresh when the artefact on disk is old. A process timer
   records what the code believed; the mtime records what happened.
2. **It survives a restart.** A venue that has just recovered reports the true age of the
   base it recovered from, immediately, before its first checkpoint tick. A timer seeded
   at process start reports zero — the freshest possible reading — for a venue that may
   be recovering from a snapshot two days old. That is the reading an operator would
   most like to have and the one a timer cannot give.
3. `matching.EngineSnapshot` carries no timestamp (`pkg/matching/snapshot.go`) and
   this slice does not add one: the snapshot is in the digest's blast radius, and
   `SEMANTICS-VERSION.md` is what a field there costs.

Three values, and the third is the interesting one:

| Situation | Reading |
|---|---|
| `SnapshotPath` unset — this venue does not checkpoint | `NaN` |
| Configured, no file yet | seconds since process start |
| File present | seconds since its mtime |

**Read wrong, `NaN` looks like a broken metric.** It means "this venue was not asked to
checkpoint", and it is `NaN` rather than zero for the reason the best-bid gauge is
(`cmd/obgw/admin.go`): zero is a legal age and a monitoring system cannot tell a
missing snapshot from a snapshot written this instant. `NaN` also never satisfies a `>`
comparison, so a venue with checkpointing deliberately off does not page — while a venue
that was *asked* to checkpoint and never has counts from process start and does.

**Read wrong, a negative age looks like arithmetic.** mtime is wall clock and so is
`time.Now()`, so a host whose clock jumps backwards reports a negative age. It is not
clamped to zero. Clamping would report the freshest possible snapshot at exactly the
moment the host's clock is wrong, and M14 records that this venue has no clock-offset
signal of any kind (§10) — a negative snapshot age is the only one it will have. It is
not a substitute for one and is not claimed as one.

The lesser wrinkle, stated so nobody chases it: filesystems with one-second mtime
granularity make a freshly-written snapshot read as up to a second old, and NFS makes it
worse. Irrelevant against a 90-second threshold; confusing in a test that asserts an age
below one second, so the acceptance test in §12 asserts below five.

### 6.2 Duration is the write, not the pause

```
obgw_snapshot_duration_ns     histogram    one observation per successful WriteSnapshot
```

Observed in `checkpointLoop` around `wal.WriteSnapshot` (`cmd/obgw/server.go`), on
the checkpoint goroutine.

A checkpoint has two halves that cost differently and hurt differently:

- **`b.runner.Checkpoint()`** serialises the book **on the matching goroutine**.
  It is the half that pauses trading — ~0.3 ms for a 5,000-order book
  ([`BENCHMARKS.md`](BENCHMARKS.md)).
- **`wal.WriteSnapshot`** encodes and writes it with a
  temp-fsync-rename-fsync sequence, on the checkpoint goroutine. It is the half that
  touches the disk — 10.9 ms at 1 K orders, 82.4 ms at 100 K.

**This metric times the second half only, and the first half is deliberately left
untimed.** The reason is not laziness, it is that timing it from here would produce a
number that means something else. `Runner.Checkpoint()` is a synchronous send through the
command queue (`pkg/matching/engine_loop.go`), so a stopwatch around it from the
checkpoint goroutine measures **queue wait plus work**. Under load the wait dominates,
and an operator reading a rising `obgw_snapshot_duration_ns` would conclude their book
had grown when in fact their queue had. Measuring the pause honestly means timing it
inside `dispatch`, on the matching goroutine, which needs a hook into `pkg/matching` that
§2.2 declines to add.

What covers the pause in the meantime, so this is a deferral and not a hole: the pause is
already visible where it actually hurts, in `obgw_message_apply_latency_ns` — a client
message that arrives during a checkpoint waits behind it, and that histogram measures
what the client experienced. The lab number is in `BENCHMARKS.md` per book size. What is
missing is the pause in isolation on a live venue, and §10 lists it.

**Where this saturates, and what to read instead.** The shared buckets now top out at
5 s (§5.5), so a book big enough to spend longer than that on one write reads exactly
`5000000000` and means "at least". The exposition also emits `_sum` and `_count`, which
are exact at any magnitude — so the threshold in §6.4 is written against those rather
than a quantile. A quantile pinned at the top bucket is not a number.

### 6.3 Failure is counted, not just logged

```
obgw_snapshot_failures_total{symbol="…"}      counter
```

Incremented where the log line already is (`cmd/obgw/server.go`). One increment per
failed `WriteSnapshot`.

Age and failures answer different questions and the venue needs both. Age says *the
recovery base is stale*; the failure counter says *and here is why it is stale*. A stale
age with a flat failure counter is a checkpoint loop that is not running at all — a
different fault with a different fix from a checkpoint loop that is running and failing.

### 6.4 Thresholds

- **`orderbook_snapshot_age_seconds` — normal: below the checkpoint interval (30 s at
  the shipped default).** Trouble: **> 3× the interval (90 s)**; page at 10×.
  Why 3: one missed tick is a write that overran its interval, which is a slow disk and
  not yet an incident; three consecutive misses is not noise. Action: new runbook section
  (§11) — the venue is still trading and its restart is getting more expensive every
  minute.
- **`orderbook_snapshot_age_seconds` reading `NaN` unexpectedly — trouble.** The venue
  was started without `-snapshot`. Every restart replays the entire log.
- **`orderbook_snapshot_age_seconds` reading negative — trouble.** Time is wrong
  somewhere: either this host's clock moved backwards, or the snapshot is on a network
  filesystem whose SERVER stamps the mtime and whose clock is the skewed one. The two
  need opposite responses, so [`RUNBOOKS.md`](RUNBOOKS.md) §"A negative snapshot age" is
  a procedure rather than a row — §2.1 requires every signal to say what the operator
  does, and the first version of this bullet said only what it meant. Time-in-force
  deadlines and the audit trail read the same clock.
- **`obgw_snapshot_failures_total` — normal: zero. Trouble: any increase.** Action: the
  previous snapshot is still in force and retention is deliberately still running against
  it (`server.go`), so nothing has been destroyed. Find out why the write failed —
  the usual answer is space, and `orderbook_wal_disk_free_bytes` says so.
- **`rate(obgw_snapshot_duration_ns_sum[5m]) / 1e9` — trouble: > 0.25.** The fraction of
  wall time this process spends writing snapshots, across every book, computed directly.
  Action: at 1.0 the loop never finishes a cycle and the configured interval is fiction.
  The fixes are a longer interval (which lengthens recovery) or a smaller book. Note the
  arithmetic that makes this real rather than theoretical: 82.4 ms at 100 K orders means
  eight books of that size already consume 2.2% of a 30-second tick, and the cost is
  O(book).
  **Corrected after review: this was written as `_sum`/`_count` "> 25% of the interval,
  summed across books".** That formula does not compute that quantity. `_sum`/`_count` is
  a per-WRITE mean and has no book count in it, so eight books at 82 ms each — 660 ms of
  a 30 s tick — read as 82,000,000 and an operator following the row literally concludes
  everything is fine at any book count. The multiplication was named in the prose and
  missing from the arithmetic. A rate over `_sum` has it built in.
- **`obgw_snapshot_duration_ns_count` — trouble: not advancing while
  `obgw_snapshot_failures_total` is also flat, and a snapshot path is configured.** The
  checkpoint loop is not running, as opposed to running and failing. Same shape of signal
  as §5.3, same reasoning. It is in the [`RUNBOOKS.md`](RUNBOOKS.md) table; the first
  version of §11 defined this threshold here and never carried it across, which is
  exactly the omission §2.1 exists to prevent — a signal with a threshold nobody wrote
  down is a signal nobody alerts on.

---

## 7. Snapshot failure and `/readyz` — the decision

M14 lists snapshot failure as one of three undefined degraded behaviours. The obvious
move is to copy the WAL-failure path, which fails readiness and latches
(`cmd/obgw/admin.go`). **This slice deliberately does not.**

### 7.1 The decision

**A venue that cannot checkpoint keeps trading, keeps reporting ready, and says so in
the readiness body.**

`/readyz` returns **200** with a body that names the degradation:

```
ready: queue 0/8192, event sequence 41552 (degraded: BTC-USD checkpoint 412s old, 13 failures)
```

The degraded clause appears when any book's snapshot age exceeds 3× the checkpoint
interval — the same threshold §6.4 pages on, so the page and the probe agree. The status
code does not change.

### 7.2 Why, against the obvious symmetry

The WAL-failure path fails readiness because **a command acknowledged now is not durable
now**. It is a correctness failure, it is happening per command, and every second of
continued trading adds to the set of orders the venue has lied about. Latching is right
because clearing it means somebody decided the disk is fixed.

A failed checkpoint costs none of that. Every acknowledged command is still in the log;
the previous snapshot is still valid; recovery still produces the correct book. What it
costs is **recovery time later** — the tail to replay grows, and with `-wal-retain`
configured, the retained set eventually cannot be joined to the snapshot at all and
recovery refuses to start (`LOG-ROTATION.md` §4.4).

Failing readiness for that is the wrong trade, and the reason is what readiness *means*
at a venue. Readiness takes the node out of rotation. At a stateless service that means
traffic moves elsewhere; at a venue it means **this book stops receiving orders while it
holds every position that is already in it**. Clients cannot enter, and — because the
orchestrator may then decide to restart the node — the venue is pushed toward exactly
the restart whose cost the failed checkpoint has been inflating. A checkpoint failure
would become a trading outage, then a slow restart, then a longer outage.

### 7.3 What the terminal case already is

The failure is not open-ended, because it degrades into a behaviour that is already
defined and already drilled:

1. Checkpoints stop landing. `orderbook_snapshot_age_seconds` climbs;
   `obgw_snapshot_failures_total` counts.
2. Retention's predicate needs a **verified snapshot** covering the segments it would
   delete (`server.go`, `LOG-ROTATION.md` §5.1), so with the snapshot frozen,
   retention stops deleting.
3. The log grows. `orderbook_wal_bytes` climbs, `orderbook_wal_disk_free_bytes` falls.
4. At the low-water mark the venue warns and forces retention; at the stop-water mark
   every book goes **cancel-only** (`server.go`), which is a defined,
   client-visible, drilled state with a runbook entry.

So the venue does have a terminal behaviour for "cannot checkpoint": it is the disk
behaviour, arrived at by the disk path. This slice's job is to make the hours *before*
that visible, which is precisely what was missing.

### 7.4 What a venue that cannot checkpoint but can still trade reports

All four, together, and none of them alone:

- `orderbook_snapshot_age_seconds{symbol}` climbing — **the signal**.
- `obgw_snapshot_failures_total{symbol}` incrementing — **the reason**.
- `/readyz` 200 with the degraded clause — **what a human sees when they check by hand**.
- The existing `obgw: %s checkpoint: %v` log line — **the error itself**.

### 7.5 The degraded clause costs the probe nothing, and it took a correction to keep it

`cmd/obgw/admin.go`'s own contract for `/readyz` is that it is "derived from signals that
cost nothing — queue occupancy, and whether the event sequence advances while work is
waiting". The first implementation of the clause broke it: `checkpointDegradation` called
`snapshotAgeSeconds`, which `os.Stat`s the snapshot file, once per book per probe.
`/readyz` had done no filesystem I/O at all before that.

A snapshot on a mount that hangs — NFS, a stalled device, a disk that stops answering —
makes `os.Stat` block. The probe never answers, the orchestrator times it out, marks the
pod not-ready and restarts it. That is precisely the outcome §7.2 argues against, arrived
at through the probe instead of through the status code: a snapshot-storage problem takes
a book holding positions out of rotation and invites the restart the failed checkpoint has
been making more expensive.

So each book caches the snapshot's **mtime** (`symbolBook.snapMTime`), refreshed where I/O
is already happening — once in `NewServer`, and on every checkpoint tick — and the probe
computes the age from it with no syscall. `/metrics` still stats every scrape and updates
the cache; a scrape is not a probe and can afford it.

Caching the mtime rather than the age is what makes it safe to let go stale. The age is
computed at read time, so a checkpoint loop that dies stops refreshing the cache and the
age goes on climbing exactly as it should. A cached age would freeze at its last value and
report a healthy venue forever — which would have been the same class of defect as §5.5's.
Seeding in `NewServer` is what keeps the load-bearing property of §6.1: a venue recovered
onto a base backdated two hours reports 7200 seconds on its **first** probe, before any
scrape. `TestReadinessReadsNoFilesystem` proves it by making the file unstatable and
asserting the answer is still the file's real age.

**Read wrong, this looks like a missing check.** An operator who knows a WAL failure
fails readiness will read a snapshot failure passing readiness as an oversight. It is the
opposite: a deliberate asymmetry, from a distinction worth learning — a WAL failure means
acknowledged commands are not durable **now**; a snapshot failure means recovery will be
slow **later**. Only one of those is a reason to stop trading, and it is not this one.

**And the other direction, stated so nobody proposes it later:** no, a checkpoint failure
should not take the venue cancel-only after N failures. That trades a
recovery-time problem for a certain trading outage, on a timer, unattended. The disk path
already does it when the disk actually runs out, which is the condition that justifies it.

---

## 8. Signal 4 — recovery duration

### 8.1 The metric and its boundary

```
obgw_recovery_duration_ns{symbol="…"}     gauge family
```

Set once per book during `NewServer`, never changing for the life of the process. It
measures **everything between "we have a configuration" and "this book can run"**:

- `wal.RecoverWithOptions` — read the snapshot, walk and verify the retained log, apply
  the tail (`cmd/obgw/server.go`).
- `reg.Adopt(recovered.RestingOrders())` — rebuild the session naming index.
- `feed.Adopt(...)` — seed the market-data feed.
- `wal.OpenWith` — open the log for appending.

**Not just `wal.Recover`.** The operator's question is "how long was my venue down", not
"how long did the WAL package take", and the two Adopts are O(book) — at 100 K orders
they are not a rounding error on a 174 ms recovery. Measuring the narrow interval would
produce a number that is reliably smaller than the truth, which is the worst kind of
wrong for a figure that feeds an RTO.

It excludes process start, flag parsing, and listener bind. Those are constant and are
not what grows.

**Books recover serially**, in the `cfg.Symbols` loop. So the venue-wide recovery cost is
`sum(obgw_recovery_duration_ns)` and that sum is legitimate rather than an approximation
— which is the reason this is a labelled gauge family and not a per-book metric plus a
separate venue total. Two names for one number is how they drift.

`NaN` when no WAL is configured, by §6.1's rule: a venue with no log did not recover, and
zero would read as "recovered instantly".

### 8.2 Why a gauge and not a histogram

It is observed once per process. A histogram of one observation reports a quantile that
is the observation, in a bucket that rounds it, with a `_count` of 1. A gauge is the
honest shape: this is a fact about this process, and it changes when the process is
replaced.

**Read wrong, this looks like a stuck gauge.** It never moves. It is not supposed to; it
is a property of the last recovery, and the way to read it over time is
`max_over_time(obgw_recovery_duration_ns[30d])` across restarts, which is where the
signal actually is (§8.3).

### 8.3 The threshold, argued rather than derived

This is the one signal in this document with no measured normal, because "normal" is
whatever this deployment's book size and retained log make it. What it has instead is a
**policy** threshold and a **trend** threshold, and the second is the one that matters.

- **Policy: > 30 s warns, > 120 s pages.** These are the numbers
  [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) has to state as the RTO anyway,
  and a deployment with a different RTO overrides both. A default of "no threshold"
  would leave the metric unwatched, which §2.1 forbids.
- **Trend: > 2× the previous restart.** This is the real alert, and it is the one that
  catches the failure [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §1 describes exactly:
  *"unusual in that it is invisible until the worst moment. A venue that never restarts
  never notices. The first restart after a long uptime is when the arithmetic arrives,
  and by then the operator is already in an incident."* A venue whose recovery went from
  2 s to 40 s over a month is one restart from that incident, and the only way to see it
  is to compare restarts.

Action for both: read it against `orderbook_wal_bytes` (the retained set, which is what
recovery reads) and `orderbook_resting_orders` (which drives the snapshot restore). If
bytes grew, `-wal-retain` is unset or too high. If orders grew, the snapshot half grew
and retention will not help — `LOG-ROTATION.md` §8 says the snapshot term is unbounded
and this is where a deployment finds that out before a restart does.

---

## 9. What each measurement costs, and what stays untimed

The matching hot path is zero-allocation and `TestZeroAllocHotPath`
(`pkg/matching/gate_test.go`) proves it by running `BenchmarkEngine_MatchInto` and
`BenchmarkEngine_CancelReplaceInto` and asserting 0 allocs/op.

**Nothing in this slice adds an instruction to any path that test exercises.** Those
benchmarks drive `Engine` directly, with no `Runner`, no `CommandLog` and no gateway.
That is a property to keep rather than a coincidence to rely on, so §12 asserts it
directly as well: the benchmark's allocation count must be unchanged.

Per measurement, on the path it is taken:

| Measurement | Goroutine | Frequency | Cost | Against |
|---|---|---|---|---|
| `obgw_refused_total` | connection read loop (and the matching goroutine for `unknown_order`, §4.2) | one per reject | one atomic add on a pre-resolved handle; 0 allocs | a wire encode and a channel send already on that path |
| `obgw_shed_unreported_total` | disconnect handler | one per dropped mass cancel | one atomic add | a socket teardown |
| `obgw_wal_append_latency_ns` | **matching goroutine** | one per journalled command | 2× `time.Now()` + one `Observe` ≈ **80 ns**, 0 allocs | append p50 **1,625 ns** → ~5%; whole `Runner.Process`+sink+WAL path 18,260 ns → **0.4%** |
| `obgw_wal_sync_latency_ns` | syncLoop (or the matching goroutine under `-sync-every-command`) | 50/s (or one per command) | ~80 ns | an fsync of ~3.8 ms → 0.002% |
| `obgw_snapshot_duration_ns` | checkpoint goroutine | one per book per 30 s | ~80 ns | a 10–82 ms write |
| `obgw_snapshot_failures_total` | checkpoint goroutine | one per failure | one atomic add | a `log.Printf` already there |
| `orderbook_snapshot_age_seconds` | scrape goroutine | one `os.Stat` per book per scrape | a stat | two `wal.Stat` calls already per book per scrape |
| the `/readyz` degraded clause | probe goroutine | one per probe per book | **no syscall** — an atomic load of a cached mtime (§7.5) | an atomic and two queue counters already there |
| `obgw_recovery_duration_ns` | startup | once per book, ever | nothing | — |
| `obgw_login_refused_total` | handshake goroutine | one per refused login | one map lookup and one atomic add | a socket write and a TCP teardown |

Three notes on the 5%, because it is the only number here big enough to argue about:

- It is 5% of the **append**, not of the venue. The append is 9% of the group-committed
  write path (1,625 ns of 18,260 ns), so the venue-level cost is under half a percent,
  and the durable path's cost is dominated by an fsync this measurement does not touch.
- `Observe` allocates nothing: `time.Time` and `time.Duration` are values, nothing is
  boxed into an interface, and the bucket search is a `sort.Search` over 19 constants
  followed by three atomic adds. `pkg/wal`'s append already allocates **6** objects and
  632 B per command for the JSON encode ([`BENCHMARKS.md`](BENCHMARKS.md); this document
  said 9 before it was measured on this machine), and the timing adds zero to that.
  §12's assertion is relative rather than absolute — `metered <= plain + 0.5` — so it
  holds whatever the encoder's own count happens to be, which is the right shape for an
  assertion about *this* measurement's cost.
- **Sampling was rejected.** One-in-N would cut the cost and would also lose the one
  event this histogram exists to catch in production: a rotation costs 12.4 ms inside a
  single append, once every four minutes at the shipped segment size, and a 1-in-1,000
  sample sees it approximately never. The tail is the whole reason to measure the append
  at all.

**What is deliberately left untimed**, so this stays true:

- `Engine.Match`, `Engine.CancelReplaceInto` and everything else inside `pkg/matching`.
  No counter, no clock, no hook. The client-visible proxy is
  `obgw_message_apply_latency_ns`, which already contains the engine's per-command work
  plus the queue wait, measured where a client actually feels it.
- `Runner.tryEnqueue`'s shed branch (§4.1).
- The checkpoint's matching-goroutine pause (§6.2).
- `pkg/wal`'s internals: no timing inside `w.mu` (§2.2).
- The rate gate's own refusal count inside `pkg/gateway` (§2.2); it is counted where it
  becomes a reject.

---

## 10. What this deliberately does not do

- **No replica lag.** `obgw` has no replication topology — the primary/follower pair
  exists only in `examples/replication`, where the number is readable as
  `primary.LogSeq()` against `follower.Applied()`. A lag gauge exported from a process
  with no follower reads zero forever, and a metric that is always green is worse than an
  absent one. It belongs with roadmap item 2 (the sequence trio), which is where the
  primary's applied and synced sequences get exported at all.
- **No commit lag.** Same dependency, and a harder one: "commit" needs a durable
  sequence and an applied sequence that mean the same thing on both sides of a failover,
  which is M2 contract work. Measuring it before naming it produces a number two people
  will read differently during a failover.
- **No feed lag.** Subscriber eviction is defined and drilled; the *lag* of a subscriber
  that has not yet been evicted needs a per-subscriber position exported from
  `marketdata.Feed`, which is a per-connection cardinality question this page has not had
  to answer before.
- **No clock offset.** A process cannot measure its own clock's error. It needs an
  external reference — NTP peer statistics or a PTP daemon — and that is a deployment
  integration, not a metric. §6.1's negative snapshot age is the nearest thing this venue
  will have and is not offered as a substitute.
- **No GC pause histogram.** `runtime/metrics` makes it nearly free and it still does not
  belong here: its threshold is derived from the apply-latency budget rather than from
  itself, and this slice does not set that budget. It goes in the slice that does.
- **No structured logging, no trace ids, no order/session correlation.** Every emission
  in this repository is `log.Printf`. Converting them is a change to every operational
  message the venue produces and to every runbook that quotes one — a slice of its own,
  and one whose first question is whether the runbook's quoted strings become a
  compatibility surface.
- **No machine-readable alert rules.** A rules file that has never been loaded by a
  Prometheus is a file that is wrong in ways nobody has found. [`RUNBOOKS.md`](RUNBOOKS.md)
  §"Alert thresholds" stays the source of truth, and it stays a table a human reads.
- **No incident annotations, no audit-log retention.** The second is not a smaller
  version of WAL retention: the WAL is the command journal, and a security event log is a
  different artefact with a different retention policy and a different reader.
- **No per-account labels on anything.** A venue with ten thousand accounts and a
  seventeen-value reason vocabulary would produce 170,000 series from one metric. Per-account
  behaviour is `pkg/surveillance`'s job, and it is already there.
- **No per-symbol histograms** (§2.4) and **no change to `observability.Snapshot`** (§3).
  This entry also said "no new bucket bounds"; §5.5 records why that was reversed — two
  bounds were added because the paging threshold on the sync histogram was above the top
  bucket and therefore dead.
- **No per-book WAL latency, and nothing else localises a slow log.** §2.4 states the
  format-writer reason. What §2.4 originally also claimed — that `orderbook_queue_depth`
  and `orderbook_last_event_sequence` catch it per book — is false: both are venue-wide.
  At a three-book venue with one slow device there is no series that says which book. The
  honest answer is the device, and [`RUNBOOKS.md`](RUNBOOKS.md) says so beside the
  threshold. Fixing it properly means label support on `observability.Histogram`, which is
  a change to the exposition writer and belongs in the slice that makes it.
- **`obgw_publisher_dropped_total` is not re-typed.** It is a counter registered through
  `Gauge` and it stays that way in this slice. Re-typing an existing series is a change
  to a metric this slice did not add, for a cosmetic gain, and it belongs in its own
  commit where somebody can think about whose dashboard it touches. Named here so the
  inconsistency is on record rather than discovered.
- **No new wire message and no new reason code.** §4.3 changes which existing code one
  path sends, and adds none.
- **No flag to turn the measurement off.** A knob nobody sets is a configuration matrix
  nobody tests. §14 states the risk that makes this the wrong call and what would reveal
  it.

---

## 11. The alert-threshold rows

These are the rows added to [`RUNBOOKS.md`](RUNBOOKS.md) §"Alert thresholds". The table's
existing rows are untouched.

| Signal | Threshold | Why |
|---|---|---|
| `obgw_refused_total{reason="overloaded"}` | any increase | The venue shed client orders. Queue depth should have crossed 0.75 first; if it did not, the burst outran the scrape or the matcher stalled |
| `obgw_refused_total{reason="throttled"}` | 10× the trailing hour | Non-zero is normal at 1,000/s per account. The step change is the signal; a level threshold pages on onboarding |
| `obgw_refused_total{reason="shutting_down"}` | any increase with no deployment in progress | Something is stopping this process and it is not you |
| `obgw_refused_total{reason="halted"}` | any increase while `orderbook_phase` is 1 | A refusal for a halted book against a book reporting open: the stop-water mark went cancel-only between scrapes |
| `obgw_shed_unreported_total` | **any increase** | A disconnected client's orders were left resting after the venue undertook to pull them. Nobody was told, because there was nobody to tell |
| `obgw_wal_append_latency_ns` p99 | > 1 ms sustained 5 min | Normal is ~10 µs. Two orders of magnitude up means the page cache is not absorbing writes; still below a rotation's 12.4 ms, which must not trip it |
| `obgw_wal_sync_latency_ns` p99 | > 100 ms; page at > 1 s | This IS the variable half of the recovery point objective. The published 20 ms window is the ticker, not the window. The quantile saturates at the 5 s top bucket; `_sum`/`_count` is exact |
| `obgw_wal_sync_latency_ns_count` | not advancing for 5 s while `orderbook_last_event_sequence` advances | The group-commit loop is gone. The venue is acknowledging orders it is not making durable, and nothing else says so |
| `orderbook_snapshot_age_seconds` | > 3× the checkpoint interval (90 s default); page at 10× | The recovery base is stale and every minute makes the next restart longer. The venue is still trading, deliberately |
| `orderbook_snapshot_age_seconds` | `NaN` unexpectedly, or negative | `NaN`: started without `-snapshot`, so every restart replays the whole log. Negative: this host's clock went backwards |
| `obgw_snapshot_failures_total` | any increase | Why the age is climbing. The previous snapshot is still in force and retention still runs against it, so nothing is lost yet |
| `rate(obgw_snapshot_duration_ns_sum[5m]) / 1e9` | > 0.25 | The fraction of wall time spent writing snapshots, across every book, computed directly. `_sum`/`_count` is a per-write mean with no book count in it |
| `obgw_snapshot_duration_ns_count` | not advancing while `obgw_snapshot_failures_total` is flat and a snapshot path is configured | The checkpoint goroutine is not running, as opposed to running and failing |
| `obgw_login_refused_total{reason="not_authorised"}` | any sustained rate, or 10× the trailing hour | Somebody is guessing passwords, or a deployment carries stale credentials. Disjoint from `obgw_refused_total` (§4.6) |
| `obgw_login_refused_total{reason="no_session"\|"bad_sequence"}` | any increase outside a restart window | Clients are losing their place in their own streams |
| *(both WAL histograms)* | — | Venue-wide, merged across books. Nothing on the page says WHICH book's log is slow (§2.4, §10) |
| `obgw_recovery_duration_ns` | > 30 s warn, > 120 s page, or > 2× the previous restart | The trend is the real alert: recovery cost is invisible until the restart you did not choose |

Three new runbook **sections**, each drilled in CI like every other entry on that page:

1. **"Checkpoints have stopped landing"** — signal, what the code has already done (kept
   trading, kept the previous snapshot, kept retention running against it), what to do,
   and what makes it worse (restarting into a stale base for no reason; disabling
   retention, which is the one thing still bounding the disk).
2. **"The group-commit loop has stopped"** — signal is a flat sync count with a live
   event sequence. What to do: take the node out of rotation and restart it. What makes
   it worse: assuming the latched `walFailed` path would have caught it. It only catches
   a sync that *failed*.
3. **"Orders left resting after a disconnect"** — signal is `obgw_shed_unreported_total`.
   Reconcile the disconnected account's orders against the feed and cancel by hand.
4. **"A negative snapshot age"** — added after review, because §6.4 defined the signal and
   §2.1 requires a procedure. Two causes needing opposite responses: this host's clock
   moved backwards, or the snapshot is on a network filesystem whose server's clock is the
   skewed one. The first is a trading-affecting event, because time-in-force deadlines read
   the same clock.

And two prose corrections, which are part of the deliverable rather than tidying:

- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) and `pkg/wal`'s package comment:
  the recovery point objective is `20 ms + p99 fsync`, and the second term is now
  measured (§5.4). Today both state a constant.
- [`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M14: six crosses become ticks
  (queue-full events, WAL append latency, WAL sync latency, snapshot age, snapshot
  duration, recovery duration), snapshot failure moves from "undefined" to defined with a
  pointer at §7, and the status line stops saying *every* lag is missing — because
  several still are, and the list must say which.

---

## 12. Deliverables and acceptance criteria

The rule for this table: **every signal must be shown moving under the condition it
describes, by a test that induces the real condition.** Not by calling the setter, not by
constructing a collector and observing into it by hand. The fixtures to induce them
already exist — `blockingLog` (`cmd/obgw/drills_test.go`) wedges a real matching
goroutine so a real queue fills; a read-only directory
(`cmd/obgw/diskfull_test.go`) makes a real write fail.

| # | Deliverable | Done when |
|---|---|---|
| 1 | `observability.Counter` | `TestCounterIsACounter`: registered twice with the same labels returns the same handle; the exposition emits `# TYPE … counter`, one `HELP`/`TYPE` pair per name, series sorted, values escaped by the existing `quote` |
| 2 | Counter increment allocates nothing | `BenchmarkCounterAdd` reports **0 allocs/op**, and a benchmark of the reject path reports the same allocation count as before this slice |
| 3 | **A real shed is counted** | `TestDrillAShedIsCounted`: a wedged matching goroutine, orders entered over a real socket until rejects come back; `obgw_refused_total{reason="overloaded"}` **equals the number of rejects the client received** — not merely non-zero |
| 4 | A rate-gate refusal is counted | `TestARateGateRefusalIsCounted`: rate 1/s burst 1, ten orders, `{reason="throttled"}` == 9 and `{reason="overloaded"}` == 0 |
| 5 | The refusal funnel is complete | `TestEveryRejectSiteIsCounted`: the sum over all `obgw_refused_total` series equals the number of `CmdReject` messages the client received across a mixed session (enter, dated, conditional, cancel, reduce, replace, mass cancel, malformed, unauthorised) |
| 6 | The reason vocabulary is frozen | `TestEveryReasonCodeHasAMetricName`: **parses `pkg/orderentry/reason.go`** and fails when a `Reason*` constant has no label. The first version compared two hand-written tables in this repository's own test file and froze nothing — verified by adding `ReasonRiskLimit = 18` to `pkg/orderentry` and watching it pass. It now fails |
| 7 | §4.3's mapping bug is fixed | `TestACancelRefusedDuringShutdownSaysSo`: `ReasonShuttingDown`, not `ReasonOverloaded`; and `{reason="overloaded"}` does not increment during a clean drain |
| 8 | The unreported drop is counted | `TestCancelOnDisconnectDropIsCounted`: queue wedged, client disconnects, `obgw_shed_unreported_total{op="cancel_on_disconnect"}` == 1 and the orders are still in the book — asserting both halves, because the counter is only worth anything if it means what it says |
| 9 | **The append histogram is exhaustive** | `TestAppendLatencyCountsEveryCommandKind`: a tape containing every mutating `cmdKind`; `obgw_wal_append_latency_ns_count` **equals `Writer.Seq()`** |
| 10 | Append excludes the fsync | `TestAppendLatencyExcludesTheSync`: under `-sync-every-command`, append p99 stays under 1 ms while sync p50 is milliseconds. The two histograms are disjoint by construction, and this is what proves the decorator is nested the right way round |
| 11 | Sync latency moves, and its count is a heartbeat | `TestSyncLatencyIsObserved`: 200 ms of a live server yields `_count` ≥ 8 with the venue idle; under `-sync-every-command`, `_count` equals the command count |
| 12 | Append timing costs what §9 says | `BenchmarkAppendWithoutTiming` / `BenchmarkAppendWithTiming`: allocations **unchanged** (6/op as measured here, and `TestAppendTimingAllocatesNothing` asserts the relative bound rather than the absolute count), time within 10% of the untimed path. Published in [`BENCHMARKS.md`](BENCHMARKS.md) as a row, not asserted as a claim |
| 13 | `TestZeroAllocHotPath` is untouched and still passes | Unmodified file, 0 allocs/op, and the two benchmarks it runs report the same `B/op` as before |
| 14 | **Snapshot age moves under a real failure** | `TestSnapshotAgeClimbsWhenCheckpointsFail`: read-only snapshot directory mid-run; age crosses 3× the interval, `obgw_snapshot_failures_total` increments, and the book keeps trading |
| 15 | Snapshot age survives a restart | `TestSnapshotAgeSurvivesARestart`: checkpoint, stop, backdate the file's mtime by an hour, restart; the gauge reads ~3,600 immediately, before the first tick. A process-local timer cannot pass this |
| 16 | Snapshot age reports what it cannot know | `TestSnapshotAgeIsNaNWithoutASnapshotPath`, and counts from process start when configured-but-absent |
| 17 | **A venue that cannot checkpoint stays ready** | `TestDrillCheckpointFailureKeepsTrading`: `/readyz` returns **200**, the body contains the degraded clause naming the symbol and the age, orders still match, and `walFailed` is still false |
| 18 | Snapshot duration moves | `TestSnapshotDurationIsObserved`: `_count` equals the number of successful writes, `_sum`/`_count` is within an order of magnitude of a wall-clock measurement taken by the test |
| 19 | **Recovery duration is reported and is the whole restore** | `TestRecoveryDurationIsReported`: 10,000-record log with a 5,000-order book, restart; the gauge is > 0, ≤ the wall time the test measured around `NewServer`, and **greater than the time `wal.Recover` alone takes on the same fixture** — which is the assertion that fails if the Adopts are outside the interval |
| 20 | Recovery duration reports what it cannot know | `NaN` with no WAL configured; per-symbol series at a two-book venue whose sum is the venue total |
| 21 | The exposition stays well-formed | `TestExpositionFormatIsWellFormed` (existing, unmodified) still passes with every new family registered; `cmd/obdash`'s parser reads the page unchanged |
| 22 | Every new runbook row is drilled | Three new drills, one per §11 section, each verified to **fail** against deliberately broken code before it counts |
| 23 | Surface change is deliberate | `internal/apicheck` regenerated with exactly the four added lines of §3 and no others; `internal/semcheck` green **without regeneration** — if it fires, this slice changed matching and must not have |
| 24 | Prose corrected | §11's two corrections, plus `RUNBOOKS.md`'s table and three sections, plus M14's ticks |
| 25 | **Every threshold on the page can actually fire** | `TestSyncLatencyThresholdsAreReachable`: at each tier, the reading an operator would take under the condition that tier exists for. Added after review found the sync p99 page tier sitting four times above the top bucket |
| 26 | **Refusals before login are counted** | `TestFailedLoginsAreCounted`: twenty-five bad passwords over real sockets move `obgw_login_refused_total{reason="not_authorised"}` to 25 and leave `obgw_refused_total` at zero — the two families disjoint, asserted in both directions. `TestEveryLoginRejectCodeHasAMetricName` parses `internal/wire/soup.go` to freeze the vocabulary |
| 27 | **The readiness probe still costs nothing** | `TestReadinessReadsNoFilesystem`: the snapshot is made unstatable and `/readyz` still reports its real age from the cached mtime. Fails against the statting implementation, which reports the process's own age instead |
| 28 | A venue with `-snapshot` and no `-wal` is not degraded forever | `TestSnapshotWithoutAWALIsNotPermanentlyDegraded`: no log means no checkpoint loop, so no degraded clause and no firing age alert |

---

## 13. Sabotage runs required before this counts as done

Each is a deliberate break, run to confirm the test that should catch it does. This
repository has had a test pass against its own sabotage twice —
[`TRADE-BUST.md`](TRADE-BUST.md) §7 and [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9.2
— and that is the only reason §12's claims are worth anything.

1. **Wrap `timedLog` outside `syncingLog` instead of inside.** Deliverable 10 must fail:
   append p99 must jump to milliseconds under `-sync-every-command`. **This is the most
   important run in the list**, because the composition is invisible in every other test
   — the default configuration behaves identically either way, and the mode where it
   matters is the one nobody runs locally.
2. **Delete `AppendSetPhase` from `timedLog`** (forward it undecorated). Deliverable 9
   must fail on `_count != Seq()`. If it passes, the tape does not contain a phase
   transition and the fixture is wrong, not the code.
3. **Remove the increment from one `sess.reject` call site.** Deliverable 5 must fail. If
   it passes, the funnel assertion is counting rejects at the same place it counts the
   metric and proves nothing.
4. **Count the shed in `Runner.tryEnqueue` instead of at the reject**, forwarding the
   count up. Deliverable 3's equality must fail — the queue-level count includes the
   mass-cancel and cancel-on-disconnect enqueues that produce no client reject.
5. **Increment after the wire encode succeeds rather than before the decision.**
   Deliverable 5 must fail with a client whose outbound queue is full (§4.2 rule 1),
   which the fixture induces by never reading the socket.
6. **Seed snapshot age from a process-local timer instead of the file's mtime.**
   Deliverable 15 must fail: the restarted venue reports ~0 for an hour-old base.
   Deliverable 14 must still pass — which is the point, and the reason the sabotage is
   worth running: the *easier* implementation passes the failure test and fails only the
   restart test.
7. **Clamp negative snapshot age to zero.** Deliverable 16's clock-jump leg must fail.
8. **Make snapshot failure fail `/readyz`, latching like the WAL path.** Deliverable 17
   must fail. This one sabotages a *decision* rather than a mechanism, which is exactly
   why it is here: §7 is the section most likely to be "fixed" by somebody who has read
   only the WAL-failure path.
9. **Time only `wal.Recover`, leaving both `Adopt` calls outside the interval.**
   Deliverable 19 must fail on the "greater than `wal.Recover` alone" leg. If it passes,
   the fixture's book is too small for the Adopts to be measurable and the fixture is
   wrong.
10. **Feed both append and sync into one histogram.** Deliverables 9 and 11 must both
    fail — the count matches neither the log's sequence nor the sync count.
11. **Replace the resolved counter handle with a `map[string]` lookup per increment.**
    Deliverable 2 must fail on allocations.
12. **Revert §4.3's mapping fix.** Deliverable 7 must fail, and — worth seeing once —
    deliverable 3 must still pass, because a clean drain and a real shed increment the
    same series. That is the reason the fix is in this slice rather than deferred.
13. **Register `obgw_refused_total` through `Gauge` instead of `Counter`.** Deliverable 1
    must fail on the `# TYPE` line. If nothing fails, the exposition test is checking
    values and not types, and it is the test that is wrong.

---

## 14. How this can fail, stated in advance

So that whatever gets written after the code is not graded on a curve.

- **The 80 ns could be 800 ns on somebody's hardware.** `time.Now()` is a vDSO read on
  Linux and a `mach_absolute_time` on Darwin, and on some virtualised hosts it is a
  trap into the hypervisor. There, two clock reads per append is not 5% of the append,
  it is 100%. §10 declines a flag to turn it off; if this bites, the answer is a flag,
  and the measurement that reveals it is deliverable 12's benchmark run on the host in
  question rather than on a laptop.
- **The append histogram may show that rotation is worse in production than in the lab.**
  12.4 ms is an APFS number from a benchmark. The first production p99.99 could be far
  worse, and this metric is the first thing that would show it — which is a success for
  the metric and a problem for `LOG-ROTATION.md` §11's first bullet, whose answer
  (pre-create the next segment) is a change to the crash matrix.
- **`obgw_refused_total` counts messages, not orders, and somebody will read it as
  orders.** One client message produces at most one reject today, so they are equal
  today. A future batch-entry message would break that silently, and the metric's HELP
  string is the only place that would say so.
- **The reject funnel runs partly on the matching goroutine** (§4.2). It is one atomic
  add and it is on the *unknown-order* path only. If a future change moves more rejects
  into resolvers, this becomes a real cost on the hot goroutine, and nothing will alert
  on that but a benchmark somebody remembers to run.
- **mtime-based snapshot age depends on the filesystem's clock, not the venue's.** On NFS
  the server's clock decides, and a skewed pair produces a persistently wrong age —
  including a persistently *negative* one, which §6.1 promises means something else. The
  failure mode is an operator chasing a clock problem that is real but is not the one the
  metric implies.
- **§7's decision could be wrong for a deployment this repository has not seen.** A venue
  with an aggressive orchestrator, an RTO measured in seconds, and a hot standby might
  genuinely want a failed checkpoint to move traffic. The decision here is made for a
  single-node venue holding a book, which is what `obgw` is. If the replication work
  makes standby promotion routine, §7 should be revisited rather than inherited.
- **The exposition roughly doubles in size**, and most of the growth reads zero forever.
  A one-book venue gains seventeen refusal series of which perhaps five are ever
  non-zero, three login-refusal series, one drop counter, three per-symbol series, and
  three histograms of twenty-two lines each. That is the correct trade for a label set that is bounded and frozen —
  a reason that has never fired is exactly the reason you want already graphed when it
  does — and it is still a page that is harder to read than the one it replaces. If the
  first thing a dashboard does is hide the zero series, this has not helped anybody.
- **The sync-count heartbeat could be the only thing that ever catches its failure.**
  This bullet used to say there was no test that kills the loop. There is:
  `TestDrillTheGroupCommitLoopHasStopped` closes `srv.quit`, waits for `syncLoop` to
  return, then advances the event sequence forty times and asserts the sync count is
  frozen while `walFailed` has not latched — which is the compound alert exactly as the
  runbook describes it. The line was written before the implementation and survived it,
  while [`RUNBOOKS.md`](RUNBOOKS.md) and [`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md)
  said the opposite in the same repository. What genuinely remains is narrower: the drill
  kills the loop by the one route the code offers, and a loop that dies by panicking
  inside `Sync` would take the whole process with it rather than leaving a venue that
  trades without syncing. That case is not reachable from a test and is not covered.

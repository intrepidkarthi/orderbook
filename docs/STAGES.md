# Stage Attribution — Where a Command's Time Goes in the Venue

Status: **implemented; first measurement in BENCHMARKS.md; step 3.4 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. The question

[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md) M10 asks for queue, match, log and
publication delay measured separately. Until now the gateway had one server-side
histogram for a command, `obgw_message_apply_latency_ns`. It lumps decode, queue wait
and dispatch together, by its own comment. When a venue slows, that number says that
it slowed, not where.

## 2. The stages

A command entering `cmd/obgw` passes through these, in order:

| Stage | From | To | Metric |
|---|---|---|---|
| **queue** | the command enters the Runner's queue | the matching goroutine takes it | `obgw_stage_queue_ns` (new) |
| **log** | the matching goroutine hands it to the command log | the append returns | `obgw_wal_append_latency_ns` (exists) |
| **sync** | a group commit starts its `fsync` | the `fsync` returns | `obgw_wal_sync_latency_ns` (exists; per sync, not per command) |
| **match** | the append returns | the engine has applied it and published its events to the sink | `obgw_stage_match_ns` (new) |
| **publish wait** | the first event of a batch reaches the publisher | the publisher's pump takes the batch | `obgw_stage_publish_wait_ns` (new) |
| **fan-out** | the pump takes the batch | every account stream it touches has it | `obgw_stage_publish_fanout_ns` (new; per batch) |

A command's queue, log and match stages add up to the time the matching goroutine
spent on it, plus its wait. The publish stages run on the pump's goroutine, off the
matcher, and are per batch, because that is how the pump works.

## 3. The hooks

- **`matching.RunnerConfig.ObserveStages func(queue, match time.Duration)`**
  - It is called on the matching goroutine after each command, so it must be cheap.
  - **When it is nil, the Runner reads no clock and stamps nothing.** The bench gate's
    `RunnerBare` benchmark is the check on that.
  - When it is set, a command carries its enqueue instant, about 8 bytes, and the
    matcher reads the clock twice more per command.
- **`(*orderentry.Publisher).ObservePublish(fn func(wait, fanout time.Duration))`**
  - It is set before `Pump` runs, and is nil by default.
  - The publisher notes when its queue goes from empty to non-empty, so the wait is
    measured from the batch's oldest event.
- **`cmd/obgw`** sets both, feeding four new histograms in the admin exposition beside
  the two WAL histograms.

## 4. How it is tested

- **Runner.** With an observer, every command reports exactly once, in order. A command
  made to wait behind a slow one shows that wait as queue time, and a slow engine
  operation shows as match time. Without an observer, the hot path allocates nothing
  more than before, and the existing allocation tests still hold.
- **Publisher.** A pump held up by a slow registry shows the delay as fan-out. A batch
  that waits behind it shows the delay as wait.
- **`obgw`.** After a short session, the four histograms have counts. The queue and
  match counts equal the commands the session sent.
- **Sabotage.** Each of these must fail a test:
  - swapping the queue and match durations;
  - stamping enqueue at dispatch, which makes queue time always zero;
  - observing a command twice;
  - measuring the publish wait from the newest event.

## 5. A first measurement

A local run of `obgw` with `obsoak` at a steady rate reads the six histograms. Their
quantiles go into [`BENCHMARKS.md`](BENCHMARKS.md) with the machine, the rate and the
durability mode, so the first published breakdown says where a command's time went.

## 6. Splitting the match stage

After [`WAL-SYNC.md`](WAL-SYNC.md), the match stage's p90 sat at ≤ 500 µs. The engine's
own cost is a few hundred nanoseconds per command, so nearly all of that is the sinks
`cmd/obgw` attaches. They run on the matching goroutine, in this order, through one
`matching.MultiSink`:

1. the name index, which makes an order addressable by its client id;
2. the order-entry publisher's `OnEvents`, which copies the batch into its queue;
3. the market-data feed;
4. the metrics collector.

`obgw` wraps each in a timer, one histogram per sink and one observation per batch:
`obgw_sink_index_ns`, `obgw_sink_publisher_ns`, `obgw_sink_feed_ns` and
`obgw_sink_collector_ns`. The wrapper is two clock reads per sink per batch, the same
cost `timedLog` pays per append.

**Tested:** after a session, all four histograms have the same count, and it is greater
than zero. Each batch passes through every sink once, so unequal counts mean a sink was
skipped or double-wrapped. **Measured:** STAGES.md §5's soak again, with the four
quantiles beside the match stage's.

## 7. What the split found: the feed's ring

Measured with §6's histograms, same soak:

| Sink | Mean | p90 ≤ | Total over 60 s |
|---|---:|---:|---:|
| market-data feed | 138 µs | 500 µs | **14.97 s of the match stage's 15.52 s** |
| publisher | 1.7 µs | 5 µs | 0.19 s |
| name index | 0.2 µs | 500 ns | 0.02 s |
| collector | 0.1 µs | 250 ns | 0.01 s |

`marketdata.Feed.publishLocked` evicted from its gap-fill ring with
`copy(f.ring, f.ring[1:])`. Once the ring was full, every update shifted the whole ring
by one slot on the matching goroutine. At obgw's default 65,536 updates of about 120 bytes
each, that is about 7.8 MB moved per update. The cost appeared only after the first
65,536 updates, which is why a short test never saw it, and why it showed in the mean
and not the median.

**The fix.**
- The ring becomes a fixed circular buffer: a head index and a count, so an eviction
  advances the head instead of moving memory.
- `Since` copies out in at most two pieces, across the wrap.
- Everything callers see is unchanged: sequences, `ErrSequenceEvicted`, the gap-fill
  window.

**Tested.**
- The existing feed tests pass unchanged.
- A new test publishes three times the ring's capacity, and requires `Since` to return
  exactly the retained window at many points across the wrap. It also requires
  eviction to start where it did before.
- A test compares the cost per publish into a full ring of 1,024 with one of 65,536. On
  the old code the large ring is about 64× slower per publish, and the test requires
  the two within 3×.

**Re-measured** with §5's soak: the feed's histogram, and the match stage's.

**Result (2026-10-07).**
- Publishing into a full ring now costs 16 ns at both 1,024 and 65,536 slots, where
  65,536 had cost 258 µs.
- The ring tests pass; the four sabotages of the circular indexing are caught.
- The soak was run four times, alternating the two builds:

| | Old ring | New ring |
|---|---|---|
| match p90 / p99 | ≤ 500 µs / ≤ 1 ms, both runs | ≤ 25 µs / ≤ 50 µs, both runs |
| match mean | 129 µs | 6.4 µs |
| queue p90 | ≤ 500 µs, both runs | ≤ 10–25 µs, both runs |
| queue p99 / client p99 | 5 ms / 5 ms, then 100 ms / 100 ms | 250 ms / 250 ms, then 250 µs / 5 ms |

**The match stage is now what the engine and its sinks actually cost.** The queue's
p99, and the client's, swung between 5 ms and 250 ms across runs of either build.
That tail does not belong to either ring. Each run also includes one or two
checkpoints, taken on the matching goroutine at 13–14 ms each with about 640 resting
orders. That fits the smaller tail spikes, not the large ones. The large spikes are
most likely this laptop, as §5's 100 ms run of both builds suggested. Telling the two
apart needs a quieter machine, and it is the next open question here.

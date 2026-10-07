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

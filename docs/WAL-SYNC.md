# The `fsync` Outside the Log's Lock

Status: **implemented and re-measured; step 3.4a of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. What was found

Stage attribution ([`STAGES.md`](STAGES.md), [`BENCHMARKS.md`](BENCHMARKS.md)) put the
p99 of both the queue wait and the WAL append in the same bucket as the `fsync`: ≤ 5 ms
on an M4. `Writer.Sync` holds `w.mu` from the buffer flush through the `fsync`. The
group-commit loop calls it every 20 ms, so each time, the matching goroutine's next
append waits for the disk, and every command behind it waits too.

## 2. What must still hold

1. **Sync's promise.** When `Sync` returns nil, every record appended before `Sync` was
   called is on stable storage. That is the venue's recovery point (20 ms plus the sync
   time), and it does not weaken.
2. **One file, one sync.** The `fsync` covers the file the flush wrote to. A rotation
   must not close that file while its `fsync` is running. A closed descriptor returns
   `EBADF`, which would latch a durability failure the disk never had.
3. **The latch.** A failed flush or `fsync` latches `failed`, every append after it
   refuses, and clearing it takes a restart. The latch can be set by a sync running
   outside the lock, and is read by appends inside it.
4. **Rotation stays atomic.** Nothing outside `w.mu` ever observes a half-rotated writer.
   No reader may see the old file closed and the new one not yet installed.
5. **Close is final.** `Close` syncs and closes. A `Sync` running when `Close` is called
   finishes first, and a `Sync` called after `Close` returns an error rather than
   touching a closed file.

## 3. The change

- **`Sync` takes a second mutex, `syncMu`, so syncs never overlap.** Under `w.mu` it
  refuses if `failed` is set, flushes the buffer, notes the active file, and sets
  `syncing`. It releases `w.mu`, runs the `fsync`, then re-takes `w.mu`. There it clears
  `syncing`, broadcasts on a condition variable, and latches any error.
- **`rotateLocked` and `Close` wait while `syncing` is set**, on that condition
  variable, before their first step. Waiting releases `w.mu`. That is safe because
  appends come from one goroutine, the one that is rotating, and nothing else changes
  the writer's state. Once the wait returns, `w.mu` is held again and the rest runs
  exactly as today.
- **Appends during an `fsync` write into the buffer, and possibly to the file.** Whether
  that `fsync` covers them is not promised either way. The next `Sync` does, which is §2.1
  exactly.
- `Sync` after `Close` returns `ErrClosed`.

The `fsync` call goes through a test-only hook, like `beforeRotateStep`, so tests can
make it slow or make it fail.

## 4. How it is tested

- **The stall is gone.** With the hook sleeping 100 ms in the `fsync`, an append made
  while a `Sync` is in flight returns in well under that. On the old code it waits the
  full 100 ms, and the test is watched failing there first.
- **The promise.** Records appended before `Sync` are readable by `ReadAll` from another
  descriptor after it returns. Records appended during it are present after the next
  `Sync`.
- **Rotation during an `fsync`.** A tiny segment size forces rotations while a slow sync
  is in flight. No error is latched, `ReadAll` returns every record in sequence order,
  and the sealed segments pass verification.
- **The latch.** The hook fails once. `Sync` returns the error, `Failed()` reports it,
  and the next append refuses.
- **Close during an `fsync`** waits for it, and a `Sync` after `Close` returns
  `ErrClosed`.
- **Everything existing still passes**, including `docs/LOG-ROTATION.md`'s crash matrix,
  under `-race` as well.
- **Sabotage.** Each of these must fail a test:
  - fsyncing under the lock again;
  - rotation not waiting;
  - latching nothing on an `fsync` error;
  - dropping `syncMu`.
- **Re-measured.** The §5 soak of [`STAGES.md`](STAGES.md) runs again, and the queue and
  append tails are reported beside the first measurement.

## 5. What it did (2026-10-07)

**Built as specified, with one addition the first test run forced.** The first version
had rotation and `Close` wait while `syncing` was set and nothing more. Under a tight
loop of syncs it hung for over 300 s. Each time a sync finished, the next one took the
lock before the waiting rotation re-took it, and set `syncing` again. Now a rotation or
`Close` first sets `holdSyncs`, so no new `Sync` starts, and then waits only for the one
in flight.

**Tests.**
- The stall test was watched failing on the old code first: an append during a 150 ms
  `fsync` took 161 ms. It passes now.
- `TestSyncsDoNotOverlap` was added after the first sabotage of `syncMu` survived. It
  makes overlapping syncs observable directly.
- Five of six sabotages are caught: `fsync` under the lock, rotation not waiting,
  `Close` not waiting, no latch, and no `syncMu`.
- The sixth, waiting without the hold, survives. Starvation is a liveness failure that
  needs the losing interleaving, and no test here can force it. The first version's
  hang is the evidence the hold is needed.
- The whole `pkg/wal` and `cmd/obgw` suites pass under `-race`, the rotation crash
  matrix included.

**Re-measured**, with STAGES.md §5's soak (M4, 2,000 msg/s, group commit, 60 s). Old and
new builds were run back to back twice. Bucket upper bounds:

| Stage | Old p50 / p90 / p99 | New p50 / p90 / p99 |
|---|---|---|
| WAL append | 2 µs / 10–25 µs / **5 ms** | 2–5 µs / 25 µs / **100 µs** |
| queue wait (first pair) | 10 µs / 1 ms / 5 ms | 5 µs / 250 µs / 1 ms |
| match | 10 µs / **250 µs** / **500 µs** | 10 µs / **500 µs** / **1 ms** |
| client end-to-end p99.9 (first pair) | 100 ms | 25 ms |

- **The append no longer waits for the disk.** Its p99 fell fifty-fold in both pairs, and
  the client's worst tail shrank.
- **The match stage got a bucket slower, in both pairs.** The matcher now runs while the
  `fsync` is in flight instead of sitting it out, and the two compete. That is the next
  thing stage attribution should split. It is recorded rather than averaged into a win.
- The second pair's queue p99 read 100 ms for **both** builds. That was the machine, not
  the change.

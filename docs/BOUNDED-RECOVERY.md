# Bounded Recovery — Skipping a Covered Prefix Without Stopping Looking At It

Status: **built** — slice 1 of [`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md)
M3, written before the code as this repository does it ·
Author: Karthikeyan NG · Last updated: 2026-08-16

> **§9 records where the code disagreed with this document.** Three places: the
> internal reader is called `walkLog` and returns a struct rather than
> `readFrom(path, afterSeq)`; one of §7.2's sabotage runs did not fail the test it was
> supposed to, and finding out why produced a test this document did not ask for; and
> the measured speedup is roughly 26×, not the "roughly half" §5.3 and §8 expected.
> The last one is the interesting one, and §8 already said which way to resolve it.
>
> **§9.5 records what an adversarial review then found this document had decided by
> omission**: one behaviour change §5 did not list, one claim in §9.1 that was broader
> than the code, and three published numbers that were wrong. §5 and §9.3–§9.4 are
> corrected in place; §9.5 says what was wrong and how it was found, because a spec
> that quietly edits its own numbers is worth less than one that shows them.

Companion documents:
- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) §"Running continuously" — the
  measurement this exists to move, and the sentence "a venue left running becomes
  unrestartable within days".
- [`RUNBOOKS.md`](RUNBOOKS.md) §"A corrupt log record" — the operator procedure that
  only makes sense if the venue actually verifies the whole log. §5 is about not
  quietly invalidating it.
- [`BENCHMARKS.md`](BENCHMARKS.md) §"Recovery time" — the published table, which §7
  requires republishing because its fixture conflates two different terms.

---

## 1. Why this exists

`wal.Recover` reads and parses the entire write-ahead log even when a snapshot already
accounts for nearly all of it. The snapshot bounds what is **applied**; it bounds
nothing about what is **read**. Measured today: 1.47 s and roughly 611 MiB of
allocation for a log with nothing left to apply.

The failure this produces is unusual in that it is invisible until the worst moment. A
venue that never restarts never notices. The first restart after a long uptime is when
the arithmetic arrives, and by then the operator is already in an incident.

Two things are wrong, and this document fixes one of them:

1. **A restart reads the whole log.** That is this slice.
2. **The log never shrinks.** That is slice 2 — [`LOG-ROTATION.md`](LOG-ROTATION.md),
   now built — and it is untouched here.

Fixing (1) without (2) is worth doing on its own because the two walls are different
heights. The memory wall is the sharp one, 2.1 KB of allocation per stored record of
306 bytes, and it is removed entirely. The time wall is reduced, not removed. §6 says
so plainly so nobody reads this as the milestone being done.

## 2. What `Recover` does after this change

Unchanged in shape. The snapshot is read first, then the log, then the tail is applied.

1. `ReadSnapshot(snapPath)`. A corrupt snapshot is refused before the log is opened.
   This ordering is deliberate and pinned by `snapshot_durability_test.go:202`: a venue
   whose base is unreadable should say so without first spending a minute reading a log
   it is not going to use.
2. `after = snap.WALSeq` when a snapshot exists, `0` otherwise. Unchanged.
3. `readFrom(walPath, after)` instead of `ReadAll(walPath)`. This is the new internal
   reader. It walks every record in the file, in order, verifying each one, and
   **retains** only those at ordinal `after+1` and beyond. `after == 0` makes it
   behave exactly as `ReadAll` does today, record for record and error for error.
4. `matching.RestoreEngine(config, snap)` or `matching.NewEngine(config)`. Unchanged.
5. `RestoreAfter(eng, entries, after)`. Unchanged, and deliberately still passed
   `after`. Ordinal arithmetic decides what the reader **keeps**; the `Seq > afterSeq`
   filter decides what is **applied** out of that. The order is not symmetric and §9.2
   says why it matters: the filter can drop a record the walk retained, and it never
   sees a record the walk discarded. Retaining one record too many is safe. Discarding
   one too many is not, which is what Rules 1 to 4 are for.

A missing log file still yields the snapshot alone. A missing snapshot still replays
the whole log. Neither present still yields a fresh engine.

`ReadAll` keeps its signature, its behaviour and its godoc example. It becomes
`readFrom(path, 0)` and nothing else changes about it. That is not politeness toward
the API; a dozen tests index the returned slice by ordinal and assert its length
(`boundary_test.go:105`, `checkpoint_test.go:157`, `runner_recovery_test.go:206`,
`cmd/obgw/synclog_test.go:34`), and `RUNBOOKS.md` tells an operator to reach for it
when a client and the venue disagree about an order.

`lastSeq` is fixed in the same pass, for the same reason and with the same mechanism.
It needs one number, the last record's `Seq`, and today it gets it by parsing the whole
file into a slice and taking the last element. An `obgw` restart therefore reads the
log **twice**: once in `Recover` (`cmd/obgw/server.go:276`) and once inside `Open`
(`:296`). After this change `lastSeq` walks the frames, verifies every CRC exactly as
before, keeps the most recent verified payload in a reusable buffer, and parses that
one payload at EOF. Framing and checksum damage still surface exactly as before, which
matters because `integrity_test.go:276` asserts `Open` returns `ErrCorrupt` and injects
CRC damage to get it. A payload that passes its CRC and does not decode in the middle of
the file no longer stops `Open` — §9.1 argues that `Open` has to be the most permissive
reader in the package, not merely that it happens to be.

## 3. The framing arithmetic

No on-disk change. Not the record layout, not `Magic`, not `MaxRecordBytes`.

### 3.1 Headered framing (current format)

```
offset 0    "OBWAL\x01"                      6 bytes
            [len:4][crc:4][payload:len]      record 1
            [len:4][crc:4][payload:len]      record 2
            ...
```

`len` and `crc` are big-endian `uint32`. The CRC is CRC-32C (Castagnoli, hardware on
amd64 and arm64) over the payload bytes only, not over the length prefix. Record *k*
occupies `8 + n_k` bytes, so the offset of record *k*'s length prefix is
`6 + Σ_{i<k} (8 + n_i)`.

That sum is the whole point of §5: **payload lengths vary, so the offset of record
`after+1` cannot be computed without reading every length prefix before it.** There is
no arithmetic that jumps the prefix.

The per-record loop, for every record including skipped ones:

1. `io.ReadFull` 4 bytes → `n`. A short read is a clean stop (torn tail), exactly as
   today.
2. Bound check: `n == 0 || n > MaxRecordBytes` (8 MiB) → `ErrCorrupt`. Unchanged, and
   it is what catches `integrity_test.go`'s corrupted length prefix.
3. `io.ReadFull` 4 bytes → `want`. Short read is a clean stop.
4. `io.ReadFull` `n` bytes into a reusable buffer. Short read is a clean stop.
5. `crc32.Checksum(buf[:n], crcTable) != want` → `ErrCorrupt`. **Performed on every
   record, skipped or not.**
6. `json.Unmarshal` and retention only where §4 says.

The buffer in step 4 is one slice grown on demand to the largest record seen, reused
across records. That is the entire allocation story: for the measured 300,000-record
log at 306 bytes each, the skip allocates about 300 bytes total against today's
611 MiB. Not amortised down, not pooled, just not made.

### 3.2 v1 headerless framing

```
offset 0    [len:4][payload:len]             record 1
            [len:4][payload:len]             record 2
```

No magic, no CRC. Record *k* occupies `4 + n_k` bytes. Detected the way `ReadAll`
detects it now, by peeking 6 bytes and finding they are not `Magic`.

**On a v1 log the skip still parses every record, and saves only the retention.** The
reason is that a v1 record has no checksum, so the only integrity signal available is
whether the payload decodes, and `wal.go:531` uses exactly that: an undecodable record
in a v1 log is treated as the tail. Skipping the decode would remove the only check
there is. Worse, it would remove the guard against a misframed walk: if a v1 length
prefix is damaged to some value under 8 MiB, the reader consumes the wrong number of
bytes and every frame after it is nonsense. Today that nonsense fails to decode and the
reader stops. Without the decode it would keep walking and could hand back garbage
records from beyond the boundary.

So v1 logs get the allocation win and not the time win. They are pre-checksum artifacts
kept readable so an upgrade never strands a file, and buying them speed at the cost of
their only integrity check would be a poor trade. A useful side effect: because every
v1 record is decoded anyway, the invariant check in §4 is total on a v1 log rather than
sampled.

## 4. The invariant, the check, and the fallback

The skip rests on one property: **the ordinal of a record in the file equals its
sequence number.** It holds because `Writer.append` increments `seq` and writes exactly
one record under the same mutex, rolling the counter back on the two failures that
write nothing (`wal.go:304`, `:308`), and because `Open` resumes `seq` from the last
complete record.

It is a property of the files this package writes today. It is not a law, and the next
slice of this same milestone breaks it on purpose: a rotated segment starts at ordinal
1 carrying sequence *k* ≫ 1.

**Rule 1 — read record 1 and require `Seq == 1`.**
*Reason:* this is the anchor. Without it, a rotated segment whose first record is
sequence 400,001 would be walked past in its entirety and would apply nothing, with no
error and no log line. Silent data loss, produced by an optimisation, in a venue that
believes it recovered. The check costs one `json.Unmarshal` and it fires at ordinal 1,
so the rotated-segment case is detected before any work is done.

**Rule 2 — parse record 1, and parse every record from ordinal `after` onward. Read,
CRC-verify and discard everything between.**
*Reason:* stating it this way collapses three separate checks into one predicate.
Record `after` is the last record the snapshot covers and record `after+1` is the first
one to apply, so both ends of the boundary are inspected; and when the log ends exactly
at `after` there is still a parsed record to check against, which the naive "parse the
boundary record" version does not have.

**Rule 3 — require `Seq == ordinal` at every record that gets parsed.**
*Reason:* the only cheap statement of the invariant. Two or three records are checked on
a headered log, all of them on a v1 log.

**Rule 4 — on any disagreement, discard everything, reopen the file, and read it with
the full-parse path. Do not guess an offset, do not continue from where the walk is.**
*Reason:* once the invariant is in doubt, the records already discarded may be exactly
the ones that needed applying. Continuing forward would turn a detected anomaly into
undetected data loss, which is worse than the anomaly. The cost is one extra pass in a
case that costs one pass today, paid only by files that already look wrong.

**Rule 5 — number records for error messages by file ordinal, counting skipped records.**
*Reason:* `ReadAll` today writes `record %d` using `len(out)+1`, which is the ordinal
only because it retains everything. A skipping reader that kept that expression would
report record 1,001 for the 201,001st record, and `RUNBOOKS.md` §"A corrupt log record"
tells an operator to recover "the log records *before* the corrupt one". An off-by-two-
hundred-thousand in that number is a wrong recovery, and it would look like a correct
one.

**Rule 6 — a log that ends before ordinal `after` is reported, not treated as an empty
tail.**
*Reason:* it means the snapshot claims commands the log does not contain. That is either
the wrong pair of files, or a truncated log, or the group-commit window (see below), and
the three are indistinguishable from inside this package. §5 explains why it is a report
rather than a refusal.

### 4.1 Why the snapshot-ahead case is reported and not refused

`Runner.Checkpoint` (`pkg/matching/engine_loop.go:258`) stamps the snapshot with
`lastApplied` and does **not** sync the log first. `obgw`'s checkpoint loop
(`cmd/obgw/server.go:565`) writes and fsyncs the snapshot without syncing the WAL
either. The WAL group-commits every 20 ms by default. So a snapshot can be durable
while records it covers are still in the writer's buffer, and a crash in that window
leaves exactly this state on disk, after nothing worse than an ordinary power loss.

Recovery from that pair is still correct: the missing records' effects are already
folded into the snapshot. Refusing to start there would convert a benign, recoverable
state into an outage, so `Recover` does not refuse.

It is reported because the state has a consequence that outlives the restart. `Open`
resumes the sequence from the log's last record, which is behind the snapshot's
`WALSeq`, so the venue reuses sequence numbers the stale snapshot already claims to
cover. Until the next checkpoint lands, a second crash would have `RestoreAfter` skip
those newly written records as "already covered". Naming the condition is what lets an
operator force a checkpoint and close it.

Surfacing it needs somewhere to put the fact, so:

```go
// RecoverReport describes what a recovery read, as opposed to what it applied.
type RecoverReport struct {
    SnapshotSeq   int64 // snap.WALSeq, or 0
    LogLastSeq    int64 // sequence of the last complete record
    Skipped       int   // records read and CRC-verified but not retained
    Applied       int   // records handed to RestoreAfter
    FellBack      bool  // Rule 4 fired
    SnapshotAhead bool  // Rule 6 fired
}

func RecoverWithReport(config matching.Config, snapPath, walPath string) (*matching.Engine, RecoverReport, error)
```

`Recover` keeps its exact signature and calls this, dropping the report. `cmd/obgw`
calls `RecoverWithReport` and logs the condition next to its existing "recovered N
resting orders" line. A missing log file is not the condition and is not reported: the
documented "a missing log yields the snapshot alone" behaviour is unchanged.

## 5. What is kept, what is given up, and why seeking was rejected

This is the section that decides whether the change is worth making.

### 5.1 Kept

- **Every byte of the covered prefix is still read and every CRC is still checked.** The
  venue keeps the property it is actually sold on: it refuses to start rather than serve
  a book whose log has been *altered on disk*, wherever in the log the alteration is.
  That is the media-damage property, and it is untouched. It is not the same as "any
  disagreement anywhere", which §5.2 corrects.
- **`RUNBOOKS.md` §"A corrupt log record" stays true.** The signal still appears, and it
  still appears for a record behind the snapshot.
- **Torn-tail semantics are unchanged.** A short read at the length, the CRC or the body
  is still a clean stop at the last complete record.
- **The length-prefix bound is unchanged**, so a flipped bit in a length still cannot
  turn recovery into a multi-gigabyte allocation.
- **`ReadAll`'s contract is unchanged**, and with it every test that indexes entries by
  ordinal.
- **Snapshot before log**, unchanged.
- **`RestoreAfter`'s sequence filter is still the authority over everything the reader
  hands it.** Note the qualifier; it is not the flat statement an earlier draft of this
  document made, and §5.2's last bullet says what the difference costs.

### 5.2 Given up

- **Time does not become flat in the prefix.** It falls by whatever fraction of the
  per-record cost is parse-and-retain rather than read-and-verify. Do not write "bounded
  restart" anywhere as a result of this change. §6 has the arithmetic that is left.
- **The prefix's parsed records are no longer available to `Recover`'s caller.** Nothing
  consumed them, since `RestoreAfter` dropped them, and `ReadAll` still exists for
  anyone who wants them.
- **On a v1 headerless log only the allocation improves**, for the reason in §3.2.
- **A record that passes its CRC and does not decode is no longer refused when it sits
  strictly behind the boundary.** This bullet was missing from the first draft and a
  review found it by differential fuzzing; it is the one place where this change starts
  a venue the old code refused to start, so it is written out in full.

  A checksum answers "are these the bytes that were written". It does not answer "are
  those bytes a record". The second question costs a `json.Unmarshal`, which is exactly
  what the skip stopped paying for covered records. So the check now moves with the
  snapshot: a payload of garbage that carries a correct CRC is refused at and past
  ordinal `after`, and walked past behind it.

  Reachability is narrow — the bytes have to checksum correctly, so this is a writer
  bug, a format mismatch or a deliberate forgery, never bit rot — and the consequence is
  bounded. **The recovered book is identical either way**, because `RestoreAfter` drops
  a covered record for its sequence whether or not it decoded. What is lost is the
  diagnostic, and it is lost permanently: each checkpoint moves the boundary further
  past the record, so a venue that starts once will keep starting.

  Refusing to start on it was arguably the wrong behaviour anyway — the operator had no
  route forward except surgery on a record whose effects are already folded into the
  snapshot — but that is a rationalisation after the fact, not the reason. The reason is
  that restoring the check means decoding the covered prefix, which is the entire cost
  this slice removed. `ReadAll` still decodes everything and still reports it, which is
  where `RUNBOOKS.md` sends an operator who wants to know what is in the file.
  `TestUndecodableRecordBehindTheSnapshotStartsTheVenue` pins all four legs: the venue
  starts, on the same book the undamaged log produces; `ReadAll` still refuses; `Open`
  must not refuse; and the same damage past the boundary is still refused by ordinal.
- **The invariant is checked at two or three ordinals, not all of them.** A sequence that
  drifts strictly inside the covered prefix and comes back into step by ordinal `after`
  is not detected. Worth stating plainly, and worth stating that nothing detects it
  today either: `RestoreAfter` has never compared an ordinal to a sequence. Nothing that
  is checked now stops being checked.

  One consequence of that is sharper than the sentence above admits, and belongs here
  rather than in §5.1. A record sitting at ordinal 10 of a log whose snapshot covers 50,
  carrying sequence 9,999 and a correct CRC, **used to be applied** — `ReadAll` decoded
  it, `RestoreAfter` saw 9,999 > 50 and replayed it. It is now discarded by ordinal
  before its sequence is ever read. Measured on the two builds: 101 resting orders
  before, 100 after. Neither number is more correct than the other, because the file is
  one no writer in this package can produce; what matters is that ordinal arithmetic,
  not the sequence filter, is what decides the outcome behind the boundary. Rules 1 to 4
  exist to make sure a file that could produce such a record is sent back through the
  full parse instead.

### 5.3 Why not seek past the prefix

The fast version of this change reads a length prefix, seeks `4 + n` bytes forward and
never touches the payload. It was rejected.

**It is not one seek, it is a chain of them.** Per §3.1, record offsets depend on every
preceding payload length, so even the seeking version reads a 4-byte prefix per record
and walks the whole file. What it actually saves is the payload bytes, from a page cache
that is usually warm, and a hardware CRC32C over them. That is memory bandwidth. It is
not the part of the 428 ms that hurts.

**It buys that bandwidth with the only corruption check the prefix has.** A record that
is never read is a record whose CRC is never computed, so bit rot behind the snapshot
becomes undetectable — and permanently so, because the next checkpoint moves the
boundary forward and buries it deeper. The venue's promise would quietly become "we
verify the log, except the part we do not read", which is not a promise an operator can
act on and not one this repository should make.

**And nothing in the suite would have caught the mistake.** `integrity_test.go:272`
recovers with `snapPath = ""`, and `cmd/obgw/drills_test.go:53` sets
`cfg.SnapshotPath = ""` on purpose to force recovery through the log. Both are
`after == 0` cases. There is, today, **no test that corrupts a record inside a covered
prefix**, so the seeking version would pass everything, ship, and take a durability
property with it silently. This is the decisive argument: an unchecked safety property
is one the next optimisation removes. §7 therefore requires that test to exist whether
or not the skip is ever made faster.

**The honest route to flat time already has a name.** M3's WAL list includes
"segment-level verification". A per-segment digest over its records' checksums would let
recovery verify a whole covered segment with one comparison and then genuinely skip it,
with the integrity property intact rather than dropped. That is a format change and it
belongs with rotation in slice 2. Trading the check away now, in exchange for a saving
measured in memory bandwidth, would be paying the whole price for a fraction of the win.

## 6. What this deliberately does not do

- **It does not rotate, truncate or archive the log.** The file still grows without
  bound, about 44 GiB a day at 2,500 messages/s. Restart time is still O(total log),
  with a smaller constant. One day of continuous operation at that rate is 216 million
  records; reading and CRC-verifying them at the measured 112 ns each is about 24
  seconds, and the day after that is 48, and it never stops climbing. **Slice 1
  removes the memory wall and lowers the time wall. Only rotation removes the time
  wall.** Any sentence that says otherwise is wrong.

  *(This bullet said "roughly 144 million records… about 16 seconds". 2,500 × 86,400
  is 216 million, and 216 million × 220 bytes is the same 44 GiB the sentence above it
  quotes — so the record count and the byte count disagreed with each other by 1.5×.
  Corrected by [`LOG-ROTATION.md`](LOG-ROTATION.md) §1, which had to run the same
  arithmetic to size a segment. The conclusion is unchanged, which is the point of it.)*

  **Slice 2 built the rotation.** Restart cost is now O(*retained* log), bounded by a
  byte budget the operator sets — and it is O(total log) still for a venue that does
  not set one, because deletion is off by default. See
  [`LOG-ROTATION.md`](LOG-ROTATION.md).

  *(This bullet said "still minutes" until §9.3 was measured. That was the pre-change
  constant of ~3.33 µs a record carried forward by hand — 144 million of those really is
  eight minutes. `PRODUCTION-READINESS.md` ran the same arithmetic on the post-change
  constant and said "seconds rather than minutes", and the two documents shipped in one
  changeset disagreeing about it. Corrected in the direction of the measurement.)*
- **It does not change the on-disk format.** No new magic, no version bump, no new
  field. A log written before this change is read by it, and a log written after it is
  read by an older build.
- **It does not change `ReadAll`, `Restore`, `RestoreAfter` or `Recover`'s signature.**
- **It does not give `examples/replication/primary.go:206` the same primitive.** That
  code does `ReadAll` and then filters `Seq <= h.Have` itself, so it is the second
  natural consumer of a "records after sequence N" reader and it pays the same full
  parse. Serving it means exporting the reader, and exporting it means designing its
  contract for callers outside this package. Not now.
- **It does not make concurrent writers safe.** There is no `flock`, lock file or
  `O_EXCL` anywhere in the repository. Two `Writer`s on one path both resume from the
  same `lastSeq` and interleave under `O_APPEND`, producing duplicate sequences and
  ordinals that are not sequences. Rules 1 and 3 catch that only if the damage happens
  to land at a parsed ordinal. This design neither fixes it nor claims to.
- **It does not sync the WAL before a checkpoint.** That is the actual fix for §4.1, it
  is a change to `Runner.Checkpoint` in `pkg/matching`, and it moves the checkpoint
  pause cost. Named here so it is not mistaken for an oversight.

### 6.1 Known defect, out of scope: a torn fragment followed by an append

Confirmed by experiment against the shipped package, not projected.

`Open` never truncates a torn tail. `lastSeq` stops cleanly at the last complete record,
and then `Open` takes `O_CREATE|O_WRONLY|O_APPEND` (`wal.go:243`) and writes the next
record **behind the fragment's bytes**. The fragment's length prefix now finds enough
following bytes to look complete, and its CRC fails.

Reproduction, which took three tries and produced the same shape each time:

1. Write a headered log of 5 records and close it.
2. Truncate the file by 1, 5 or 17 bytes, simulating a crash mid-write.
3. `ReadAll` → 4 entries, `err == nil`. Correct: a partial record is not applied.
4. `Open` → succeeds, `seq = 4`. `AppendSubmit` → sequence 5, written after the
   fragment.
5. `ReadAll` → 4 entries and
   `wal: corrupt record: record 5 checksum 1fe167eb, want d1517f35`.

So: **crash mid-write, first restart succeeds, second restart refuses to start.**

Out of scope for this slice, and recorded here so it is not lost. Two notes for whoever
takes it:

- The verified skip walks straight into the same failure at the same record, and fails
  there, which is the correct outcome and is a direct consequence of §5.1. A seeking
  skip would walk past it whenever the fragment sits behind the snapshot, and the venue
  would apply the records after it as though nothing were wrong.
- The fix is to truncate the torn tail at `Open`, and this slice makes it cheap: the new
  frame walk already knows the byte offset immediately after the last complete record,
  which is the offset to truncate to. It needs its own spec, because truncating a file
  in a recovery path is a decision with consequences and this one has a live case where
  the fragment is the only evidence a command was ever attempted.

## 7. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | `readFrom(path, afterSeq)` with `ReadAll = readFrom(path, 0)` | Every existing `pkg/wal` and `cmd/obgw` test passes unchanged, full run and `-race`, not just `-short` |
| 2 | `Recover` uses it; `lastSeq` walks frames and parses one record | `go test ./pkg/wal/... ./cmd/obgw/... ./examples/...` green; `integrity_test.go:276` still gets `ErrCorrupt` from `Open` |
| 3 | Corruption behind the snapshot is still detected | `TestCorruptionInTheCoveredPrefixStillRefuses`: a CRC-damaged record at ordinal 10 of a 1,000-record log with a snapshot at `WALSeq = 500` makes `Recover` return `ErrCorrupt`. It must fail against a seek-based skip |
| 4 | The skip is equivalent to the full parse | `TestSkippedRecoveryMatchesFullParseAtEveryBoundary`: for every `k` in a 200-record log, `Recover` with a snapshot at `WALSeq = k` produces a digest identical to `RestoreEngine` + `ReadAll` + `RestoreAfter` at the same `k` |
| 5 | Rule 1 and Rule 4 work | `TestSegmentStartingAtANonOneSequenceFallsBack`: a hand-built headered log whose records carry `Seq = 1000 + ordinal`, with correct CRCs, recovers the same records as the full-parse path, and the report has `FellBack == true` |
| 6 | Rule 6 works | `TestSnapshotAheadOfLogIsReported`: snapshot `WALSeq = 500` against a 100-record log recovers without error, applies nothing, and reports `SnapshotAhead == true` |
| 7 | v1 logs unaffected in behaviour | A headerless log with a snapshot covering half of it recovers to the same digest as the full-parse path |
| 8 | Allocation is flat in the covered prefix | The inverted restart-cost test, below |
| 9 | Prose corrected | `wal.go:9-13`, `wal.go:732-745`, `cmd/obgw/server.go:66-69` and `:544-548`, `PRODUCTION-READINESS.md`, `BENCHMARKS.md`, `SOAK.md:652`, `REPLICATION.md` all say what the code now does |

Three more, added after review rather than before the code. §9.5 says how each was found.

| # | Deliverable | Done when |
|---|---|---|
| 10 | §5.2's decode gap is decided, not stumbled into | `TestUndecodableRecordBehindTheSnapshotStartsTheVenue`: a CRC-valid non-record at ordinal 10 behind a boundary at 500 starts the venue on the *same digest* the undamaged log gives; `ReadAll` still refuses it by file ordinal; `Open` does **not** refuse it; the same damage at ordinal 600 is still `ErrCorrupt` |
| 11 | v1's only integrity check is pinned | `TestV1UndecodableRecordBehindTheSnapshotStillStops` fails when `!checksummed` is dropped from `walkLog`'s parse predicate. The clean-log v1 test passes against that mutation, which is why this one exists |
| 12 | The resting-book restart shape is asserted, not only benchmarked | `TestARealRestartAllocatesForItsBookAndNotForItsLog`: marginal allocation per additional covered record on `buildCoveredLog` stays under 1,000 bytes (measured 355 with the skip, 2,105 without) |
| 13 | Rule 2's boundary is `after`, not `after+1` | `TestTheLastCoveredRecordIsStillChecked` fails when `walkLog`'s `ordinal >= afterSeq` becomes `ordinal > afterSeq` |

### 7.1 The measurement, and a fixture that has to change first

`TestRestartCostTracksTheWholeLogNotTheTail` asks in its own comment to be inverted when
this lands. Inverting the assertion alone would produce a wrong result, and this is worth
spelling out because it is the kind of thing that gets committed.

`buildCoveredLog` places `prefix` buy limit orders that never match, so **all of them
rest**. Growing the prefix from 50,000 to 200,000 grows the log by 4× and grows the
recovered **book** by 4× as well. `Recover` = snapshot restore + log read, and the
snapshot restore term is O(book). With a perfect skip, that test would still show a
ratio near 4, and whoever ran it would conclude the fix does not work.

The same conflation is in the published numbers. `PRODUCTION-READINESS.md`'s "1,000
records → 4 ms, 100,000 → 428 ms, 500,000 → 1.47 s" rows and the 611 MiB figure are
totals over a fixture whose book grows with its log, and `BENCHMARKS.md` separately
records `RestoreSnapshot` at 171 ms for a 100,000-order book. So "read and CRC of a
100,000-record log is 214 ms against `Recover`'s 428 ms" is not a decomposition of one
number into halves; it compares two measurements that overlap. **Do not derive the
expected speedup from it.** Measure and publish.

So:

- Keep `buildCoveredLog` and `BenchmarkRecoverBehindACoveredPrefix` as they are. They
  measure O(book) + O(log) together, which is what a real restart costs.
- Add `buildCoveredChurnLog`, which writes submit/cancel pairs so the resting book stays
  near empty while the log grows. It isolates the O(log) term, which is the only term
  this slice touches.
- Invert the test against the churn fixture: **allocation for a 200,000-record covered
  prefix must be within 1.5× that for a 50,000-record one**, the same 1,000-record tail
  in both. Today that ratio is near 4. The failure message should say that a regression
  here means the skip stopped skipping.
- Publish, from a measurement on the churn fixture and not from arithmetic: `Recover`
  time and allocation at 50k/200k/500k covered records, before and after. Both tables
  go into `BENCHMARKS.md` with the fixture named, so the O(book) and O(log) terms are
  never added together again.

### 7.2 Sabotage runs required before this counts as done

Each of these is a deliberate break, run to confirm the test that should catch it does.
This repository has had a test pass against its own sabotage before, and that is the
only reason §7's claims are worth anything.

1. Replace the skip with `io.CopyN(io.Discard, r, int64(n))`, no CRC. Deliverable 3 must
   fail.
2. Delete the Rule 1 check. Deliverable 5 must fail.
3. Change the retention boundary from `after+1` to `after`. Deliverable 4 must fail, at
   every boundary, by double-applying one record.
4. Number error records by `len(out)+1` instead of by ordinal (Rule 5). Deliverable 3's
   error message must name the wrong record.
5. Revert `Recover` to `ReadAll` + `RestoreAfter`. Deliverable 8 must fail.

## 8. How this can fail, stated in advance

So §7's write-up is not graded on a curve.

- **The time saving could be much smaller than hoped.** If reading and verifying is most
  of the per-record cost and parsing is little of it, this buys the allocation and
  almost no time. The measurement in §7.1 decides it, and the number goes in whatever it
  says. The allocation win is not in doubt; the time win is a hypothesis.
- **The fallback could become the common path.** If real files trigger Rule 4 routinely,
  restarts get *slower* than before, because a fallback pays a skip pass plus a full
  pass. The report's `FellBack` field is there to make that visible rather than
  mysterious. If it happens, the diagnosis is that the invariant is weaker than §4
  claims, and the response is to find out why before making the fallback cheaper.
- **The snapshot-ahead report could be noise.** §4.1's window is reachable after an
  ordinary crash, so this may fire often enough that operators learn to ignore it, which
  is worse than not reporting it. If so, the fix is to sync the log before a checkpoint,
  and this report becomes rare and meaningful.
- **Rule 3's sampling could prove too thin.** Two checks on a headered log is a bet that
  the invariant breaks at the edges rather than in the middle. Slice 2's rotation breaks
  it at ordinal 1, which is checked. If some other producer breaks it in the middle,
  this design skips the wrong records and says nothing.

## 9. What building it changed — written after the code

### 9.1 The reader is `walkLog`, and it returns a struct

§2 called for `readFrom(path, afterSeq) ([]Entry, error)`. What exists is
`walkLog(path, afterSeq) (logWalk, error)`, where `logWalk` carries the retained
entries plus the four facts `RecoverWithReport` needs and cannot recompute afterwards:
the last complete record's ordinal, the last complete record's **sequence**, how many
records were skipped, and whether any parsed record's sequence disagreed with its
ordinal. `ReadAll` is `walkLog(path, 0)` and discards the rest, exactly as specified.

`lastSeq` calls it with a sentinel, `retainNothing` (`math.MaxInt64`): no ordinal
reaches it, so every record is read and verified, none is retained, and the walk still
learns the last record's sequence — from a second payload buffer that holds the last
complete record, so a torn read of the next one cannot destroy it.

One function does the reading, and the property that buys is narrower than an earlier
draft of this paragraph claimed. `ReadAll`, `Recover` and `Open` cannot drift apart on
**framing or on any checksum**: there is one loop, and no reader in the package accepts
bytes another would call corrupt on those grounds. They *do* differ on decoding, because
each decodes the records it hands back and no more — `ReadAll` every record, `Recover`
the tail it applies plus the anchor, `Open` record 1 and the last record. So `Open` will
open a file `ReadAll` refuses, when the difference is §5.2's CRC-valid non-record.

That is a deliberate ordering and not an accident of the sentinel. **`Open` has to be
the most permissive reader here.** Recovery's strictness moves with the snapshot
boundary; `Open` does not know where the boundary is. If `Open` were stricter than the
laxest `Recover`, a venue could recover its book successfully and then fail to open the
log it just recovered from — `cmd/obgw/server.go` does exactly that pair, `Recover` at
:278 and `Open` at :313 — turning a benign file into an outage by having two readers of
the same bytes disagree. What `Open` checks is what `Open` depends on, all of it: every
frame, every checksum, and the sequence of the record it is about to append behind.
`integrity_test.go:276`'s assertion that `Open` returns `ErrCorrupt` still holds for the
damage it injects, which is CRC damage.

### 9.2 Deleting the Rule 1 check did not fail the test that was supposed to catch it

§7.2's second sabotage run — delete the record-1 anchor, watch deliverable 5 fail —
**passed**. `TestSegmentStartingAtANonOneSequenceFallsBack` builds a 100-record
segment against a snapshot at 500, so the walk runs off the end of the file before it
reaches the boundary and parses the last record for its sequence anyway (Rule 2's
"there is still a parsed record to check against"). That check caught the segment, and
the anchor was never consulted.

That is a real gap in the test, not in the design. Rule 1 earns its place on a file
whose **head** disagrees and whose tail does not — the shape two writers interleaving
on one `O_APPEND` handle produce. So `TestOnlyTheFirstRecordDisagreeingStillFallsBack`
exists: record 1 carries sequence 1001, records 2..100 carry their own ordinals, the
boundary is at 50. Everything recovery parses agrees; only the anchor disagrees.
Without it, record 1 is discarded as covered when its sequence says it is not, and an
order that should be resting is silently absent. With the anchor deleted, that test
fails on all three of its assertions.

The third sabotage — moving the retention boundary from `after+1` to `after` — also
did not fail the way §7.2 predicted. It cannot double-apply, because `RestoreAfter`'s
`Seq <= afterSeq` filter drops the extra record independently; §2.5's insistence that
the sequence filter stays the authority over what the reader hands it turns out to be
load-bearing rather than decorative. It fails on the report's `Skipped` count instead,
which is what makes the boundary observable at all.

The qualifier matters and §5.1 now carries it. The filter is the authority in one
direction only: it can drop a record the walk retained, and it never sees a record the
walk discarded. Retaining one record too many is therefore safe; discarding one too many
is not, and nothing downstream would notice. That asymmetry is the whole reason Rules 1
to 4 exist, and it is why the sabotage that moves the boundary the *other* way has to be
caught by a count rather than by a book digest.

Sabotage runs 1, 4 and 5 failed exactly as specified.

### 9.3 The time win is ~26×, not ~2×

§5.3 argued that a seeking skip would save "memory bandwidth… not the part of the
428 ms that hurts", and §8 listed "the time saving could be much smaller than hoped"
as the first way this could fail. Both were reasoning from the `BENCHMARKS.md` row
labelled "`ReadAll` — read + CRC-verify the log". That label was wrong: `ReadAll`
decodes every record too, and the decode is nearly all of it. Taking the marginal
cost between the 50,000- and 500,000-record rows below, a covered record went from
~3.33 µs to ~106 ns.

Measured on `BenchmarkRecoverBehindACoveredChurnPrefix` (Apple M4, `-benchtime 1x
-count 5`, medians, page cache warm), 1,000 records to apply in every row:

| covered prefix | log on disk | before | after | alloc before | alloc after |
|---:|---:|---:|---:|---:|---:|
| 1,000 | 0.35 MiB | 6.2 ms | 3.4 ms | 3.4 MiB | 2.0 MiB |
| 50,000 | 8.93 MiB | 161 ms | 11 ms | 70.6 MiB | 2.0 MiB |
| 200,000 | 35.4 MiB | 639 ms | 37 ms | 277 MiB | 2.0 MiB |
| 500,000 | 88.4 MiB | 1.66 s | 64 ms | 772 MiB | 2.0 MiB |

Reading and checksumming is ~112 ns a record (88.4 MiB in 56 ms on a re-run of the
500,000 row). The allocation column does not move, which is what §1 promised. The label
is now corrected in `BENCHMARKS.md`, because the next person to plan against that row
would make the same mistake.

**The 1,000-record row is the one to read carefully, and it was published wrong.** It
first went out as "6.0 ms", with prose saying the row "barely improves because there is
almost nothing to skip there". Both were wrong. The fixture is 1,000 covered records
against a 1,000-record tail, so exactly half the log is skipped and the time nearly
halves, which is precisely what the model predicts — the skip pays back proportionally
from the first record skipped, not only at scale. Re-measured across two methodologies
(`-benchtime 1x -count 5` and `-benchtime 3x -count 7`), after lands at 3.4 ms with no
sample above 4.4 ms, against a before distribution that does not overlap it. A reader
planning slice 2 off the old row would have concluded the skip is worthless below tens
of thousands of covered records.

None of this makes restart time bounded. 88.4 MiB still has to be read, the file still
never shrinks, and §6's first bullet stands unamended.

### 9.4 One allocation the design did not anticipate

Once the parse stopped, what was left was eight heap bytes per record: `var lenbuf
[4]byte` and `var crcbuf [4]byte` declared inside the read loop, escaping because
`io.ReadFull` takes an `io.Reader`. Invisible against 2.1 KB a record; a third of what
remained afterwards, and enough to hold the flatness test at a ratio of 1.48 against a
threshold of 1.5. Hoisted to one `[8]byte` outside the loop, the ratio is 1.00.

Reusing the payload buffer on the `afterSeq == 0` path — which `ReadAll` and every
snapshotless recovery take — was not a stated goal and is a real improvement to both:
the old code allocated a fresh `make([]byte, n)` for every record on its way to
`json.Unmarshal`. `Entry` holds no slice aliasing the input (`encoding/json` copies
what it keeps), so the buffer is safe to reuse.

**What it is worth, measured on the path that has no skip in it at all**
(`-benchtime 3x -count 5`, medians):

| | before | after | |
|---|---:|---:|---:|
| `BenchmarkReadAll/100000` | 175.2 MB | 142.4 MB | −18.7% |
| `BenchmarkReplayTail/100000` — a snapshotless `Recover` | 235.0 MB | 202.2 MB | −14.0% |

Three allocations per record, about 328 bytes each. Worth having and no more than that.

*(This paragraph first claimed "a snapshotless 200,000-record recovery drops from
457 MiB to 107 MiB on that account alone". Those two numbers are
`BenchmarkRecoverBehindACoveredPrefix/covered200000`'s `B/op` before and after — a
**snapshotted** fixture whose reduction is overwhelmingly the covered-prefix skip — and
they are MB, not MiB: 456.6 MB is 435 MiB, which `BENCHMARKS.md` labels correctly two
tables away. The effect was credited to the wrong mechanism by a factor of about four,
which is exactly the kind of error that sends the next person optimising the
snapshotless path off a false baseline.)*

### 9.5 What review found, after the code and after §9.1–§9.4

Three independent adversarial reviews ran against the finished change: one on
durability and corruption, one on test integrity, one on the published measurements.
The first two built before/after trees and diffed behaviour; the third re-ran every
benchmark whose number this changeset publishes. What they found splits cleanly into
one thing the design had decided without noticing, one claim written wider than the
code, and three numbers that were simply wrong.

**A behaviour change §5 did not list.** Differential fuzzing — 800 generated logs
across eight mutation classes, two seeds — produced 41 files where the two builds
disagreed. Every one of them was the same class: a record complete on disk, valid CRC,
not valid JSON, strictly behind the boundary. 22 of the 41 turned a refusal to start
into a successful start. Nothing else diverged: CRC damage, damaged length prefixes
both in and out of bounds, torn tails, truncation mid-record, the §6.1
torn-fragment-then-append case, v1 headerless logs clean and damaged, empty and
header-only and absent logs, and snapshots at, behind and ahead of the log were all
byte-identical between the builds, as were the rotated-segment and duplicated-frame
shapes that trip the fallback. §5.2 now carries the bullet, and
`TestUndecodableRecordBehindTheSnapshotStartsTheVenue` pins it in four directions so a
later change cannot drift further without saying so.

**A claim wider than the code.** §9.1 said `ReadAll`, `Recover` and `Open` "cannot
drift apart on what counts as a complete, verified record". They cannot drift on
framing or checksums, which is the part that matters and the part one shared loop
guarantees. They differ on decoding, necessarily, because each decodes what it returns.
§9.1 now says which and argues why `Open` is deliberately the laxest of the three.

**Three published numbers.** The 1,000-record row of §9.3's table said 6.0 ms after,
against a measured 3.4 ms, with prose explaining the non-improvement that was not
there — corrected above, along with the same row in `BENCHMARKS.md` and
`PRODUCTION-READINESS.md`. §9.4 credited buffer reuse with a 4.3× reduction that was
actually the covered-prefix skip on a different fixture — corrected above and in
`CHANGELOG.md`. §6 carried the pre-change constant into a "still minutes" claim that
`PRODUCTION-READINESS.md` contradicted in the same changeset — corrected to the
measured ~16 s.

**Three coverage gaps, all now closed.**

Deleting `!checksummed` from `walkLog`'s parse predicate — the one term that keeps v1
logs decoding every record — passes every test in this file, including
`TestV1LogRecoversIdenticallyBehindASnapshot`, because a clean v1 log decodes
identically whether or not you look at it. *(The review that found this reported it as
passing the entire suite. It does not: `TestLegacyLogStillRecovers` in
`integrity_test.go` fails with "appended entry has seq 2, want 5", because the same
predicate feeds `lastSeq`. That is a v1 **append** check catching a v1 **recovery**
bug by luck of a shared code path, and the recovery property itself was unasserted.)*
`TestV1UndecodableRecordBehindTheSnapshotStillStops` asserts it directly, on the
recovered digest.

Changing that predicate's `ordinal >= afterSeq` to `ordinal > afterSeq` survived
everything, and it is not the neutral edit it looks like: it stops parsing the last
record the snapshot covers, which is half of Rule 2's stated reason for existing. A
sequence that disagrees with its ordinal at exactly `after` would then never be read
and the fallback would never fire. `TestTheLastCoveredRecordIsStillChecked` damages
ordinal 500 against a boundary at 500; the pre-existing test damages 600, which is
caught either way.

§7.1's move to the churn fixture left nothing asserting anything about a restart whose
book grows with its log. `TestARealRestartAllocatesForItsBookAndNotForItsLog` measures
the marginal allocation per covered record on the resting fixture — 355 bytes with the
skip, 2,105 without, a 6× separation and a threshold that will not flake, where the
obvious ratio form would have had to fit between 2.0 and 3.2.

Confirmed unchanged by the reviews and worth recording as such: allocation really is
flat, 2.10 MiB from 1,000 covered records to 500,000; the buffer reuse cannot alias a
retained `Entry`, because neither `Entry` nor anything in `pkg/types` has a `[]byte`,
`json.RawMessage`, `map` or `interface{}` field; no existing assertion anywhere in the
repository was deleted or weakened; and every document in the changeset that could have
claimed bounded restart time says the opposite instead.

# Log Rotation — Bounding the Log Itself, Not Just What Recovery Parses

Status: **built** — slice 2 of [`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md)
M3, written before the code as this repository does it ·
Author: Karthikeyan NG · Last updated: 2026-08-16

> **§12 records where the code disagreed with this document.** The set marker is
> written at the FIRST ROTATION rather than at the first `Open`, because §9's
> deliverable 16 and §7 cannot both be satisfied otherwise; §5.4's "open every segment
> before reading any" became a restartable read for a reason about file descriptors;
> §6.2's justification for reusing `ReasonHalted` turned out to rest on a claim that was
> false in the shipped code; §10's sabotage 4 passes, and why that is a real gap rather
> than a pass; and rotation costs 12.4 ms, which is a lot more than "probably invisible
> next to a checkpoint". §12.9 onwards are the second round, from an adversarial review
> of the finished code: free space is not sampled at rotation and should not be, Rule 9's
> contiguity check was never implementable, `-wal-retain` has a floor under it that the
> published sizing numbers ignored, and three defects the review found in the code
> itself.

> **The one decision this document exists to get right is §2.3.** Slice 1 skips a
> covered prefix by treating a record's ordinal in the file as its sequence, and it
> *verifies* that assumption — a disagreement discards the skip and re-reads the whole
> file. A rotated segment begins at ordinal 1 carrying sequence *k* ≫ 1. So the naive
> segmentation makes that fallback fire on every rotated log: correct book, two full
> passes instead of one, a venue slower to restart than before slice 1 existed, and the
> only trace is one log line saying the restart "was slower than it needed to be".
> §2.3 is the rule that stops it, and §9's `TestADeclaredSegmentDoesNotFallBack` is the
> test that would notice if it were quietly undone.
>
> A section recording what building this changed will be added after the code, in the
> shape of [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §9. Nothing below has been
> measured yet; every number in §9 is a target with a named fixture, and every measured
> number quoted from elsewhere says where it came from.

Companion documents:
- [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) — slice 1. §4's invariant, check and
  fallback are the constraints this design inherits; §6's first bullet is the sentence
  this slice is written to delete.
- [`RUNBOOKS.md`](RUNBOOKS.md) §"A corrupt log record" and §"A corrupt snapshot" — two
  operator procedures this changes. The second one it destroys, and §5.3 says so rather
  than letting an operator find out during an incident.
- [`PRODUCTION-READINESS.md`](PRODUCTION-READINESS.md) §"Running continuously" — the
  claim that a venue left running becomes unrestartable within days. Slice 1 lowered the
  wall; this is the slice that is allowed to say "bounded".
- [`REPLICATION.md`](REPLICATION.md) — `examples/replication/primary.go` reads the whole
  log to catch a reconnecting follower up. Retention can delete what it is about to
  read; §5.4 decides what happens instead.
- [`SOAK.md`](SOAK.md) §4 — "nothing rotates or truncates the file", and the disk-space
  headroom check in `.github/workflows/soak.yml` that exists because of it.

---

## 1. Why this exists

At roughly 220 bytes of journal per client message — the figure the soak workflow's
disk precheck already uses — a venue at 2,500 messages/s writes about **44 GiB of log a
day**, and never deletes any of it. Two consequences, and only the first is the one
people notice.

**The disk fills.** That is a hard stop, it arrives on a schedule, and today the venue's
response to it is undefined. §6 has the details; the short version is that ENOSPC
surfaces inside the 20 ms group-commit ticker, which logs the error and continues, so a
full disk produces a venue that keeps accepting and acknowledging orders that are no
longer being journalled. Nothing refuses. Nothing goes unready.

**The restart stays O(total history).** Slice 1 made recovery's *allocation* flat in the
covered prefix and cut its time by ~26× — 500,000 covered records went from 1.66 s and
772 MiB to 64 ms and 2.0 MiB. It deliberately did not bound anything, because reading
and CRC-verifying the file is still linear in the file and the file never shrinks. One
day at 2,500/s is 216 million records; at the measured 112 ns to read and verify one,
that is ~24 s, and the next day is 48, and it never stops climbing.

*(`BOUNDED-RECOVERY.md` §6 runs this arithmetic as "roughly 144 million records… about
16 seconds". 2,500 × 86,400 is 216 million, and 216 million × 220 bytes is the same
44 GiB that same bullet quotes — so the record count and the byte count in it disagree
with each other by 1.5×. Corrected here; correcting it there is on deliverable 19's list.
It does not change that bullet's conclusion, which is the point of it.)*

Rotation on its own fixes neither. A log split into 350 files a day is the same number
of bytes. **Retention is the mechanism; rotation is what makes retention safe**, because
the unit you can delete without a partially-deleted file is a whole file, and the unit
you can archive is a whole file. So this slice is: cut the log into segments that
declare where they start, delete a prefix of them under a predicate that cannot outrun
the snapshot, and define what the venue does when the disk fills anyway.

The measurable claim at the end is in §9.1: **restart cost becomes O(retained log)
rather than O(total history)**, shown by a benchmark that grows total history 10× while
holding retention fixed and requires the recovery time not to move.

## 2. The segment set

### 2.1 Layout and naming

The path an operator passes to `-wal` names a **set**, not a file. Segments are its
siblings, in the directory it already lives in:

```
/var/lib/obgw/BTC-USD.wal                        the set marker (§2.5), 18 bytes, no records
/var/lib/obgw/BTC-USD.wal.0000000000000001       segment, base sequence 1
/var/lib/obgw/BTC-USD.wal.0000000000610422       segment, base sequence 610,422
/var/lib/obgw/BTC-USD.wal.0000000001220913       segment, base sequence 1,220,913  ← active
/var/lib/obgw/BTC-USD.snap                       the snapshot, untouched by any of this
```

The suffix is the segment's **base sequence** — the `Seq` of its first record — in
decimal, zero-padded to 16 digits. Zero-padded so lexical order is numeric order, which
means `ls` and a directory read return the set in the order recovery wants without a
sort key that has to be parsed. Sixteen digits covers 10^16 records, about 126,000 years
at 2,500/s; a base that needs a seventeenth digit is refused at rotation rather than
silently changing the name's shape.

**Enumeration is an allow-list, never a glob.** A file joins the set only if its name is
exactly `<stem>.` followed by 16 ASCII digits. This is not fastidiousness. `BTC-USD.snap`
sits in that directory, `WriteSnapshot` drops `BTC-USD.snap.tmp` there while it works,
`cmd/obgw` writes `venue.json` there, and `examples/multisymbol/main.go:392` derives its
snapshot path as `walPath + ".snap"` — so a `<stem>.*` glob would hand a snapshot to a
frame parser and report the venue's log as corrupt. The strict pattern excludes all four
by construction, and it excludes tomorrow's `.gz`, `.bak` and `.2026-08-16` too.

Why siblings rather than a directory named by the path: `-wal /tmp/soak.wal` has to keep
working, and it names a file in every published invocation ([`SOAK.md`](SOAK.md) §3,
[`PROTOCOL.md`](PROTOCOL.md), the soak workflow). Turning that path into a directory
breaks every existing deployment on the upgrade, and — worse — a WAL path that is a
directory *recovers today as a clean empty log*: `walkLog` opens it, the first read
fails, the loop breaks, and `Recover` returns a fresh engine and a nil error. Any
embedder that calls `Recover` without a following `Open` starts an empty venue and says
nothing. That is fixed here independently of the layout (§4.4), but choosing a layout
that walks toward it would be perverse.

### 2.2 The segment header

A segment written by this build begins with 18 bytes:

```
offset 0    "OBWAL\x02"      6 bytes   magic and version
offset 6    [base:8]         big-endian int64: the Seq of record 1
offset 14   [crc:4]          CRC-32C over the 8 base bytes
            [len:4][crc:4][payload]     record 1
            ...
```

Records are unchanged: same framing, same CRC-32C over the payload only, same
`MaxRecordBytes`. Nothing about a record's bytes depends on which segment it is in, so a
segment's records are byte-identical to what the current build would have written at the
same point in the stream.

The header CRC covers the base and nothing else. It is four bytes to make the one number
the whole design rests on self-checking: a bit flip in a base sequence, left undetected,
would shift a whole segment's sequence space and either double-apply or silently drop a
run of commands.

`base == 0` is reserved and means "not a segment" — see §2.5.

**Three ways to declare a base, and they must agree.**

1. The filename's 16 digits.
2. The header's `base` field.
3. Record 1's `Seq`.

*Rule 1 — the name and the header must be equal; a disagreement is `ErrCorrupt` and the
venue refuses to start.*
*Reason:* they are written by one process in one operation, so they can only disagree if
the file was renamed, copied, restored from a backup, or written by something else. The
reader cannot tell which of the two is the lie, and both choices are bad: trusting the
name puts records into the wrong sequence space, trusting the header puts the set in the
wrong order. This is the answer to the obvious objection against putting the base in the
filename — that a rename produces a file which lies about itself with nothing to
contradict it. Now there is something to contradict it, and the error names both numbers
so an operator can see which one moved.

*Rule 2 — record 1's `Seq` must equal the declared base, and every parsed record's `Seq`
must equal `base + ordinal − 1`. A disagreement sets `suspect` and recovery falls back to
the full-parse path, exactly as slice 1 does today.*
*Reason:* this is [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §4's Rules 1 and 3,
rebased. It keeps meaning what it means today — "the contents of this file are not what
the file claims" — instead of quietly coming to mean "this file was rotated". §2.3 is
about the difference.

### 2.3 How this keeps slice 1's arithmetic working

Slice 1's reader (`walkLog`) does two pieces of arithmetic per record:

```go
parse  := afterSeq == 0 || !checksummed || ordinal == 1 || ordinal >= afterSeq
retain := ordinal > afterSeq
...
if e.Seq != ordinal { w.suspect = true }
```

Each of those is `ordinal` standing in for the record's sequence. Per segment, with a
declared base, they become:

```go
seq    := base + ordinal - 1
parse  := afterSeq == 0 || !checksummed || ordinal == 1 || seq >= afterSeq
retain := seq > afterSeq
...
if e.Seq != seq { w.suspect = true }
```

For a single-file log, `base == 1`, `seq == ordinal`, and every expression is
character-for-character today's. That is the backward-compatibility argument and it is
also the correctness argument: the skip's invariant was never really "ordinal equals
sequence", it was "the file declares where its sequence space starts and the records
agree", and a single file declared it by being the only file. Rotation does not weaken
the invariant; it makes the declaration explicit.

**What this buys, stated as the failure it prevents.** Derive the base by parsing record
1 instead — the tempting shortcut, since record 1 is parsed anyway — and the arithmetic
above still works, on a good file. It stops working on the file the check exists for: a
segment whose record 1 has been altered, or two writers' records interleaved under
`O_APPEND`, or a segment restored from a different venue. Deriving the base from the data
means the data can never disagree with itself, so `suspect` can never fire on a rotated
log, so slice 1's Rule 1 anchor is *deleted in effect* while appearing to be present.
The declared base is what makes the cross-check a cross-check.

**And the trap in the other direction.** With this design,
`TestSegmentStartingAtANonOneSequenceFallsBack`
(`bounded_recovery_test.go:478`) must be re-aimed, not deleted. Today it hand-builds a
headered file whose records carry `Seq = 1000 + ordinal` and asserts `FellBack == true`.
After this change that file still declares base 1 — it has a `\x01` header and no base
field — so its records still disagree with their declared sequences and it must still
fall back. Its name changes to say what it actually guards
(`TestAFileThatDeclaresBaseOneAndCarriesSequence1001FallsBack`) and its assertion is
unchanged. Beside it goes the new one:

> `TestADeclaredSegmentDoesNotFallBack` — a properly rotated set whose second segment
> declares base 1001 and whose records carry 1001…, recovered against a snapshot inside
> that segment, must produce the same book as the equivalent single file **and must
> report `FellBack == false`**.

If the implementation is wrong in the way §0 warns about, that test is the only thing in
the repository that fails. Every other test still passes, the recovered book is still
correct, and the venue is slower than it was before slice 1. This is why that assertion
is written as a deliverable in §9 and sabotaged in §10.

### 2.4 The four alternatives, and why each was rejected

**Base in the filename only.** Free — no format change, nothing to migrate. Rejected as
the sole authority for the reason in §2.2 Rule 1, and for a worse one: an older build
pointed at `wal.0000000000000042` reads it happily (it is a valid `\x01` file), sets
`suspect`, falls back, recovers a partial book, and then **appends into it**, reusing
sequences 43 onward. A silent wrong answer on a downgrade is the exact failure class
this whole milestone exists to remove. Kept as the *ordering and enumeration* mechanism,
because it is the only one that lets a reader sort and select segments without opening
any of them — which §5.1 turns out to need.

**Base in the header only.** Correct, and it makes enumeration O(open every file). At
350 segments a day that is a directory read plus 350 opens on every recovery, every
retention pass, and every follower reconnect, to learn something the name could have
carried for free. Kept as the *authority*, paired with the name.

**A base-sequence record at the head of each segment.** Rejected on the stated
requirement — a reader would have to parse a record to learn the base, which is the thing
that must not be necessary — and independently vetoed by `boundary_test.go:105`, which
asserts `len(entries) == n` on the premise "one journal record per command, so record k
is boundary k". A header record breaks that premise for every consumer downstream,
including `follower.go`'s `e.Seq == f.applied+1`.

**A sidecar manifest** (`wal.index`: segment → base, last, count, digest). Buys startup
validation and retention decisions without opening a segment — but so does the name —
and costs a second durable artifact that can disagree with the directory after a crash
between sealing a segment and updating the index, plus a second thing an operator can
copy without. Strictly more state than name-plus-header, and it lies in exactly the same
way the name alone does.

### 2.5 The set marker, and the downgrade it makes loud

The stem path itself holds an 18-byte **set marker**: the `\x02` header with `base == 0`
and its CRC, and no records. It is not a segment and contributes nothing to recovery.

*Rule 3 — a set this build creates always has a marker at the stem.*
*Reason, and it is the whole reason:* without it, an older build pointed at a rotated
set finds no file at `-wal /tmp/soak.wal`, concludes there is no log, starts an **empty
venue**, and begins writing sequence 1 next to segments that already hold four hundred
million records. With it, the old build peeks six bytes, does not find `OBWAL\x01`,
treats the file as a headerless v1 log, reads `"OBWA"` as a length prefix of
1,329,747,777 bytes, and refuses with `wal: corrupt record: record 1 declares 1329747777
bytes (limit 8388608)` from both `ReadAll` and `Open`. A loud refusal on a downgrade,
where the alternative is a silent empty venue.

*(That constant is `binary.BigEndian.Uint32("OBWA")`. The survey this design was written
from had it as 1,329,873,729, which is not what those four bytes are; it is written here
because the runbook line below quotes it and an operator grepping for the wrong number
finds nothing.)*

The message routes an operator into `RUNBOOKS.md` §"A corrupt log record", which will
tell them to check SMART data for what is actually a version mismatch. Two things
follow, and both are deliverables: the **new** build must recognise `OBWAL\x02` where it
does not expect it and say "this file was written by a newer format", and the runbook
gains a line saying that a 1,329,747,777-byte record means a downgrade, not a disk.

A marker is also the answer to "does this path belong to a venue?", which `os.Stat` on a
directory cannot answer and which `examples/replication`'s `Promote` gets wrong today
(§8).

## 3. Rotation

### 3.1 When

Rotation is decided in `Writer.append`, under `w.mu`, immediately before the record's
header is written, and nowhere else.

```
rotate if  w.written + recordBytes > MaxSegmentBytes  and  w.records > 0
```

`MaxSegmentBytes` defaults to **128 MiB** — about 610,000 records at 220 bytes, roughly
four minutes at 2,500/s, and roughly 350 segments a day at that rate. Small enough that
retention and archival have useful granularity and an archived unit is a manageable
object; large enough that the two fsyncs and the directory fsync a rotation costs are
amortised over minutes rather than seconds.

The `w.records > 0` term matters: a single record may legitimately be up to
`MaxRecordBytes` (8 MiB), and a limit set below that must produce an oversized segment
rather than an infinite rotation loop. Stated so that a `-wal-segment-bytes 1MiB` in a
test does not become a hang.

*Rule 4 — rotation happens on the appending goroutine, inside the same critical section
that assigns the sequence, and never on a timer or in a background goroutine.*
*Reason:* `Sync` takes `w.mu`, flushes the buffer and fsyncs the fd. A rotation that
could land between those two lines would flush buffered bytes to the old fd and fsync
the new one, and the venue would report durable a set of records that are not — after
which `cmd/obgw`'s 20 ms `syncLoop` and `-sync-every-command`'s per-command fsync are
both racing the appender. Putting rotation under the same lock makes that structurally
impossible rather than carefully avoided. It also means there is exactly one place where
`w.f` and `w.w` change, which is what `SetOnAppend`'s "in log order" guarantee needs.

### 3.2 The steps

With `base = w.seq + 1` computed under the lock that just refused to assign it:

1. **Flush** the buffered writer into the active segment.
2. **fsync** the active segment's fd.
3. **Create** `<stem>.<base>.tmp` with `O_CREATE|O_EXCL|O_WRONLY`, write the 18-byte
   header, fsync it.
4. **Link** it into place: `link(tmp, <stem>.<base>)`, which fails with `EEXIST` if that
   segment already exists.
5. **Unlink** the temp name. The fd from step 3 still refers to the inode, now reachable
   as the segment.
6. **fsync the directory**, so both the new segment's name and the temp's removal
   survive.
7. **Close** the old segment's fd, swap in the new fd and a fresh `bufio.Writer`, set
   `w.base = base` and `w.written = 18`.
8. **Write the record** — header then payload, into the new segment, as usual.

Then `onAppend` fires, once, for that record, as it always did.

Steps 3–5 are a rename dance where `os.Create` would do, and the reason is the crash
matrix below: it removes the state "the segment exists and its header does not" from
existence, rather than requiring every reader to have a rule for it. The alternative —
create, write the header, and teach recovery that a segment shorter than 18 bytes is an
empty segment it should rewrite — was rejected because it makes a *reader* repair a file,
and a reader that writes is a reader that can turn a diagnosis into damage. The cost is
one extra directory operation every few minutes.

`link` rather than `rename` because `rename` overwrites. `EEXIST` here means something
else is writing this set (§8), and the correct response is to fail the append, which
halts the engine, rather than to overwrite a file that may hold records.

*Rule 5 — a rotation that fails at any step fails the append: `w.seq` rolls back, the
record is not written, `onAppend` does not fire, and the error reaches
`Runner.logCommand`, which halts the engine.*
*Reason:* `append` already rolls back on its two other failure paths, and the property
that matters to a log shipper is that a sequence handed to `onAppend` is a sequence that
exists. Half-rotating and continuing would give a follower a record the primary never
journalled.

### 3.3 What a crash at each step leaves

| Crash after | On disk | What the next start does |
|---|---|---|
| before 1 | Old segment with buffered records possibly absent; no new segment | Ordinary torn tail in the newest segment. §3.4 |
| 1, before 2 | Bytes written but not fsynced: the tail of the old segment may be short or torn | Same as above — the newest segment's torn tail is sealed, and the next record starts a new segment |
| 2, before 4 | Old segment sealed and durable; a stray `<base>.tmp` | The temp is not in the set (it fails the 16-digit pattern) and readers ignore it. `Open` removes it, then rotates because the newest segment is at the limit |
| 4, before 6 | Both files exist; the directory entry may not survive | Either the segment is there (empty, base declared, legal as the newest member) or it is not (identical to the row above). No third state |
| 6, before 8 | Sealed old segment, empty new segment | `Open` appends into the empty new segment. Its base already equals `last(previous) + 1`, so the contiguity check passes |
| during 8 | New segment with a torn first record | Torn tail in the newest segment; §3.4 |

The row that does not appear is "segment created, header missing", because steps 3–5
make it unreachable. That is the entire justification for those three steps and it is
worth the paragraph: an 18-byte-short file whose name declares a base is exactly the
shape that a reader would have to guess about, and guessing about the base is guessing
about which commands exist.

### 3.4 What must never be split, and one defect this closes

*Rule 6 — a record never straddles a segment boundary.*
*Reason:* `append` writes the 4- or 8-byte frame header and the payload as two separate
buffered writes. A rotation between them leaves a header in segment *n* whose payload is
in segment *n+1* — unreadable by any reader in this package, and indistinguishable from a
torn tail followed by a segment of garbage. Deciding rotation before the header write is
what makes it impossible; §10's second sabotage moves the decision between the two writes
and requires a test to fail.

*Rule 7 — a segment's records are sealed before the next segment takes a record.* Steps
1, 2 and 6 precede step 8.
*Reason:* `examples/replication/primary.go:200` syncs and then reads the log to catch a
reconnecting follower up, on the argument that "entries appended before now must
therefore all be readable on disk". That argument is false the moment a `Sync` on the
active segment leaves buffered bytes in a segment the writer has moved on from. It is
also what makes the crash matrix's first three rows benign.

*Rule 8 — `Open` never appends behind a torn tail. If the newest segment ends in a
partial record, it is sealed as it stands and the next record begins a new segment based
at `lastSeq + 1`.*
*Reason:* this closes [`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §6.1, a defect
confirmed by experiment and left open there for want of a safe fix. Today `Open` takes
`O_APPEND` and writes the next record *behind* the fragment's bytes; the fragment's
length prefix then finds enough following bytes to look complete, its CRC fails, and the
venue refuses to start on the *second* restart after a crash. §6.1 said the fix was to
truncate the torn tail, and said truncating in a recovery path needs its own spec because
the fragment is sometimes the only evidence a command was attempted. Segmentation gives a
fix that truncates nothing: leave the fragment where it is, stop writing to that file
forever, and start a new one. The evidence survives, `ReadAll` still reports the file
exactly as it is, and no reader is ever asked to parse bytes written after a gap.

This is the one place where the design deliberately produces a **sealed, non-final
segment with a torn tail**, so §4.3 has to say when that is legal.

## 4. Recovery across a set

### 4.1 Ordering and enumeration

`walkSet(stem, afterSeq)` replaces `walkLog(path, afterSeq)` as the reader behind
`ReadAll`, `Recover` and `lastSeq`.

1. Read the directory once. Take the stem if it is a regular file, and every sibling
   matching `<base>.` + exactly 16 digits. Anything else is not in the set, including
   `.tmp` leftovers.
2. Sort by base, ascending, from the name. No file is opened to do this.
3. Open each in order, read and verify the 18-byte header, and cross-check name against
   header (§2.2 Rule 1).
4. Validate the set as a whole (§4.4) **before walking any records**, so a structural
   problem is reported as a structural problem rather than as a corrupt record 400,000
   files later.
5. Walk each segment's records in order, with the per-segment arithmetic of §2.3,
   concatenating retained entries.

A segment with no `\x02` header is a legacy artifact and has **implicit base 1**: a v1
headerless file, or an `OBWAL\x01` file. If it carries a name, that name must say 1.

### 4.2 The walk, and what `ReadAll` still promises

`ReadAll(stem)` returns every retained record of the whole set, concatenated in base
order, in one flat slice. That contract is not negotiable: `boundary_test.go:105`
asserts its length, `boundary_test.go:121` and `:230` slice it by ordinal,
`checkpoint_test.go`, `runner_recovery_test.go`, `cmd/obgw/synclog_test.go:34` and the
godoc `Example` in `example_test.go` (whose `// Output:` is checked) all depend on it,
and `RUNBOOKS.md:411` sends an operator to it when a client and the venue disagree about
an order.

One qualifier has to be added to the runbook and to the godoc, and it is the price of
retention: `ReadAll` returns what is **still on disk**. Once retention has deleted a
prefix, the first entry's `Seq` is the retention floor, not 1. An operator asking "what
happened to order X" needs to be told that the answer may be in the archive.

`Recover` and `Open` keep their signatures. `Recover` gains nothing and loses nothing
except the sentence in its doc comment about the file never shrinking.

`lastSeq` walks only the **newest** segment, which is what `Open` resumes from.

*Rule 9 — `Open` fully verifies the newest segment's records; of every other segment it
validates only what the DIRECTORY says (name, header, base, and that no two segments
declare the same base) and reads none of their records. It does not check contiguity,
and cannot: contiguity needs the previous segment's last record SEQUENCE, which only
comes from reading its records. `Recover` and `ReadAll` check it; `Open` alone is not an
integrity check. §12.10.*
*Reason:* this is the decision `BOUNDED-RECOVERY.md` §9.1 left open when it argued that
"`Open` has to be the most permissive reader here". The argument was about `Recover` and
`Open` disagreeing on the same bytes and manufacturing an outage; it did not say whether
`Open` must re-verify sealed segments. It must not, for a plain reason: `cmd/obgw` calls
`Recover` and then `Open` on the same path, and `Recover` has just read and verified
every retained byte. A second full pass would double the restart cost this slice exists
to bound, to check something that was checked seconds ago. What `Open` does check is
every frame and checksum of the file it is about to append behind, plus the one
structural fact the directory can answer on its own: that no two segments claim the same
base. `integrity_test.go:276`'s assertion that `Open` returns `ErrCorrupt` for CRC damage
is unaffected: its log is one segment, which is the newest.

The first draft of this rule also claimed contiguity, and that part was never
implementable — see §12.10. The consequence for a caller is that `wal.Open` on its own
does not tell you the set is joinable: a missing middle segment, an overlap or a
CRC-damaged sealed segment all leave `Open` succeeding and `Recover` refusing. Anything
that opens a set it did not first `Recover` or `ReadAll` is extending a set it has not
checked.

### 4.3 Torn tails

*Rule 10 — a torn tail is legal in the newest segment, and legal in any segment that is
immediately followed by one whose base is exactly `last + 1`. Anywhere else it is a gap
and the venue refuses to start.*
*Reason:* Rule 8 creates sealed middle segments with torn tails on purpose, and that is
safe precisely because the next segment declares that it resumes where this one actually
stopped. The same check that makes it safe is the check that catches a segment truncated
by a filesystem, a partial copy, or an operator's `head -c`. So the rule is not a
special case for Rule 8; it is one contiguity test that Rule 8 happens to satisfy.

### 4.4 Startup validation

Six conditions, each with its own error and its own message, checked before any record is
read. Every one of them refuses to start rather than reporting, and the reason is the
same in every case: the failure mode is a book that is missing commands and looks fine.
`RestoreAfter` applies whatever it is handed whose `Seq` is past the snapshot, so a set
with a hole produces a plausible, wrong book with no error anywhere.

| Condition | Test | What it means |
|---|---|---|
| **Name/header disagreement** | declared base ≠ 16 digits in the name | The file was renamed, copied or restored. §2.2 Rule 1 |
| **Gap** | `base(S₍ᵢ₊₁₎) > last(Sᵢ) + 1` | A segment is missing from the middle, or one was truncated below its recorded span |
| **Overlap** | `base(S₍ᵢ₊₁₎) ≤ last(Sᵢ)` | Two files claim the same sequences. The shape two concurrent writers produce |
| **Duplicate base** | two names with the same 16 digits | Impossible by name; reachable through a header disagreement, and named separately so the message is useful |
| **Snapshot below the floor** | `snap.WALSeq + 1 < base(S₁)` | **The one that matters most.** See below |
| **No snapshot, floor above 1** | `snap == nil ∧ base(S₁) > 1` | The same condition with `WALSeq = 0` |

**Snapshot below the floor** is the tripwire for every possible retention bug, and it is
the reason §5's predicate is written as a predicate. The snapshot is the base; the
retained log is the tail; if the oldest retained sequence is more than one past the
snapshot's `WALSeq`, then the commands in between exist in no file the venue can read,
and recovery would produce a book that skipped them. The message names both numbers:

```
wal: log gap: snapshot covers through sequence 412,000 but the oldest retained
segment starts at 610,422 — sequences 412,001..610,421 are in no file this venue
can read. See docs/RUNBOOKS.md "A gap between the snapshot and the log".
```

Refusing here is what converts "retention deleted something it should not have" from a
silently wrong book into an outage with two numbers in it. Every sabotage in §10 that
weakens the retention predicate is caught by this check, which is why it is specified
before the predicate is.

**A set with no records** is legal and is three distinct shapes, none of which is an
error:

- Nothing at the stem and no siblings: no log. Recovery yields the snapshot alone, or a
  fresh engine. Unchanged from today, and it is the case `RecoverReport.SnapshotAhead`
  deliberately does not report.
- A marker and no segments: a venue owned this path and has written nothing. Recovery is
  the same; `present` is true, so a snapshot ahead of it *is* reported.
- A marker and one empty segment (18 bytes, base declared): the state step 6 of §3.2
  leaves, and the state a fresh `Open` creates. `lastSeq` is `base − 1`.

**A path that is the wrong kind of thing** is an error, and this is the L1 fix promised
in §2.1: a stem that is a directory, a segment that is not a regular file, a stem whose
directory cannot be read. Today a directory passed to `Recover` yields a fresh engine and
a nil error. After this it yields `wal: <path> is a directory, not a log`.

### 4.5 Why there is no segment digest in this slice

[`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §5.3 named "segment-level verification" as
"the honest route to flat time" and parked it here: a per-segment digest over its
records' checksums, verified once, letting recovery genuinely skip a covered segment.
M3's WAL list has it too. It is not in this slice, and the reason is that it does not do
what that sentence hoped.

A digest over a segment's record checksums can only be verified by computing those
checksums, which means reading every byte of the segment — the exact cost it was supposed
to remove. Verifying the digest *without* reading the segment means trusting a number in
a footer, which is precisely the trade §5.3 rejected at record granularity: the venue's
promise silently becomes "we verify the log, except the part we do not read", and a
checkpoint moves the boundary forward so the unread part only ever grows.

What actually bounds the read is retention, and retention bounds it to a number the
operator chose. A digest would reduce a bounded constant while removing a check. So:
every retained segment is read and CRC-verified on every recovery, exactly as slice 1
promised, and restart cost is O(retained bytes) with the constant slice 1 measured.

A segment digest still has a job — verifying an archived segment years later without the
venue, which is `walverify`'s job — and it belongs with the CLI tools in the next slice,
where the question "what does a digest mismatch mean for a file nothing is going to
replay" can be answered on its own.

## 5. Retention and archival

### 5.1 The predicate

Let `base(S)` be a segment's declared base and `last(S)` its final sequence. For every
segment except the newest, `last(Sᵢ) = base(S₍ᵢ₊₁₎) − 1`, which is known from the
directory listing alone — **retention never opens a segment**, and that is a direct
consequence of putting the base in the name.

```
deletable(S) ≡
     sealed(S)                                           (a)
  ∧  ∃ snap on disk : verified(snap) ∧ snap.WALSeq ≥ last(S)   (b)
  ∧  |sealed segments newer than S| ≥ MinSegments        (c)
  ∧  ∀ S′ with base(S′) < base(S) : deleted(S′)          (d)
  ∧  ∀ connected shipper c : position(c) ≥ last(S) ∨ bootstrappable(c)   (e)
  ∧  archived(S) whenever ArchiveDir is set              (f)
```

**(a) sealed, never the active segment.** Not only because it is being written, but
because `Open` derives the next sequence from the newest segment. Delete it and the venue
restarts its sequence space underneath a snapshot that already claims those numbers —
the §4.1 hazard of `BOUNDED-RECOVERY.md`, but permanent and self-inflicted. **At least
one segment always remains.**

**(b) `WALSeq ≥ last(S)`, not `≥ base(S)`.** Partial coverage of a segment is no
coverage: the records above `snap.WALSeq` are unrecoverable the moment the file is gone,
and §4.4's floor check would refuse to start. `Checkpoint` stamps `lastApplied` rather
than `Writer.Seq()`, so this comparison errs in the safe direction by construction.

**`verified(snap)` means read back from disk and checksum-checked, not `os.Stat`.** A
snapshot that exists and fails its CRC is refused by `ReadSnapshot`, and recovery falls
back to the log — so gating retention on existence rather than verifiability deletes the
fallback for a snapshot that cannot be used. Re-reading the snapshot file immediately
after writing it costs one sequential read per checkpoint interval, off the matching
goroutine, and it buys the difference between "we believe we wrote it" and "it is there".

**(c) a floor of `MinSegments`** sealed segments (default 4) kept regardless of coverage.
Not a correctness term — (b) is — but an operational one: it keeps recent history on disk
for the forensics `RUNBOOKS.md` assumes, and it keeps a reconnecting follower's catch-up
in the common case out of the snapshot-bootstrap path in (e).

**(d) a prefix, never a hole.** Retention deletes the oldest segment or nothing. A
missing middle segment is the shape §4.4 exists to catch, and it must never be
self-inflicted.

**(e) the log-shipping consumer.** §5.4.

**(f) archival before deletion**, when configured. §5.2.

### 5.2 Ordering, and the crash between

The sequence, run from `cmd/obgw`'s checkpoint loop immediately after a successful
`WriteSnapshot`, never on the matching goroutine:

1. `ReadSnapshot(snapPath)` — the same file that was just written, read back and
   verified. If it does not verify, retention does nothing this cycle and logs it.
2. Enumerate the set from the directory. Evaluate the predicate. Nothing is opened.
3. For each deletable segment, **oldest first**:
   a. If `ArchiveDir` is set: copy the segment into `ArchiveDir` under the same name,
      fsync the copy, fsync `ArchiveDir`. A failure here stops retention for this cycle;
      the segment stays.
   b. `unlink` the segment.
4. fsync the set's directory once, after the last unlink.

**The ordering that matters is that the snapshot is durable before the first unlink.**
`WriteSnapshot` already gives that: temp file, fsync, rename, directory fsync. Retention
reads it back afterwards. So there is no window in which a segment is gone and the
snapshot covering it is not on disk.

**A crash between the snapshot and the deletion** leaves everything: retention re-runs on
the next checkpoint. **A crash between two deletions** leaves a prefix of length *k* for
some *k* less than intended, which is a valid set — that is the entire reason deletion is
oldest-first, and it is the property that makes step 4's single directory fsync
sufficient. **A crash between an archive copy and the unlink** leaves the segment in both
places; the archive is idempotent by name and the next cycle overwrites or skips it.

The one state that is not benign is a crash *inside* an archive copy, leaving a truncated
file in `ArchiveDir` under a legitimate name. So the copy is written as
`<name>.partial` and renamed, the same shape as `WriteSnapshot`, and an archive reader
ignores anything not matching the 16-digit pattern. This is the same rule as §2.1 and it
is stated again because an archive directory is exactly where a second, looser
enumerator gets written by someone in a hurry.

### 5.3 What retention destroys, and what replaces it

`RUNBOOKS.md` §"A corrupt snapshot" currently says: *"Delete the snapshot and restart.
Recovery falls back to replaying the log from the beginning, which is slower but it is
exact."* Under retention that procedure produces a venue that refuses to start (§4.4's
floor check) — or, if the check were missing, a book missing every command below the
retention floor.

This is the sharpest conflict in the slice and it is decided here rather than discovered
in an incident. **The runbook changes, and the drill
`TestDrillCorruptSnapshotProcedureRestoresTheSameBook` (`cmd/obgw/drills_test.go:100`)
changes with it.** The new procedure:

1. Before deleting anything, `ls` the segment set. The oldest segment's name is the
   retention floor, in decimal, with no tooling required. That number is the whole
   diagnosis.
2. **If the floor is 1** — a venue whose retention has never fired, which is every venue
   running with the default configuration (§5.5) — the old procedure is still exactly
   right. Delete the snapshot, restart, replay from the beginning.
3. **If the floor is above 1**, the snapshot is the only base the retained log can be
   joined to. Deleting it destroys the book. Restore the archived snapshot and the
   archived segments from the floor onward, or fail over to a node that has the state.
4. If neither exists, the honest answer is that the venue's recovery point is the last
   good snapshot and there is not one — which is a statement about the backup policy,
   and the runbook should say so in those words rather than offering a procedure that
   cannot work.

The deeper answer is that **a venue running retention without archival has a recovery
point equal to its newest snapshot, and one corrupt snapshot away from nothing.** That
sentence belongs in `PRODUCTION-READINESS.md` next to the RPO/RTO table, and it is the
argument for `-wal-archive` being the first flag an operator sets after `-wal-retain`.

Keeping *two* snapshots so that one may always be thrown away is the other obvious fix.
It is not in this slice — it is a change to the snapshot's on-disk layout and to
`WriteSnapshot`'s atomic-replace argument, both explicitly out of scope — and it is named
in §8 as the follow-up it is.

### 5.4 The log-shipping consumer

`examples/replication/primary.go:206` reads the whole log with `ReadAll` and filters
`Seq <= h.Have` to catch a reconnecting follower up. Three things change, and the first
two are required by this slice rather than optional.

**Required: `ReadAll` over the whole set.** Otherwise the primary ships a partial
catch-up starting at some segment's base, the follower's gap check
(`follower.go:109-112`) sees `e.Seq > applied+1`, and it terminates with `gap in the
feed`. It fails loudly rather than silently, which is the safety net working — but a
follower that kills itself on every rotation is not a venue, and "protocol error" is the
wrong diagnosis to hand an operator for "the primary rotated its log".

**Required: bootstrap instead of a partial catch-up when the follower is below the
floor.** If `h.Have + 1 < base(S₁)`, the records the follower needs are archived or gone.
The primary must answer with a snapshot — the same `Checkpoint()` path it already uses
for `h.Have == 0` — and not with the records it happens to still have. Shipping a set
that starts above `Have + 1` is exactly the gap the follower would refuse, arriving from
the one source that is supposed to be authoritative. This is the same answer market data
already gives an evicted subscriber (`MDRejectEvicted`, `RUNBOOKS.md` §"A subscriber has
fallen off the retention ring"), so it is a shape operators already know.

**Required, and cheap: tolerate a segment vanishing mid-read.** Retention runs in the
same process on the checkpoint cadence. A catch-up read that enumerates and then opens
can race an unlink between the two. On POSIX an already-open fd survives the unlink, so
the fix is to open every selected segment before reading any of them, and to treat
`ENOENT` on open as "re-enumerate, and bootstrap if the floor has moved past me". No
lease, no refcount, no lock. Term (e) of §5.1 is satisfied by making the reader
restartable rather than by making retention wait.

`p.wal.Seq()` and the live fanout are unaffected: both are in-memory and keyed on
sequence, not on files.

### 5.5 Defaults, and why deletion is not one

**Rotation is on by default.** It changes where bytes live and nothing about what they
say, and a venue that does not rotate cannot ever be bounded.

**Deletion is off by default.** `-wal-retain` defaults to zero, meaning keep everything,
meaning today's behaviour with better file names. Deleting a venue's journal is not a
default anybody should get by upgrading, and the failure mode of getting it wrong is the
one thing in this repository that cannot be undone.

The venue says so at startup, in the voice `cmd/obgw` already uses for "no -wal path" and
"no -admin address":

```
obgw: -wal-retain is unset — the log will grow without bound (about 44 GiB a day at
2,500 msg/s) and restart time grows with it. Set -wal-retain and -wal-archive.
```

Flags: `-wal-segment-bytes` (128 MiB), `-wal-retain` (0 = keep everything; a byte budget
for the retained set), `-wal-retain-segments` (`MinSegments`, 4), `-wal-archive` (empty),
plus §6's two thresholds. `wal.Open(path)` keeps its exact signature with these defaults;
`wal.OpenWith(path, Options)` is where they are set, which leaves
`INTEGRATION.md:196`, the godoc `Example` and every existing test untouched.

**The knob is bytes, and it converts directly to restart time.** From slice 1's measured
88.4 MiB read and CRC-verified in 56 ms, a retained set costs roughly **0.65 s per GiB**
to read on that hardware, plus O(book) for the snapshot and O(tail) for what is left to
apply. An operator who wants a one-second restart budget picks about 1 GiB. That sentence
is the deliverable of this whole slice, and §9.1 is where it gets measured rather than
derived.

*(Measured, it is 0.74 s/GiB warm and 2.07 s/GiB on a cold first pass, and the cold
one is the case worth budgeting for — so the published figure is 2 s/GiB and the
one-second budget is about 500 MiB. §12.7.)*

*(And the budget is a budget, not a bound. `MinSegments` is checked after it and wins,
so the retained set never falls below `(MinSegments + 1) x MaxSegmentBytes` — 640 MiB at
the shipped 4 and 128 MiB, whatever `-wal-retain` says. §11 saw this coming and §12 did
not come back to it, so the sizing sentence above was published as though the floor were
not there. It is: 500 MiB of budget against the default segment size buys 640 MiB and
~1.3 s. Pick the segment size and the segment floor together with the byte budget.
§12.11.)*

## 6. Disk space

### 6.1 What happens today, which is nothing

Worth stating precisely because it is worse than "undefined". `Writer.append` writes into
a `bufio.Writer`, so ENOSPC almost never surfaces at the append — it surfaces at the
`Flush` inside `Sync`, which runs on `cmd/obgw`'s 20 ms `syncLoop`
(`cmd/obgw/server.go:541-559`), whose entire error handling is:

```go
if err := b.wal.Sync(); err != nil {
    log.Printf("obgw: %s wal sync: %v", b.symbol, err)
}
```

It logs and continues. So a full disk gives a venue that keeps accepting orders, keeps
acknowledging them, keeps matching them, and stops journalling — fifty log lines a second
and no other signal. `/readyz` stays ready. The write-ahead property, which is the reason
this package exists, is silently gone. **Every acknowledgement after the first failed
sync is a lie**, and the venue is the only party that could know.

### 6.2 Thresholds, and what the venue does at each

Free space is sampled with `statfs` on the set's directory **once per checkpoint tick**,
which is cheap and is not on the matching goroutine.

*(This said "at every rotation, and once per checkpoint tick". Rotation does not sample
it and, on reflection, should not: `rotateLocked` runs on the matching goroutine under
the writer's lock, and it has no policy to apply to the answer — every threshold in the
table below belongs to `cmd/obgw`, not to `pkg/wal`. The consequence of sampling on one
cadence rather than two is that a venue with small segments and a fast fill rate reaches
the stop-water mark up to one checkpoint interval later than a per-rotation sample would
have caught it, which is an argument for a shorter `-checkpoint`, not for a `statfs`
inside the append path. §12.9.)*

| Level | Default | What the venue does |
|---|---|---|
| **Healthy** | — | Nothing. `orderbook_wal_bytes` and `orderbook_wal_disk_free_bytes` are exported |
| **Low water** | `-wal-min-free 2GiB` | Log a warning naming the set and the free bytes; run retention immediately instead of waiting for the next checkpoint |
| **Stop water** | `-wal-min-free-stop 256MiB` | Put every book into **cancel-only**. Log it. `orderbook_phase` reads 2 |
| **Failure** | ENOSPC on a write, flush, fsync or rotation | Halt the book. Fail readiness. Refuse every mutating command |

**Cancel-only rather than halt at the stop-water mark**, and the reason is the client's,
not the venue's: a venue that stops accepting *everything* leaves participants holding
positions they cannot withdraw, on a venue whose disk is about to fill. Cancel-only lets
them get flat, and it removes the largest source of log growth — new orders — while
letting the resting book drain. It does not stop the log growing, because a cancel is a
record too, which is why the failure level below it exists and is not optional.

**Cancel-only reuses an existing, defined, client-visible state.** `orderbook_phase`
already reports 2 for it, clients already receive `ReasonHalted` (wire 10) for a refused
new order, and `RUNBOOKS.md` already documents what it means. Inventing a
`ReasonDiskFull` reason byte would be a wire change this slice does not need, and the
"why" belongs where an operator reads it — the metric, the log line and the runbook — not
in a byte a trading client is expected to branch on.

*Rule 11 — a `Sync` failure halts the book and fails readiness. It is never logged and
retried.*
*Reason:* two reasons, and the second is the sharper one. A log that cannot be flushed
means every command since the last successful sync is acknowledged and not durable, so
continuing to serve is continuing to lie. And a partially completed flush leaves
`bufio.Writer`'s buffer in an indeterminate relationship to the file: appending behind it
writes a record into the middle of a torn one. So the writer enters a **sticky failed
state** — every subsequent append and sync returns the same error, the engine stays
halted rather than flapping open and closed as space transiently appears, and clearing it
requires a restart, which is where the operator gets to decide whether the disk is
actually fixed.

*Rule 12 — the stop-water mark must leave room for the venue to stop cleanly.*
*Reason:* shutdown flushes a buffer, fsyncs a segment, writes a snapshot and fsyncs a
directory. 256 MiB is far more than any of that needs and is chosen to be obviously
sufficient rather than tuned; the point of the rule is that the number is not zero and is
not the same as the failure threshold.

### 6.3 What the client sees

- **Low water:** nothing. It is an operator condition.
- **Stop water:** new orders, replaces and conditional orders are rejected with
  `ReasonHalted`. Cancels and reduces are accepted. Existing resting orders are
  untouched. Market data is unaffected.
- **Failure:** every mutating command is refused, `/readyz` returns 503, and the
  orchestrator takes the node out of rotation. A client's last acknowledged order is the
  last one that was durable, because the halt happens on the failed sync and not after
  it.

The honest limit of that last sentence, stated because it is the kind of thing that gets
overclaimed: with the default 20 ms group commit, commands acknowledged inside the
window that ended in the failed sync were acknowledged before they were durable, and
their fate is whatever the disk did. That window is the existing, documented durability
window (`-sync-every-command` closes it), and a full disk does not widen it. It is not
new here, but it is the moment it matters.

## 7. Backward compatibility

Three artifacts must keep working, and the tests that encode them must pass **unchanged**.

**An existing single-file log written by the current build** (`OBWAL\x01` at the stem).
It is a segment with implicit base 1, so §2.3's arithmetic is character-for-character
today's. It recovers to the same book, `ReadAll` returns the same slice, and `Open`
appends to it as it always did. `integrity_test.go` and `boundary_test.go` pass as
written.

**A v1 headerless log.** Same, minus checksums: implicit base 1, every record parsed
(slice 1's §3.2 rule that a v1 log's only integrity signal is whether it decodes is
untouched), the invariant check total rather than sampled.
`TestLegacyLogStillRecovers` — including its append-keeps-v1-framing half — passes as
written.

**The migration**, which is the one new behaviour. On `Open`, if the stem is a regular
file holding records, it is renamed to `<stem>.0000000000000001` and a marker is written
at the stem. Both are logged.

*Rule 13 — only `Open` migrates. `Recover`, `ReadAll` and every other reader treat a
legacy stem as a segment and write nothing.*
*Reason:* `cmd/obgw` recovers before it opens, so a reader that migrated would mutate the
directory during the one operation an operator runs when they are least sure what is on
disk. A reader that writes is a reader that can turn a diagnosis into damage. It also
keeps `Recover` usable on a copy of a venue's data directory mounted read-only, which is
how anyone sane investigates a corrupt log.

The migration is crash-safe in the same shape as everything else here: crash before the
rename leaves the legacy stem, and the next `Open` retries; crash between the rename and
the marker leaves a valid single-segment set with no marker, and the next `Open` writes
it. The marker is advisory (it exists to make a downgrade loud), so its absence is never
an error.

**A framing change is legal at a segment boundary and only there.** A v1 segment stays v1
for as long as it is the active segment; the segment rotation creates is `\x02` and
checksummed. This is the `Writer` doc comment's existing sentence — *"Rotate to get
checksums on an old file"* — becoming true for the first time.

**Error messages keep their shape.** `bounded_recovery_test.go` matches on the substring
`"record 10 "`. The new message is
`wal: corrupt record: BTC-USD.wal.0000000000610422 record 37 (sequence 610458) checksum
1fe167eb, want d1517f35` — segment name, in-segment ordinal, and the sequence the two
imply. It contains `record 37 `, so those assertions hold unweakened, and it answers the
question `RUNBOOKS.md` §"A corrupt log record" actually asks: an operator told to recover
"the records before the corrupt one" needs a sequence, not a per-file ordinal that is
ambiguous across 350 files. Slice 1's Rule 5 is preserved and generalised, not dropped.

**Three tests keep their meaning and change their arithmetic.**
`TestOnlyTheFirstRecordDisagreeingStillFallsBack` (`:519`),
`TestASequenceThatIsNotItsOrdinalAfterTheBoundaryFallsBack` (`:552`) and
`TestTheLastCoveredRecordIsStillChecked` (`:250`) each encode `Seq == ordinal`. Each is
still exactly right re-expressed as `Seq == base + ordinal − 1`, and each is testing
something this slice makes *more* load-bearing rather than less: the first is the
two-writers-interleaving shape, the second is drift past the boundary, the third is the
half of Rule 2 that a review found unasserted. Rewriting the arithmetic is legitimate.
Deleting any of the three is not, and neither is letting one of them pass because the
declared base happens to make the disagreement disappear — which it must not, since the
base is declared by the header and the disagreement is with the records.

**What legitimately breaks, and must be fixed rather than deleted:** the byte-surgery
test helpers (`bounded_recovery_test.go:75, 92, 101, 288, 441, 633`,
`integrity_test.go`'s seven sites, `wal_test.go:195`) each need a "which segment"
argument; `cmd/obgw/drills_test.go:71` and `recovery_test.go:364` inject damage through
`os.ReadFile(cfg.WALPath)` and `os.OpenFile(cfg.WALPath, O_APPEND)` and must be re-aimed
at a named segment — they are the two drills `RUNBOOKS.md` claims to enforce, so
re-aiming them is required work; and `restart_cost_test.go:157, 193` stat `walPath` for
the published `log-MiB` figure and must sum the set, or `BENCHMARKS.md` becomes quietly
false.

## 8. What this deliberately does not do

- **It does not add a segment digest or any form of verification that skips reading a
  segment.** §4.5 argues why, and the short version is that it is the trade
  `BOUNDED-RECOVERY.md` §5.3 already rejected, made coarser. Every retained byte is still
  read and every checksum still checked.
- **It does not build `walcat`, `walverify`, `walinspect`, `walreplay` or `walarchive`.**
  They are the next slice, and archived-segment verification is the first thing that
  slice has to answer.
- **It does not change the snapshot format**, and it does not keep a second snapshot so
  that one may be thrown away. §5.3 says why that is the natural follow-up and why it is
  a different change: it touches `WriteSnapshot`'s atomic-replace argument, which is the
  one piece of durability machinery in this package nobody has had to reason about twice.
- **It does not sync the WAL before a checkpoint.** `BOUNDED-RECOVERY.md` §4.1's window
  is unchanged, `SnapshotAhead` still fires after an ordinary crash, and retention's
  predicate is deliberately written not to depend on that window being closed: term (b)
  compares sequences, and a segment's own durability comes from term (a) rather than
  being inferred from the snapshot.
- **It does not make concurrent writers safe.** There is still no `flock`, lock file or
  `O_EXCL` on the active segment. What changes is narrow and worth stating: two writers
  rotating cannot both create the same segment, because §3.2 step 4 links into place and
  the loser gets `EEXIST`, fails its append and halts its engine. Before rotation they
  still interleave under `O_APPEND` and produce duplicate sequences, which Rule 2 catches
  only if the damage lands at a parsed ordinal. Narrowed, not closed, and the honest
  summary is that segmentation makes the consequences of the unfixed bug *less*
  recoverable, since the interleaved records now sit in a file that declares a base they
  do not respect.
- **It does not fix `examples/replication`'s `Promote`.** `Promote` writes a snapshot
  with `WALSeq = 0` and opens `walPath` on the assumption that a fresh log is a path that
  does not exist. Pointed at an existing set, `Open` resumes from the newest segment's
  sequence while the base snapshot claims 0, and recovery replays the old primary's
  entire log on top of the promoted book. That is a live defect today for one file; a set
  makes it easier to *see* (the marker answers "does a venue own this path?", which
  `os.Stat` on a directory could not) and no easier to hit. §4.4's floor check catches
  neither half of it. It needs an explicit "this path must not name an existing set"
  refusal in `Promote`, and that belongs with the replication work.
- **It does not give `examples/multisymbol` a recovery path.** `recoverSymbol` is still
  used only by tests, so that venue starts empty regardless of what is on disk. Worth
  knowing before retention deletes anything there, and not fixed here.
- **It does not add a time-based retention policy.** Retention is coverage, count and
  bytes. "Keep 30 days" is a compliance question whose answer is archival, not deletion.
- **It does not compress, encrypt or checksum archived segments beyond what they already
  carry.** A segment in the archive is byte-identical to the segment that was in the set.
- **It does not add a wire message or reject reason.** §6.2 reuses cancel-only and
  `ReasonHalted` on purpose.
- **It does not bound the snapshot.** A venue with a large book still pays O(book) on
  every restart and every checkpoint, and the trade-bust registry still grows without
  bound inside it ([`TRADE-BUST.md`](TRADE-BUST.md) §7). Retention bounds the log term
  and nothing else, and `PRODUCTION-READINESS.md` must say which term is bounded rather
  than saying "restart is bounded".

## 9. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | Segment layout, naming, header, marker | A fresh venue writes a marker and `<stem>.0000000000000001`; `TestSegmentNameAndHeaderMustAgree` refuses a renamed segment naming both numbers |
| 2 | **Slice 1's skip survives rotation** | `TestADeclaredSegmentDoesNotFallBack`: a set whose second segment declares base 1001, recovered against a snapshot inside it, produces the byte-identical book digest of the equivalent single file **and reports `FellBack == false`** |
| 3 | The old fallback still fires for the right reason | `TestAFileThatDeclaresBaseOneAndCarriesSequence1001FallsBack` (today's `TestSegmentStartingAtANonOneSequenceFallsBack`, re-aimed) still asserts `FellBack == true`, unchanged in its assertions |
| 4 | Rotation never splits a record | `TestARecordNeverStraddlesASegment`: 200,000 records at `MaxSegmentBytes = 1 MiB`; every segment's final record is complete, and `ReadAll` over the set equals `ReadAll` over the same stream written to one file |
| 5 | The crash matrix | `TestCrashDuringRotation` with an injectable failure at each of §3.3's six points; every one recovers to the same book, and the two that must be unreachable (segment without a header) are asserted unreachable |
| 6 | Recovery across a set equals one file | `TestRotatedSetRecoversTheSameBookAsOneFile`, for a set of 1, 2, 7 and 40 segments, at every snapshot boundary in `TestSkippedRecoveryMatchesFullParseAtEveryBoundary`'s style |
| 7 | Startup validation | Five refusals, one test each: gap, overlap, missing middle, name/header disagreement, snapshot below the floor. Each error names the segments and sequences involved |
| 8 | An empty set is not an error | Three shapes of §4.4 recover to the snapshot alone; a stem that is a directory is `ErrNotALog`, where today it is a fresh engine and a nil error |
| 9 | `Open` does not append behind a torn tail | `TestATornTailSealsItsSegment`: the `BOUNDED-RECOVERY.md` §6.1 reproduction — write 5, truncate 17 bytes, `Open`, append, `ReadAll` — now succeeds on the second restart, and the fragment is still on disk |
| 10 | Retention cannot outrun the snapshot | `TestRetentionNeverDeletesAheadOfTheSnapshot`: 50 segments, checkpoints and retention interleaved with live appends under `-race`; after every cycle, `snap.WALSeq + 1 ≥ base(S₁)` |
| 11 | Retention refuses a snapshot it cannot verify | `TestRetentionIgnoresACorruptSnapshot`: CRC-damage the snapshot, run a retention cycle, assert nothing was deleted |
| 12 | The floor check is the tripwire | `TestSnapshotBelowTheRetentionFloorRefuses`: delete a segment by hand, restart, get `ErrLogGap` naming both sequences — never a book |
| 13 | Retention is crash-safe | `TestCrashDuringRetention`: fail after *k* unlinks for every *k*; every resulting set is valid and recovers to the same book |
| 14 | Disk-full is defined | `TestDiskFullHaltsRatherThanAcknowledging`: an injected ENOSPC at flush halts the book, fails `/readyz`, and no command acknowledged after the failure exists |
| 15 | The stop-water mark works | `TestStopWaterGoesCancelOnly`: below the threshold, new orders are rejected with `ReasonHalted` and cancels still succeed |
| 16 | Backward compatibility | `pkg/wal`, `cmd/obgw` and `examples/...` green, full run and `-race`, with `integrity_test.go`, `boundary_test.go` and `example_test.go` unmodified |
| 17 | Replication survives rotation and retention | The replication drills with `MaxSegmentBytes = 1 MiB` and retention on: a follower reconnecting across a rotation catches up; one reconnecting below the floor is bootstrapped, not gapped |
| 18 | **Restart cost is O(retained log)** | §9.1 |
| 19 | Prose corrected | `wal.go:9-18` and `:940-944`, `cmd/obgw/server.go`'s checkpoint-loop comment (which now says that this loop is exactly where the read gets bounded, since retention runs from it), `INTEGRATION.md`'s recovery comment, `RUNBOOKS.md` (three sections: the corrupt-snapshot procedure, the corrupt-record ordinal, and the "never edit" rule, which must now also say never rename, move or delete a segment), `PROTOCOL.md` §"Durability", `SOAK.md` §4, `PRODUCTION-READINESS.md`, `BENCHMARKS.md`, and `BOUNDED-RECOVERY.md` §6's records-per-day arithmetic — none of them still says the file never shrinks, and none says "bounded restart" without naming retention |

### 9.1 The measurement that matters

Every other number in this slice is secondary to one claim, and it has to be measured in
the one shape that cannot be faked by a fixture whose book grows with its log — the
mistake `BOUNDED-RECOVERY.md` §7.1 documents.

**`BenchmarkRestartWithRetention`.** Built on a churn fixture, which writes
submit/cancel pairs so the resting book stays near empty while the log grows, so the
measurement isolates the O(log) term from the O(book) term. *(Written here and in
`BENCHMARKS.md` as `buildCoveredChurnLog`, which is slice 1's fixture in
`restart_cost_test.go` — it does not rotate and takes no retention parameters. The one
this benchmark uses is `buildRetainedChurnLog` in `retention_bench_test.go`. Both names
are corrected, because a reader reproducing the table looks the fixture up.)*

- Retention fixed at 8 segments of 1 MiB.
- Total history written: 10 MiB, 100 MiB, 1 GiB — a 100× range.
- Snapshot current in every row; 1,000 records to apply in every row.
- Report `Recover` wall time, `B/op`, and bytes actually read from disk.

**The assertion:** recovery time at 1 GiB of total history must be within **1.25×** of
recovery time at 10 MiB. The same benchmark with retention disabled is the control and
must show the ~100× it shows today. The failure message says that a regression here means
retention stopped bounding the read, which is the only thing this slice is for.

Publish, into `BENCHMARKS.md`, with the fixture named:

- `Recover` time and allocation at 10 MiB / 100 MiB / 1 GiB of total history, retention
  on and off.
- Bytes read per restart against retained bytes, which should be a straight line through
  the origin with the slope `BENCHMARKS.md` already records for read-and-CRC.
- Rotation's cost in the append path: p50/p99/p99.9 of `AppendSubmit` with rotation on
  against off, at 128 MiB and at 1 MiB segments. A rotation is two fsyncs, a link, an
  unlink and a directory fsync, and it will be visible in the tail. **Publish the number
  rather than claiming it is negligible** — at 128 MiB segments it happens every four
  minutes, and a 10 ms hiccup every four minutes is a different product decision from a
  10 ms hiccup every four seconds.

And into `PRODUCTION-READINESS.md`, the four figures M3 asks for per book size, which
retention finally makes answerable: maximum restart duration, maximum WAL tail, disk
space required, and the recovery point objective — which, under retention without
archival, is the newest snapshot, and that sentence goes in the table.

## 10. Sabotage runs required before this counts as done

Each is a deliberate break, run to confirm the test that should catch it does. This
repository has had a test pass against its own sabotage twice — once in
[`TRADE-BUST.md`](TRADE-BUST.md) §7, once in `BOUNDED-RECOVERY.md` §9.2 — and that is the
only reason §9's claims are worth anything.

1. **Derive the segment base from record 1's `Seq` instead of the header.** Deliverable 1
   must fail (`TestSegmentNameAndHeaderMustAgree` has nothing left to compare), and
   deliverable 2 must still pass — which is the point: the sabotage that *looks* harmless
   is caught by the structural test, not by the recovery test.
2. **Revert the per-segment arithmetic to slice 1's `ordinal`.** Deliverable 2 must fail
   on `FellBack == false`. If it does not, the trap in §0 has fired and the test is
   wrong, not the code. This is the single most important run in this list.
3. **Move the rotation decision between the frame-header write and the payload write.**
   Deliverable 4 must fail.
4. **Drop step 2's fsync of the sealed segment.** Deliverable 5's crash-after-step-1 case
   must fail. (If it passes, the fault injection is not injecting a crash, it is injecting
   a clean shutdown.)
5. **Replace steps 3–5 with `os.Create` + write header.** Deliverable 5's
   "segment without a header is unreachable" assertion must fail.
6. **Delete the contiguity check.** Deliverable 7's gap, overlap and missing-middle tests
   must all fail, and deliverable 6 must recover a *wrong* book — which is the outcome
   worth seeing once.
7. **Gate retention on the snapshot existing rather than verifying.** Deliverable 11 must
   fail.
8. **Change term (b) from `WALSeq ≥ last(S)` to `WALSeq ≥ base(S)`.** Deliverable 10 must
   fail. This is the subtle one: it is wrong only for the records above the snapshot's
   sequence inside the deleted segment, so a test that only checks the *count* of
   segments will pass.
9. **Delete segments newest-first.** Deliverable 13 must fail at some *k*.
10. **Let retention delete the active segment.** Deliverable 10 must fail, and the
    failure should be `Open` resuming from a sequence the snapshot already claims.
11. **Restore the sync loop's log-and-continue.** Deliverable 14 must fail.
12. **Make the writer retry after a failed sync instead of latching.** Deliverable 14
    must fail on the "no command acknowledged after the failure exists" leg.
13. **Have the primary ship whatever it still holds instead of bootstrapping a follower
    below the floor.** Deliverable 17 must fail, with the follower reporting `gap in the
    feed`.
14. **Migrate a legacy stem from a reader instead of from `Open`.** Deliverable 16 must
    fail — specifically, a `Recover` against a read-only directory must fail, and if it
    does not, the reader is writing.

## 11. How this can fail, stated in advance

So §9's write-up is not graded on a curve.

- **Rotation's fsyncs could be visible in the latency tail.** Two fsyncs, a link, an
  unlink and a directory fsync land on the appending goroutine, which is the matching
  goroutine when the `Writer` is a `Runner`'s command log. At 128 MiB segments that is
  every four minutes and probably invisible next to a checkpoint's pause; at small
  segments it will not be. §9.1 measures it. If it is bad, the answer is to pre-create
  the next segment ahead of need — which is a change to the crash matrix and would need
  its own section, not a patch.
- **The `MinSegments` floor could make retention useless in exactly the case it matters.**
  Four 128 MiB segments is 512 MiB of floor. A venue that wants a 256 MiB budget cannot
  have one without shrinking the segment size, which raises the rotation rate, which is
  the previous bullet. The two knobs interact and the defaults may prove to be the wrong
  pair.
- **The floor check could fire in production for a benign reason nobody predicted.**
  §4.4's refusal is deliberately absolute, and an absolute refusal that turns out to have
  a benign cause is an outage this design created. If that happens, the response is to
  find the cause and name it — not to downgrade the check to a warning, which would put
  us back to a wrong book with a log line.
- **`ReadAll` returning a suffix could break a consumer nobody surveyed.** The contract
  changes from "the whole history" to "what is retained" the first time a segment is
  deleted, and that is a semantic change hiding behind an unchanged signature. The survey
  found `primary.go`, the runbook and a dozen tests; it did not find every embedder, and
  embedders do not have tests in this repository.
- **The downgrade story could be worse than §2.5 claims.** The claim is that an old build
  refuses loudly on a marker. It rests on `MaxRecordBytes` catching a 1.3 GB length
  prefix, which it does — but only for a build that has that bound, which is every build
  since it was added and not necessarily every build in somebody's deployment. A truly
  old build is a truly bad outcome and this design does not prevent it.
- **Retention plus a corrupt snapshot could be a worse combination than §5.3 admits.**
  The RPO becomes "the newest snapshot" for any venue without archival, and archival is
  off by default. If operators run retention without archival — which is exactly what
  happens when the disk-space problem is urgent and the backup problem is not — this slice
  will have traded a disk that fills for a book that cannot be reconstructed. The startup
  refusal makes it loud. It does not make it recoverable.

---

## 12. What building it changed — written after the code

### 12.1 The marker arrives at the first rotation, not at the first `Open`

§2.5 Rule 3 says "a set this build creates always has a marker at the stem", and §9's
deliverable 1 says "a fresh venue writes a marker and `<stem>.0000000000000001`". That
cannot be reconciled with §9's deliverable 16 and §7's first paragraph, and the
conflict is not subtle: `integrity_test.go:59` reads the bytes at the WAL path and
asserts they begin with `OBWAL\x01`. A fresh `Open` that writes an 18-byte `\x02`
marker there fails that assertion, and deliverable 16 requires `integrity_test.go` to
pass **unmodified**.

So the rule became: **a path with nothing at it gets a single `OBWAL\x01` file, exactly
as before; the set materialises around it at the first rotation.** The stem is renamed
to `<stem>.0000000000000001` and the marker is written in its place, inside
`rotateLocked`, under the same lock.

This is a better rule than the one it replaces, and the argument is §2.5's own. The
marker exists so a downgrade is loud rather than silent — an old build pointed at a
rotated set would otherwise find no file, conclude there is no log, and start an empty
venue beside four hundred million records. A venue that has *not* rotated has no such
problem: the stem is the whole log, and an old build reads it correctly, which is the
right outcome and not one worth converting into a refusal. The marker now appears at
exactly the moment it starts being needed, and never before.

What it costs: the `\x02` header is not a proof of ownership for a path a venue has
opened but not yet filled, so §2.5's aside about `Promote` answering "does this path
belong to a venue?" holds only after a first rotation. `Promote` is out of scope here
either way (§8).

The migration is crash-safe at the first point: a crash before the rename leaves the
legacy stem and the next rotation retries.
`TestTheStemBecomesSegmentOneOnTheFirstRotation` pins that.

**The second point is NOT safe, and this paragraph used to say it was.** It said "a
crash between the rename and the marker leaves a valid single-segment set with no
marker, and the marker is advisory, so nothing refuses" — and every clause of that is
true and the conclusion is wrong. *Nothing refusing* is the failure. A set with numbered
segments and no file at the stem is the one shape §2.5 exists to prevent, the only one
an older build cannot detect, and "the marker is advisory" describes this build's
readers rather than the build the marker is there for. The same state is produced
without any crash at all, by a `writeMarker` that fails with ENOSPC or EROFS — the disk
condition this milestone exists for. §12.8 has the rest, and §12.12 has the repair.

### 12.2 §5.4's "open every segment first" became a restartable read

§5.4 requires tolerating a segment vanishing mid-read and prescribes the mechanism:
"open every selected segment before reading any of them". What is built instead is a
read that RESTARTS: `walkSegment` reports `errSegmentVanished` on `ENOENT`, and every
entry point that can meet it — `ReadAll`, `Recover`, `Open`, `ReadAfter` — re-runs the
enumeration, bounded at eight attempts so a genuine filesystem problem surfaces as an
error rather than as a spin.

The reason is file descriptors. A venue running without retention accumulates
segments — 350 a day at the 128 MiB default and 2,500 msg/s — and holding one open fd
per segment for the duration of a recovery would exhaust the default `ulimit -n` of
256 on macOS within a day of uptime, in the recovery path, which is the one place a
venue can least afford a new failure mode. It would also not close the window it was
aimed at: enumeration itself races retention, and an fd opened after the listing
cannot help with a file deleted before the open.

§5.4's own sentence is the one that matters and it is satisfied: *"Term (e) of §5.1 is
satisfied by making the reader restartable rather than by making retention wait."*
`TestRetentionRacingAReaderIsSurvivable` runs a catch-up reader against twelve live
retention cycles under `-race`; it found the bug this paragraph describes, on the
first run, as a spurious `ErrLogGap`.

### 12.3 §6.2's argument for reusing `ReasonHalted` rested on something false

§6.2 declines to invent a `ReasonDiskFull` on the grounds that cancel-only "reuses an
existing, defined, client-visible state — `orderbook_phase` already reports 2 for it,
clients already receive `ReasonHalted` (wire 10) for a refused new order".

The first half was true. The second was not. `orderentry.ReasonFor` had no case for
`types.ErrTradingHalted` or `types.ErrNewOrdersHalted`, so both fell through to
`ReasonOther` (wire 1) — the code a client is told to treat as "something else went
wrong". `ReasonHalted` was defined, documented, frozen in the wire numbering, and
handled by `cmd/obsoak/main.go:257`, and **nothing in the repository ever sent it**.
Found by writing `TestStopWaterGoesCancelOnly` and watching it assert 10 and receive 1.

Two cases were added to `ReasonFor`. That is a behaviour change outside this slice's
stated scope and it is recorded here rather than buried: a halted or cancel-only venue
now refuses new orders with `ReasonHalted` where it used to say `ReasonOther`. No test
asserted the old value, no wire constant changed, and the alternative was to leave
§6.2's justification resting on a claim its own test disproves.

**And they were first written *over* the `matching.ErrShuttingDown` case rather than
beside it**, which is the same defect this section congratulates itself for finding,
committed in the act of fixing it — a defined, documented, frozen reason code that
nothing sends. `ReasonShuttingDown` (wire 15) is reached live from `cmd/obgw`'s replace
and reduce paths, which feed the done channel's error straight into `ReasonFor`
(`server.go:1457`, `:1542`), and `cmd/obsoak` branches on it; for the time that case
was missing, a client that replaced or reduced an order against a venue draining for a
restart received `ReasonOther`. The full suite and `-race` were green throughout,
because **nothing in the repository tested `ReasonFor` at all** —
`cmd/obgw/hardening_test.go:208` asserts only that the wire and `orderentry` constants
carry equal values, which they still did.

The case is restored, and the fix is not the restoration. `ReasonFor` is now pinned by
a table with one row per sentinel (`pkg/orderentry/reason_test.go`), because the failure
mode here is a **deletion**, and a test per case only covers the cases somebody
remembered to write a test for.

### 12.4 Sabotage 4 passes, and that is a gap rather than a pass

§10's fourth run — drop step 2's fsync of the sealed segment, and require deliverable
5's crash-after-step-1 case to fail — **does not fail**. §10 predicted this in the same
breath: *"If it passes, the fault injection is not injecting a crash, it is injecting a
clean shutdown."* That is exactly what happened.

`TestCrashDuringRotation` injects a failure at each numbered step and then abandons the
writer, which models a process that dies. It does not model a machine that loses power,
and only the second one can tell an fsynced segment from an unfsynced one: the bytes
are in the page cache either way and the test's next `Open` reads them back through the
same kernel. Testing that fsync honestly needs a crash below the filesystem — a VM
snapshot, `dm-flakey`, or a fault-injecting FUSE layer — and none of those is in this
repository.

So: the crash matrix's six states are covered against *process* death, the
"segment without a header is unreachable" assertion is real (sabotage 5 fails it), and
the durability of the sealed segment's bytes across a *power* loss is asserted by
construction and not by test. Stated rather than glossed, because the alternative is a
row in a table that looks tested and is not.

### 12.5 A rotation costs 12.4 ms, and §11's first bullet was right

§9.1 required publishing rotation's cost rather than calling it negligible, and §11
listed the latency tail as the first way this could fail. Measured on
`BenchmarkRotationAppendTail`, attributing cost to the appends that actually rotated
rather than inferring it from the difference between two runs (Apple M4, APFS,
`-benchtime 200000x`):

| segments | ns/op | p50 | p99 | p99.9 | rotations | mean rotation | worst |
|---|---:|---:|---:|---:|---:|---:|---:|
| off | 2,526 | 1,625 ns | 9,959 ns | 52 µs | 0 | — | — |
| 128 MiB | 2,423 | 1,625 ns | 9,958 ns | 45 µs | 0 | — | — |
| 1 MiB | 6,001 | 1,625 ns | 9,959 ns | 76 µs | 58 | **12.4 ms** | **21.2 ms** |

Two fsyncs, a `link`, an `unlink` and a directory fsync, on a filesystem where fsync
is not cheap. At the 128 MiB default that is 12 ms every four minutes at 2,500 msg/s —
rarer and smaller than a checkpoint's pause, and defensible. At 1 MiB it is every two
seconds and the mean cost per append triples, which makes small segments a test
fixture rather than a configuration, and that sentence is now in
[`BENCHMARKS.md`](BENCHMARKS.md). §11's remedy — pre-create the next segment ahead of
need — remains the right one and remains a change to the crash matrix, so it is not in
this slice.

### 12.6 Smaller things

- **`MinSegments` needed a way to say "none".** Zero has to mean the default, because
  the zero `Options` value is the shipped policy and a floor of nothing is not a policy
  anybody should acquire by leaving a field unset. Negative means no floor. Found by a
  test that passed `MinSegments: 0`, expected no floor, and got four.
- **A torn tail in an EMPTY segment needed its own answer.** §3.4's Rule 8 seals a
  torn segment and starts the next at `last + 1` — but a segment holding a header and
  nothing but a torn first record has `last = base - 1`, so `last + 1` is the name it
  already occupies. The fragment is moved aside to `<name>.torn`, which no enumerator
  matches, and the segment is written cleanly at the base it declared. Nothing is
  truncated and no complete record is lost, because there was not one.
  `TestTornTailInAnEmptySegmentIsSealed`.
- **Sabotage 2 is caught by four tests, not one.** §0 and §2.3 claim
  `TestADeclaredSegmentDoesNotFallBack` would be "the only thing in the repository that
  fails". Reverting the per-segment arithmetic also fails
  `TestRotatedSetRecoversTheSameBookAsOneFile`,
  `TestASegmentWhoseRecordsDisagreeWithItsDeclaredBaseFallsBack` and
  `TestRetentionNeverDeletesAheadOfTheSnapshot`, all of which assert
  `FellBack == false` on a set this package wrote. The claim was about a repository
  that did not yet have those tests; the trap is real and it is now guarded four times.
- **Sabotage 6 does not make deliverable 6 recover a wrong book**, as §10 predicted it
  would. Deleting the contiguity check fails the gap, overlap and truncated-middle
  refusals — all three, as specified — but
  `TestRotatedSetRecoversTheSameBookAsOneFile` has no hole in its fixture, so it has
  nothing to recover wrongly. The wrong book is visible in
  `TestAMissingMiddleSegmentRefuses`, which reads a set with a hole and gets a nil
  error and a short slice.
- **`ReadAfter` is exported.** `BOUNDED-RECOVERY.md` §6 declined to export a
  "records after sequence N" reader for `examples/replication`, on the grounds that
  exporting it means designing its contract for callers outside the package. Retention
  made it necessary rather than convenient: a caller that filters `ReadAll` itself
  cannot distinguish "there are no records past N" from "the records past N were
  deleted", and shipping the second as the first is how a follower gets a gap from the
  one source that is supposed to be authoritative. Its contract is the one thing the
  caller could not compute: `ErrBelowFloor`.

### 12.7 The knob's arithmetic is 2 s/GiB cold, not 0.65

§5.5 derives the operator-facing sentence — "an operator who wants a one-second
restart budget picks about 1 GiB" — from slice 1's 88.4 MiB read and CRC-verified in
56 ms, which is 0.65 s/GiB. Measured at a gigabyte, that number is right for a WARM
re-read (1,068 MiB in 767 ms, 0.74 s/GiB) and wrong for the case that matters: a
single first pass over a file just written costs 2.21 s, or 2.07 s/GiB. A restart
worth budgeting for is usually a restart after a reboot, so the published figure is
now **2 s/GiB cold**, and the one-second budget is about 500 MiB rather than 1 GiB.

The other half of that sentence was worth measuring rather than assuming, and it came
out the opposite way from the guess: **segment count does not affect the read.** The
same 1,068 MiB reads in 767 ms as 1,069 segments of 1 MiB and 799 ms as 9 segments of
128 MiB. A thousand extra `open`/`close` pairs are nothing next to a gigabyte of I/O.
So the retained SIZE comes from the restart budget and the SEGMENT size comes from
§12.5's append-latency table, and the two knobs do not interact through the read at
all — which is not what §11's second bullet assumed when it worried about them
interacting.

### 12.8 Two holes the tests found in the design, both about an absent stem

Neither is in §3.3's crash matrix, because both are reachable without a crash.

**A segment created while the stem does not exist.** `Open` materialises a numbered
segment in two places besides rotation: a set that has a marker and no segments, and
the torn-tail seal of §3.4 Rule 8. In the second, if the torn segment holds a header
and no complete record, the fragment is moved aside — and if that segment WAS the
stem, the stem is now gone, leaving numbered segments with nothing at the path the
operator passed to `-wal`. That is precisely the shape §2.5 exists to prevent.
`openFreshSegment` now writes the marker whenever the stem is absent.

**A stem reclaimed by retention.** The same Rule 8 leaves a pre-rotation stem holding
records 1..k as the *oldest member of a set*, so retention will eventually come to
delete it. Unlinking it produces the same absent stem. It is replaced by the marker
instead, with `writeMarker`'s temp-and-rename, so there is no window in which the path
does not exist. `TestRetentionLeavesAMarkerWhereTheStemWas`.

The asymmetry worth naming: every other downgrade shape is LOUD without help. An old
build that appends into a legacy stem alongside a numbered segment produces two files
claiming the same sequences, which this build refuses as an overlap on the next start.
Only "no file at all" is silent, because "no file" is a legal state meaning "no venue
has ever run here". So the marker earns its place at exactly the boundary between
those two, and the rule is not "always write a marker" but "never leave the stem
absent while segments exist".

**A third hole, found by review rather than by the tests, and it is the one the rule
above was already stating.** Both cases here are places that CREATE the absent stem and
were fixed where they created it. Nothing enforced the rule as a rule: given the state
by any other route, no reader and no writer repaired it. `OpenWith`'s ordinary path
decided "this is a legacy stem" from `newest.named`, which is false the moment the
migration's rename has happened, so neither `Open` nor any later rotation ever wrote the
missing marker — measured, still absent after 46 further rotations, with 300 records in
the set and an old build reading it as an empty venue. The migration window in §12.1 and
a failed `writeMarker` both land there. §12.12.

### 12.9 Free space is sampled once per checkpoint tick, not at every rotation

§6.2 said "at every rotation, and once per checkpoint tick", and `pkg/wal/diskfree.go`'s
own doc comment said the same. There is exactly one non-test call to `FreeBytes` in the
repository, in `cmd/obgw`'s checkpoint loop. Rotation does not sample it.

Correcting the code rather than the document was considered and rejected.
`rotateLocked` runs on the matching goroutine under the writer's lock, so a `statfs`
there is a filesystem call in the append path — and `pkg/wal` would have nothing to do
with the answer, because every threshold in §6.2's table belongs to `cmd/obgw`. Sampling
where the policy is is the right shape; the sentence was describing an intention.

What the single cadence costs is real and worth stating: a venue with small segments and
a fast fill rate crosses the stop-water mark up to one checkpoint interval later than a
per-rotation sample would have caught it. The answer is a shorter `-checkpoint`, not a
syscall under the writer's lock.

### 12.10 Rule 9's contiguity check was never implementable

Rule 9 said `Open` "validates the structure of every other segment (name, header, base,
contiguity)", and `lastSeq`'s doc comment said it checks "the structure it is about to
extend". Contiguity is not checked and cannot be by this design: `contiguityError` needs
the previous segment's **last record sequence**, which comes only from reading its
records, and reading only the newest segment is the whole point of the change §12.7
measures. `enumerateSet`'s `validateStructure` checks duplicate bases and nothing else.

Confirmed by running: on a 23-segment set with a middle segment deleted, `ReadAll` and
`Recover` both return `wal: log gap`, and `wal.Open` returns nil and resumes appending.
Same for an empty middle segment, a zero-length segment, an overlapping pair, and a byte
flipped inside a sealed segment.

The rule now says what `Open` does — the directory-answerable structure, plus every frame
and checksum of the newest segment — and says the consequence out loud, because it is the
part a caller can get wrong: **`wal.Open` is not an integrity check.** `cmd/obgw` is safe
because it calls `Recover` first on the same path. `examples/replication`'s primary opens
without recovering, which is right for an example that starts an empty book, and now says
so in its doc comment.

Unlike the five deviations §12 already recorded, this one was asserted as fact in a godoc
rather than argued for, which is why it survived to be found by review instead of by
writing it down.

### 12.11 `-wal-retain` is a budget with a floor under it, and the sizing numbers ignored it

§5.1's predicate has both terms — (b) the snapshot, (c) `MinSegments` — and §11 saw the
interaction coming: *"Four 128 MiB segments is 512 MiB of floor."* Then §12 never came
back to it and the operator-facing numbers were published as though it did not exist.

The order in the loop is what makes it decisive: the byte budget is tested first and
`break`s when it is satisfied, and the `MinSegments` floor is tested second and `break`s
when it is not. So the floor always wins. Measured with a one-byte budget against the
default floor: 93 segments become 5, which is `MinSegments` sealed plus the active one,
and `bytes <= RetainBytes` never fires at all. Scaled to the 128 MiB default that is
**640 MiB**, and the published advice — "an operator who wants a one-second cold restart
budget picks about 500 MiB of `-wal-retain`" — is unreachable at the defaults it was
written against. 500 MiB buys 640 MiB and about 1.3 s.

Three changes rather than one, because a floor nobody can see is the actual defect:

- The arithmetic is stated wherever the sizing advice is — the flag help, `Config`,
  `wal.Options`, `BENCHMARKS.md`, `PRODUCTION-READINESS.md` and §5.5 — as
  `(MinSegments + 1) x MaxSegmentBytes`, with the remedy, which is to bring the segment
  size down with the budget rather than to raise the budget.
- `RetentionResult.Skipped` already named the term that stopped each cycle, and
  `cmd/obgw` discarded it. It is now logged when it CHANGES, so an operator watching a
  disk fill under a configured retention learns which term is holding the set up instead
  of watching nothing happen.
- `TestTheByteBudgetIsFlooredByMinSegments` pins the arithmetic, so the number in the
  documents and the number in the code cannot drift apart again.

### 12.12 Three defects the review found in the code, and what they had in common

All three were silent, all three were in the parts of this slice that only run when
something has already gone wrong, and none of them was reachable by any test in the
repository.

**Archiving into the log's own directory destroyed the archive and reported success.**
With `ArchiveDir` set to the directory holding the set, a segment's archive target IS the
segment. `archiveSegment`'s idempotency check — a file at the target with a matching
size, which is what makes a cycle that crashed between the copy and the unlink safe to
repeat — found the live segment, agreed it was already archived, and returned nil.
Retention unlinked it immediately afterwards. Measured on a 24-segment set: 22 reported
in `res.Archived`, 22 gone, `ArchivedSegments` returning only the two still live, and
`cmd/obgw` logging "retention deleted 22 segment(s) ..., archived 22". A success line for
total loss. It is an easy mistake to make on purpose, because §5.3 makes archival the
only thing between retention and an RPO of "the newest snapshot", so every document here
tells an operator to set `-wal-archive` the moment they set `-wal-retain` — and the log
directory is the first path to hand. `wal.CheckArchiveDir` now refuses it, compared by
inode as well as by string so a symlink or a relative path does not get through, from
`Retain` before anything is deleted and again at startup in `cmd/obgw`.

**A set whose stem is absent was never repaired.** §12.8's third hole. `healMissingStem`
runs in `OpenWith` before any branch, writes the marker when there are numbered segments
and nothing at the stem, and touches a stem that exists under no circumstances — a
pre-rotation stem still holds records, and a repair that overwrote it would be the damage
it exists to prevent. It is the same kind of thing `removeStaleTemps` already does, for a
stronger reason.

**The active segment's file descriptor kept the temp's name.** `materialiseSegment`
builds a segment through `<base>.tmp`, links it into place and unlinks the temp — and
returned that fd. An `*os.File` remembers the name it was opened with, so from the first
rotation onward every write, flush, fsync and close error on the ACTIVE segment named a
file that does not exist. Observed on a real ENOSPC: `WAL SYNC FAILED — halting the book
...: write /Volumes/WALTINY/venue/v.wal.0000000000078054.tmp: no space left on device`,
with `ls` showing the segment present at 1,069,056 bytes and no `.tmp` anywhere. That is
worse than a wrong filename: §3.3 teaches that a stray `<base>.tmp` means a rotation that
crashed between materialising a segment and linking it into place, so the message argues
for the wrong diagnosis at the moment an operator is following the runbook. The segment
is now reopened on its final path after the link; durability belongs to the inode, which
the link preserved and the fsync had already covered.

### 12.13 Retention was skipped on a full disk, and the low-water line said otherwise

Two things in `cmd/obgw` that only misbehave in the state §6 is entirely about.

`checkpointLoop` ran retention only after a SUCCESSFUL `WriteSnapshot`, and `continue`d
past it otherwise. `WriteSnapshot` fails on a full disk. So the one automatic mechanism
that can free space was skipped exactly when it was the only one left. It is safe to run
regardless, and always was: `Retain` does not trust that write — it re-reads the snapshot
from disk and verifies it, so a failed checkpoint leaves the PREVIOUS snapshot in force
and retention deletes only what that one already covers.

`checkDiskSpace`'s switch is ordered by severity, so below the stop-water mark the
low-water case — the only branch that ran retention — is unreachable. Below the stop-water
mark the venue went cancel-only and then did nothing at all, every tick, forever.
Retention now runs in both branches.

And the low-water line ended "; running retention now" unconditionally, while `retain`
returns immediately when `-wal-retain` is unset, which is the default. Reproduced against
a real 20 MiB filesystem: three lines claiming to run retention, then the stop-water
line, then the halt, with no segment ever deleted. `RUNBOOKS.md` was right about this and
only the log line lied — but the log line is what an operator reads first, at 03:00. It
now says which of the two things is actually about to happen.

### 12.14 Cancel-only does not clear, and nothing said so

`checkDiskSpace` sets `diskStopped` once and nothing anywhere clears it: not free space
returning, not an admin endpoint, because there is not one. A book that dipped below
`-wal-min-free-stop` for a single checkpoint tick keeps refusing new orders with
`ReasonHalted` until the process restarts, even though no sync ever failed.

The behaviour is deliberate and stays — a venue oscillating in and out of cancel-only
around a threshold is worse for participants than one that stays out until a human has
looked. What was missing is that nobody said so. `RUNBOOKS.md` documented the one-way
latch for the SYNC FAILURE and described cancel-only separately as a lesser, earlier
state, so an operator who freed space before any sync failed would reasonably expect
trading to resume. It is now stated in the runbook, in the log line at the moment the
venue goes cancel-only, and on the field itself.

### 12.15 The floor check found a defect that predates this milestone

§5.1's floor check is the tripwire every retention bug trips, and the first thing it
caught was not a retention bug.

`matching.NewRunnerFor` builds a Runner over a recovered engine and had no way to be told
where in the log that engine stood, so `lastApplied` started at zero however the engine
came to exist. A checkpoint taken after a restart and before the first command therefore
stamped a snapshot holding the complete book with `WALSeq 0` — "this covers nothing,
replay from the beginning". The window is one checkpoint tick with no command in it,
which at the shipped 30-second cadence is any restart into a quiet market.

Before this milestone that is silent and sometimes wrong: the next recovery re-applies
the whole log on top of a snapshot that already contains it, and the only thing stopping
a doubled book is the duplicate-client-order-id ring — which is bounded (4096 in
`cmd/obgw`, zero by default in `pkg/matching`) and does not apply at all to an order with
no client order id, which the wire accepts. Fifty of those through `cmd/obgw` over TCP,
a restart, one quiet checkpoint and another restart give a book of a hundred. With
`-wal-retain` on it stops being silent and becomes fatal: the retained floor climbs past the sequences
the zeroed stamp claims not to cover, and the venue refuses every subsequent start with
`ErrLogGap` naming segments that have already been deleted, with archival off by default.

It is not this slice's defect and it is not fixed in this slice's commit. It is recorded
here because this slice's check is what made it visible, and because §5.1's floor check
should be read as having done its job rather than as having caused an outage. The fix —
`matching.RunnerConfig.LastApplied`, seeded in `cmd/obgw` from the recovery report — and
the runbook entry for a snapshot stamped `0` travel separately.

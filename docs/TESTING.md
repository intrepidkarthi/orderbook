# Testing — Break It On Purpose

Author: Karthikeyan NG · Last updated: 2026-08-11

Companion documents: every spec's §7. [`REPLICATION.md`](REPLICATION.md),
[`TRADE-BUST.md`](TRADE-BUST.md) and [`MULTI-SYMBOL.md`](MULTI-SYMBOL.md) each
record a test that went green for a reason it was not testing, and each of those is
a case study below.

---

## The rule

**A new test does not count until it has been run against code deliberately broken
in the way it claims to detect.**

Not "tests pass". Not "coverage went up". Run the test against a version of the
code with the specific defect it exists to catch, watch it fail, put the code back.
If it does not fail, the test is decoration and the property is unproven — however
green the suite is.

This is already the standing rule for the replication drills, whose file header
opens by naming the sabotage each one was verified against
(`examples/replication/main_test.go`). This document generalises it, because the
same mistake has now been made six times across the five tests below, in code that
was being written carefully, by people who knew about the rule.

## Why it keeps being necessary

A test can be green for three reasons: the code is right, the test is asleep, or
the test is measuring something adjacent to what it claims. The suite reports all
three identically. Nothing about writing the test carefully distinguishes them —
the only thing that does is making the code wrong and seeing what happens.

The failures below were not sloppy. Each was a test that read correctly, asserted a
real property, and would have been approved in review.

## The two shapes

**Green when broken.** The common one. The test's assertion is satisfied by
something other than the property — a side effect, a neighbouring field, a
different error path.

**Red when fine.** Rarer and worse for morale, because it trains people to
re-run until it passes. Usually a measurement whose noise was never compared to its
threshold.

## Case studies

Each names what the test claimed, why it passed (or failed) anyway, and what fixed
it. They are in the repository so the next person can read the fix next to the
mistake.

### 1. A digest test satisfied by a sequence counter

*[TRADE-BUST.md](TRADE-BUST.md) §7.* The claim: a bust registry left out of the
snapshot is caught by the state digest, so a follower that loses a bust cannot go
undetected.

The test busted a trade on one engine, not on another, and compared digests. They
differed — but they would have differed anyway, because emitting `EventBusted`
advances `EventSeq` and the digest covers that. It passed with the registry removed
from the snapshot entirely.

**Fixed by** comparing two snapshots that differ in *nothing else*: the same
snapshot with `Busted` cleared. The property is now isolated from its side effects.

### 2. A checksum test satisfied by a magic number

*[MULTI-SYMBOL.md](MULTI-SYMBOL.md) §7.* The claim: a manifest altered on disk
fails its CRC and is refused rather than believed.

The test corrupted the file by flipping its first `0` byte. That byte lives inside
the magic string's `\u0001` JSON escape, so the corrupted file failed the *magic*
check — and went green with the CRC comparison disabled.

**Fixed by** editing a symbol's index through the parsed JSON and leaving the
checksum alone, which is the corruption a CRC exists for: one that parses
perfectly.

### 3. A timing test whose noise reached its threshold

The claim: an unknown account does the same work as a wrong secret, so login timing
cannot enumerate participants.

Two versions of this failed, in both shapes. The first used a 64-byte secret and
**passed against a short-circuiting `Authenticate`**, because at that length the
comparison costs less than the map lookup before it. Rewritten with a 64 KiB secret,
it then **failed on correct code** under parallel load — 4.19 against a threshold of
4, twice — because it summed wall clock, which measures the work plus however long
the scheduler kept the goroutine off a core.

**Fixed by** taking the floor of several rounds rather than their sum: noise only
ever adds time, so the minimum estimates the real cost. The signal against a genuine
short-circuit is now ~8000× the threshold rather than barely over it.
[SOAK.md](SOAK.md) reached the same conclusion about heap growth — *watch the floor,
not the trend* — which is worth noticing: the repository already knew this and the
test had not caught up.

### 4. A drill that blamed the wrong follower

*[REPLICATION.md](REPLICATION.md) §6, drill D6.* The claim: a wedged follower is
shed rather than waited on.

The drill drove traffic until `Shed() != 0` and assumed the follower cut was the
wedged one. A follower that actually *applies* commands is slower than a wedged
socket, which merely fills a kernel buffer at no cost to the primary — so about one
run in twelve the healthy follower's buffer overflowed first, and the drill reported
"shedding the wedge broke the healthy follower", the opposite of what happened.

**Fixed by** attributing the shed (`Primary.ShedPeers`) so the drill can wait for the
*wedge specifically*, and pacing the tape so there is one candidate rather than two.

### 5. A test double more permissive than the real thing

*v0.24.0.* The claim: `cmd/obdash` speaks the market-data protocol correctly, and is
the external consumer proving [PROTOCOL.md](PROTOCOL.md) is writable from the format
alone.

When the wire went to v4, `MDSubscribe` gained a `Symbol` the venue refuses a
subscription without. The dashboard stopped being able to connect **and every one of
its tests passed**, because its fake venue decoded the subscribe and never looked at
the field.

**Fixed by** making the double refuse exactly what the real venue refuses. A test
double more permissive than the thing it stands in for certifies the wrong system —
and it is the one kind of sabotage you cannot discover by breaking the *code*, since
the code was fine and the environment was wrong.

## What this is not

- **Not mutation testing.** No tool, no coverage of every mutant. One targeted
  sabotage per claim, chosen by the person who knows what the claim means.
- **Not a substitute for negative cases.** A test with only positive assertions
  usually fails this exercise, but passing it does not excuse never testing the
  refusal path.
- **Not free.** It costs a few minutes per test — and every case above was caught
  in under two minutes of it, before the test had been trusted by anybody. That is
  the whole economics: minutes now against a property nobody can later tell was
  never proven.

## Doing it

Sabotage in the working tree, run the one test, restore. Keep the restore
mechanical so a broken tree cannot be committed by accident:

```sh
cp pkg/matching/snapshot.go /tmp/snap.bak
#   ... make the specific defect ...
go test ./pkg/matching/ -run TestBustChangesTheDigest -count=1   # must FAIL
cp /tmp/snap.bak pkg/matching/snapshot.go
go build ./...                                                   # must build
```

Then say so where it will be read: the drills name their sabotage in a file header,
and the specs record the ones that caught something in §7. A sabotage that found
nothing needs no ceremony; one that found a sleeping test is the most useful thing
you will write that day.

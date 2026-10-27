# Account Hint — Interning an Account Once, Not on Every Add

Status: **accepted 2026-10-27 (§7 answered); §5's upper bound is measured first** · Part of M11 in
[`PERFORMANCE-ROADMAP.md`](PERFORMANCE-ROADMAP.md), following
[`BOOK-LADDER.md`](BOOK-LADDER.md) §7 · Author: Karthikeyan NG · 2026-10-27

## 1. The cost

Every `OrderBook.Add` turns the order's account string into a dense `int32`
(`userTable.intern`, `pkg/orderbook/index.go`). The `int32` lives on the book node, so
a cancel never hashes the string again, and it indexes the per-account resting-order
counts behind `MaxOrdersPerAccount`.

Profiled on `main` after the ladder (`BenchmarkTapeReplay/sink=nil`, M4, 10 s,
2026-10-27): `Engine.Match` is 4.04 s, and `userTable.intern` is **0.41 s of it,
about 10%**. That is a string hash plus a compare in `mapaccess2_faststr`, and it is
the only string-keyed lookup on the tape's path.

The caller almost always knew the account long before the order arrived. `obgw` has
it on the session, `pkg/fix` on the session, a Python caller in a variable. It is
hashed again on every order anyway.

## 2. Constraints

1. **No answer may change.** Per-account counts, `MaxOrdersPerAccount`, ownership
   checks on cancel, STP, snapshots, digests. A hint must only ever skip work.
2. **The public API grows only by addition.** `internal/apicheck` records it.
3. **Single writer.** A book's account table is written on its matching goroutine
   (or under its mutex). A caller on another goroutine cannot be handed the table.
4. **Many books per caller.** An `obgw` session's orders go to several books, and each
   book interns into its own table, so one session account has a different `int32` in
   each book.
5. **No `unsafe`** outside tests (M11's hot-path rules). That rules out a cache keyed
   by the string's data pointer.

## 3. Options

| Option | What | Verdict |
|---|---|---|
| A. **Verified hint on the order** | `types.Order` gains `AccountHint int32`. A book given a hint checks it with one string compare against the name it stored for that id. Pass → no hash. Fail → interns as today | **proposed** (§4) |
| B. Shared account registry across books | one name↔id table for many books | ids agree across books, but the table becomes shared mutable state across matching goroutines: a lock or a copy-on-write scheme on the hot path. More machinery for the same saving |
| C. Last-account cache in the table | remember the last name and id | no API change, but it only hits when one account sends consecutive orders. At a venue with many interleaved clients it rarely hits. It could be added later, alongside A |
| D. A new engine entry point taking an `AccountID` | `MatchAs(order, acct)` and so on | needs a variant of every entry point (`Match`, `Process`, `Replace`, the `Runner`'s enqueue methods, the conditionals). A field on the order reaches all of them for free |

## 4. The proposed design (option A)

- **`types.Order.AccountHint int32`**, tagged `json:"-"`. Zero means no hint. It is
  never part of an order's identity, its wire form or its snapshot encoding.
- **`(*OrderBook).AccountHint(name string) int32`** interns `name` under the book's
  write lock and returns its id + 1, so that zero stays "none". A caller asks once per
  account per book, for example on login, or on a session's first order to that book,
  and keeps the value.
- **`Add` with a hint:** the table keeps `names []string` beside the map. With
  `h = order.AccountHint`, if `1 ≤ h ≤ len(names)` and `names[h-1] == order.UserID`,
  the node's account is `h-1`. Otherwise the book interns `UserID` as today. A wrong,
  stale or foreign hint, or one from another book, costs one compare and changes
  nothing. That is the whole safety argument, and it is what §6 tests.
- **The compare is cheap.** When the caller uses the same string value it interned
  (the common case: a session's account field), Go's string equality sees equal data
  pointers and lengths and stops there. Otherwise it compares a few bytes. Either way
  there is no hash.
- **`Engine.AccountHint(name)`** forwards to its book, so a caller holding an engine
  need not reach for the book. For a `Runner`, the call takes the book's lock from the
  caller's goroutine. The book is already safe for concurrent use (its package doc, `orderbook.go:11`), so this
  is a lock acquisition per account per book, not per order.
- **Who uses it, in this change:** `obgw` (per session, per book, cached on first
  order) and the bench tape's replay driver, as a second gated variant. Other callers
  (`pkg/fix`, the C ABI, `pkg/itch`) can follow, and the field works for them without
  further API.

## 5. Measure the prize before paying for the API

The API costs something permanent: a public field and two methods. So before any of it
is written:

- **Upper bound, on CI.** A throwaway branch where `Add` skips `intern` entirely and
  uses a fixed account id. It is wrong, and it exists only to measure. Three
  `bench-gate.yml` dispatches against `main`. Its `TapeReplay/sink=nil` ratio is the
  most any hint could win.
- **Rule, set now:** go on to §4 only if that upper bound is **≤ 0.95 in all three
  runs**, i.e. at least 5% of the replay. Below that, the saving is not worth a public
  field. This document records the number and stops.

If it goes ahead, the real change is judged under BOOK-LADDER's method:

- three CI dispatches;
- the hinted tape variant at least 5% faster than the unhinted one, in all three;
- every enforced benchmark passes;
- digests equal.

## 6. Correctness checks

- **A hint never changes an answer.** A model-style test runs random adds and cancels
  with, for each order at random: no hint, the right hint, another account's hint, an
  out-of-range hint, or a hint from a different book. The per-account counts,
  `OrdersByUser`, and `Orders()` must equal those of the same sequence run without
  hints.
- **Sabotage:** skipping the name compare (trusting the hint) must fail that test.
- **Unchanged:** both tape digests, semcheck, refmatch, snapshot round-trips (a
  restored engine has a fresh table, so every hint from before the restore fails its
  check and falls back, which the test covers too), and flash1.

## 7. Open questions for review

*Answered 2026-10-27: the proposals stand. (1) Option A. (2) The upper bound is
measured first, with the 5% bar. (3) `obgw` and the bench replay in this change.*

1. **Option A**, a public `AccountHint` field verified on use, rather than B, C or D?
2. **Measure first (§5)**, with the 5% bar, before any API is written?
3. **Scope:** `obgw` and the bench replay in this change, with FIX, the C ABI and ITCH
   later?

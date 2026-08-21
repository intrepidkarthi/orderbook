# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
versions may include breaking changes).

## [Unreleased]

### Changed

- **CI builds and race-tests on two Go versions rather than one.** Every workflow
  pinned Go 1.23 while 1.27 was the current release, so the project was testing
  neither what the README promises nor what most people now build with. `bench`,
  `pages` and `soak` move to 1.27; `ci.yml` runs a matrix of **1.23 and 1.27**.

  The matrix is the point rather than the version bump. The README says "Requires Go
  1.23 or later" and nothing built against 1.23 to check it — a floor nothing compiles
  against is a floor nobody has tested. It now fails a job when it stops being true.

  `go.mod` is deliberately **not** bumped. The `go` directive is the minimum every
  consumer of an embeddable library has to meet, and raising it to a release three days
  old would lock out everyone on 1.23 through 1.26 to gain nothing the matrix does not
  already prove.

- **The Go Report Card badge is gone.** goreportcard.com was sunset on 1 July 2026 and
  `gojp/goreportcard` is archived. The badge endpoint still answers 200 — what it now
  serves is an SVG reading *"go report: retired"*, so the README was advertising a dead
  service in the row of badges a first-time visitor reads first.

- **The README leads with the demo and the quickstart, not the caveats.** Quickstart
  moved from line 204 to line 40: demo GIF, badges, one paragraph of what this is,
  install, quickstart — then the honesty section under its own heading, which the pitch
  links to by anchor rather than burying above the fold. Nothing in the warning is
  dropped; the blockquote, Scope, "What ships" and "What does not, and is yours" all
  survive intact and now sit together instead of restating the deployment-property
  argument across four separate paragraphs.

### Added

- **A published coverage number, and the list of what it counts.** There was no
  coverage report anywhere, so the only way to know the figure was to run the profile
  by hand: **73.4%** across every package, **83.8%** with `cmd/`, `examples/` and
  `legacy/` ignored. `codecov.yml` states the ignore list and why — those packages are
  `main()` wiring and runnable demonstrations, and what is under them is covered by
  `pkg/` and by `cmd/obgw`'s own end-to-end suite. The project status target is 80%,
  which is the floor it must not fall through and not the number it is today.

### Fixed

- **The published test count and fuzz-target count in
  [PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md).** The count read "over 600"
  against an actual 844, which is the floor degrading the safe direction it was
  designed to degrade. The fuzz-target count next to it read "two" against an actual
  **three** — `FuzzDifferential` shipped in 0.26.0 and this line did not move with it,
  which is not a floor going stale but a figure that was simply wrong.

  Corrected to "over 800 test functions, three fuzz targets". The historical statements
  elsewhere — the soak write-up's "480 tests, two fuzzers", and this file's own "as it
  then stood (480 functions)" — are left alone, because they describe what existed at
  the moment being narrated.

- **The README still published `Add` at 1.05 allocs/op against a measured 2.01.**
  v0.25.0 corrected this figure in [BENCHMARKS.md](docs/BENCHMARKS.md) — including a
  paragraph on it having been wrong in the flattering direction — and did not correct
  the same sentence in the README, so the number the most-read page carried went on
  understating the cost of growing a book by half for another release.

  Re-measured rather than copied across: `TestAddAloneDoesAllocate` prints **2.0101**
  allocs/op, 94.57 B/op. The two figures beside it were already right (cancel 0.0002,
  cancel + replace 0.0000). It is a deterministic allocation count, not a timing, so it
  was in the test log the whole time — which is the same way the original error survived.

- **The conformance-suite scenario count read 23 against an actual 28.** The count was
  correct at v0.25.0 and 0.26.0 added five scenarios to `TestEventStreamReconstructsBook`
  without moving it — three fill-or-kill × STP combinations among them. Counted by
  running the test rather than by reading the table, because the STP cases are generated
  per mode rather than written out. Corrected in the README and in
  [PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md).

- **`PERFORMANCE-ROADMAP.md` said CI had "never two Go versions".** That stopped being
  true in this release. It now says what is the case: still one OS, still one toolchain
  for the benchmarks, and the benchmark matrix still does not exist.

## [0.26.0] - 2026-08-21

### Added

- **What sharding actually scales to, measured.** `matching.Shards` gives each symbol
  its own single-writer book, and two documents said distinct symbols "scale linearly
  across cores". Nobody had measured it, and it is not true.

  `BenchmarkShards_Scaling` (`pkg/matching/shard_bench_test.go`) routes `b.N` operations
  across N shards, one producer goroutine per shard, on a 70/20/10 rest / cancel /
  marketable mix holding ~2 K resting orders per book. Pinned to the four performance
  cores of an M4, median of five 10-second runs: **876 K ops/s at one book, 1.64 M at
  two (1.88×), 1.97 M at four (2.24×)** — and then nothing, 2.00 M at six and 1.99 M at
  eight, inside the ±4% run-to-run spread.

  The reason is the shape of a shard rather than anything in the matching. Each shard is
  a *pair* of goroutines, a producer blocked on its reply channel and the matching
  goroutine draining the command queue, so N books want 2N runnable goroutines and past
  the core count the machine goes into the handoff. Books beyond that buy queue
  headroom, not throughput, and venue capacity is not symbols × single-book throughput.

  The 4 allocs/op corroborates the durable-path table rather than contradicting it:
  `Runner.Process` is 3, and this benchmark allocates the order *inside* the timed loop,
  which the single-threaded benchmarks hoist out. New section in
  [BENCHMARKS.md](docs/BENCHMARKS.md#scaling-across-cores), summarised in the README.

- **The venue counts what it refuses and times what makes an order durable.** Sixteen
  metric families already said what the venue *did*; nothing said what it *dropped*,
  and the durability path was untimed.

  `obgw_refused_total{reason}` counts every refusal at `session.reject` — the single
  funnel all fifty refusal sites already pass through — and increments **before** the
  encode, so the counter can never claim a rejection the client did not get. Measured
  equal end to end: 51,568 counted against 51,568 `CmdReject`s received.
  `obgw_login_refused_total{reason}` is separate because a failed login is written
  straight to the socket and never reaches that funnel.
  `obgw_shed_unreported_total` counts the shed with nobody left to tell: a disconnected
  client's orders left resting because the cancel could not be queued. It cannot appear
  in the refusal counter by construction, which is why it is its own series.

  `obgw_wal_append_latency_ns` and `obgw_wal_sync_latency_ns` are timed separately and
  on the right side of the group commit. That boundary is the point — a sync latency
  that is really an append latency reads healthy while fsync is the slow thing. The two
  are 225× apart under `-sync-every-command` (17 µs against 3.9 ms), so they are
  demonstrably not measuring each other. This is also the variable half of the recovery
  point objective, which is `20 ms + p99 fsync` and not the 20 ms ticker alone;
  corrected in `pkg/wal`'s package comment and PRODUCTION-READINESS.

  `orderbook_snapshot_age_seconds{symbol}` reads the file's mtime rather than a
  process-local timer, so a restarted venue cannot report a fresh checkpoint it never
  took. `/readyz` reports the degradation without failing it: a venue that cannot
  checkpoint but can still trade is not one to pull from rotation, and saying so is
  different from staying silent. Plus `obgw_snapshot_duration_ns`,
  `obgw_snapshot_failures_total{symbol}` and `obgw_recovery_duration_ns{symbol}`.

  Thirteen threshold rows in [RUNBOOKS.md](docs/RUNBOOKS.md), each with a normal value,
  a trouble value and an action, because a metric nobody has a threshold for is a
  metric nobody looks at. Limitations sit next to their thresholds rather than in a
  footnote: the WAL histograms are venue-wide and merged across books, and the sync
  quantile saturates at its top bucket.

  Cost: **82 ns and zero allocations** per timing — about 5% of an append, under 0.5%
  of the group-committed write path. `TestZeroAllocHotPath` passes unmodified and
  `internal/semcheck` is green without regeneration, which is what proves this slice
  did not change matching. Design, thresholds and thirteen sabotage runs:
  [LAG-AND-SHED.md](docs/LAG-AND-SHED.md).

  **Two things adversarial review caught before they shipped.** A paging threshold of
  1 s stood against a histogram whose top bucket was 250 ms, so that tier could never
  fire and an operator would have read healthy through an arbitrarily slow disk — the
  buckets now run to 5 s. And failed logins were invisible to a counter whose name
  implied it covered refusals.

- **A reference matcher, and a random-tape differential harness that compares it against
  the engine after every command.** This is milestone M9's central ask, which the last
  roadmap survey found did not exist in any form. Spec, and the record of what building
  it found: [REFERENCE-MATCHER.md](docs/REFERENCE-MATCHER.md).

  `internal/refmatch` is a limit order book written to be READ: two sorted slices
  scanned linearly, cancel by linear search, depth by a fold. No index, no pool, no
  id-to-node map. It is slow on purpose and must never be optimised — a model with an
  index has the engine's bug class. It imports the standard library and nothing else,
  and `TestReferenceMatcherImportsNothing` parses the files to keep it that way.
  `pkg/types` is the tempting exception and it is refused: `types.Order.Fill` is where
  "filled + remaining == quantity" is maintained, and an invariant both sides get from
  the same seven lines is an invariant nothing is checking.

  Equivalence is a single comparable `Observation` produced by both sides and compared
  **whole** with `reflect.DeepEqual` — not field by field, because a field-list
  comparator is where a future field silently stops being checked. It carries the
  verdict as a mapped enum (error strings are not a contract), the command's trades
  field by field, the resting book as a **ranked** L3 list, the L2 aggregate
  **asymmetrically** (the engine's maintained `TotalQty` and `count` against the
  model's summed L3), order and trade ids exactly including the shard field, the event
  stream as an ordered list, and the state, last trade price and both next-id counters.
  Exactly three things may differ, each with what still constrains it.

  **Twenty-one deliberate engine mutations are all caught, and every shrunk
  reproduction is 1 to 4 commands** — a LIFO price level, a maker/taker price
  inversion, a partial fill leaving the wrong remainder, an off-by-one in a level
  aggregate, a reduce that re-queues, an IOC that rests, `nextID` moved below the
  admission checks, and fourteen more. The failure message prints the shrunk tape as a
  pasteable `[]tape.Cmd` literal alongside the seed, the profile, the divergence class
  and a field diff.

  The harness's own guards were then sabotaged twelve ways, and the two that did not
  behave as specified are written down rather than quietly fixed
  ([REFERENCE-MATCHER.md](docs/REFERENCE-MATCHER.md) §10.3). Deleting the L2
  comparison makes two mutations pass against a broken engine; comparing the book as a
  set makes a third pass. One seed catches 8 of 18 mutations, the sweep catches 18 of
  18, and that number is the argument for the sweep's size.

- **`internal/tape`: one command generator for the whole repository, and a shrinker.**
  `buildTape`, `tapeCmd`, `tapePhases` and `lcg` have moved out of `pkg/wal`'s tests,
  and `pkg/wal` is now a consumer. Two generators means two alphabets, means one of
  them is extended and the other is not, means the sweep that claims to be the stronger
  test exercises less than the one it should dominate.

  The command format is **deletion-closed**: a command names an earlier order by the
  POSITION of the submit that created it, not by an index into the ids collected so
  far. Deleting a submit therefore leaves later commands naming a position that
  produced no order — a well-formed command with a predictable rejection — instead of
  silently retargeting every later cancel onto a different order, which is what would
  make a shrinker lose the failure it is shrinking.

- **`FuzzDifferential`, a committed seed corpus, and a nightly time-bounded `-fuzz`
  job.** The committed sweep in `go test ./...` stays a fixed handful of seeds, because
  a test people will actually run beats a nightly job they will not.

- **`orderbook.PriceLevel.Count`** exposes a level's maintained order count, and
  `GetBidLevels`/`GetAskLevels` now carry it on the copies they return. It is the
  maintained count and not a walk of the list, which is the point: comparing it against
  an independent count is what catches the two drifting apart.

- **`TestIceberg_RefillGoesToTheBackOfTheQueue`.** A refilled iceberg slice is new
  liquidity and joins its price level behind everything already resting there.
  `engine.go` stated the rule in a comment at the refill site and nothing asserted it.
  It matters because it is a fairness rule with money attached: an order that kept its
  place through every refill would take priority it never queued for, which is the
  advantage displaying liquidity is supposed to buy.

- **`TestSnapshotRestoreEqualsUninterruptedExecution`.** Restores a snapshot at three
  fork points per tape and drives the **remainder** of the tape through the restored
  engine, comparing every observation against the model. Nothing in the repository
  previously restored a snapshot and then kept trading against it.

- **`TestRecoveryTapeSpeaksTheTierOneAlphabet`** and **`TestOrderAttributesAreGenerated`**,
  two guards against the alphabet silently narrowing again, both asserting reached
  rather than drawn wherever an outcome is observable. **`TestDifferentialTapeIsPinned`**
  protects the recorded mutation evidence from a generator change that re-rolls the
  tapes while every test still passes — which one draft of `ExoticDamp` would have done.

### Fixed

- **A refused iceberg no longer makes the venue's own snapshot unloadable.**
  `ProcessIceberg` registers the iceberg in the engine's registry *before* settling it —
  the maker-side refill needs to find it there — and did not undo that when the settle
  refused. `TakeSnapshot` then wrote an entry for an order that is not on the book, and
  `LoadSnapshot` refuses such a snapshot outright with *"iceberg N has no resting
  displayed slice"*. **One refused iceberg and the venue could never load a checkpoint
  again** — not after further trading, not after a restart — and 1000 of them left 1000
  rows in every snapshot, so the reject path grew memory per client.

  It is pre-existing, reachable before this release through a dust iceberg or a halted
  venue, and it is fixed **now** because the change above makes it the routine outcome of
  its own headline rejection: an over-cap iceberg used to rest and is now refused. It
  also landed on this release's own upgrade path — the runbook tells an operator to take
  a checkpoint after accepting a semantics mismatch, and measured end to end, that
  produced a venue that would not restart.

  The repair is the invariant rather than the symptom: the registry holds icebergs whose
  displayed slice is **resting**, so the entry is dropped whenever the command ends
  without one. That covers a rejection for any reason — a halted venue, any of the five
  caps, a band breach, a post-only that would cross, a fill-or-kill that cannot fill —
  and the `CANCELLED` immediate-or-cancel remainder as well, which a
  rejection-only repair would have left open.
  [ICEBERG-ADMISSION.md](docs/ICEBERG-ADMISSION.md) §13.4.

  Not fixed, and deliberately: the identical symptom reached through self-trade
  prevention under `DECREMENT`, where a maker iceberg is removed without refilling. That
  one destroys six lots of a client's order, and a loadable snapshot there would hide it
  rather than repair it.

- **A recovery from the journal alone no longer loses every iceberg's hidden reserve.**
  `types.NewIcebergOrder` shrinks the order it is handed to the display size and keeps
  the remainder on the wrapper, so by the time `AppendIceberg` could journal anything,
  `Order.Quantity` was already the slice — and `Hidden` was never written at all. A
  nine-lot iceberg shown three was recorded as `quantity 3, display 3`, and a replay
  rebuilt it with `hidden = 3 - 3 = 0`. **A venue recovering from its log alone came
  back with every client's reserve gone**, which is the path a venue takes when its
  snapshot is missing, refused as corrupt, or below the retention floor. A recovery
  *with* a snapshot was never affected; `EngineSnapshot` carries the reserve.

  The cost is not one order. Measured on `iceberg 9 shown 3, buy 9, sell 5` with no
  fill-or-kill anywhere: the recovered venue printed 2 trades for 8 lots where the venue
  printed 3 for 9, **printed five lots at 100 that the venue never printed** — the buy
  that should have been filled by the reserve rested instead and was hit by the next
  order — and ended holding the opposite side of the book.

  `AppendIceberg` now logs a **copy** of the order at the client's total
  (`TotalRemaining()`), leaving the live order the engine is matching against untouched,
  plus `Entry.TotalQty`: the same number a second time, whose only job is to be
  **absent** in records written before this change. A reserve field could not have done
  that — `omitempty` drops a genuine zero reserve, so a pre-fix record with a lost
  reserve and a post-fix record with no reserve would be byte-identical. Cost: **14
  bytes on `KindIceberg` records only** (311 → 325), zero on every other kind, no
  framing, magic, header, CRC or snapshot change, and every existing record
  byte-identical. A hand-built record with the total in `Quantity` and no `TotalQty`
  still restores correctly.

  **A log written before this change is refused rather than replayed into a wrong
  book** — `wal: iceberg reserve unknown`, naming each record's sequence and the order
  it owns. It fires if and only if such a record would be APPLIED: one the snapshot
  already covers is read, verified, skipped and never refused, so a venue that
  checkpointed before upgrading starts with no ceremony. `ReadAll`, `Open`, `Restore`
  and `RestoreAfter` never refuse. The override is a **count** —
  `-wal-accept-iceberg-loss N`, which must equal the number found — because a boolean
  goes into a unit file during one incident and stays for the life of the deployment,
  and this build cannot produce such a record. It relaxes that gate and nothing else.
  New: `RecoverReport.IcebergsWithoutReserve`, `.IcebergReserveLossAccepted` and
  `obgw_recovery_iceberg_reserve_unknown_total{symbol}`, whose normal value is 0
  forever. Runbook: "An iceberg whose reserve was never journalled".

  **`matching.SemanticsVersion` does not move, and that is argued rather than assumed.**
  `pkg/matching` is untouched; the engine was never wrong, it correctly executed a
  command the journal had corrupted. What moved is the translation between bytes and
  commands. Bumping would also refuse *every* semantics-2 segment with unreplayed
  records on every venue, iceberg or not — a check firing on the happy path, which is
  how an override ends up permanent. `internal/semcheck` is green **without
  regeneration**, which is the proof.

  The audit behind it asked the question that mattered — *is the iceberg the only one?* —
  and answered it for stop, stop-limit, OCO, pegged and trailing: **it is**, including a
  stop that fired mid-tape and a trailing stop ratcheted three times, both of which come
  back exactly because the log carries the trades that produced them. That audit is now
  three standing guards (a log-only round trip per wrapper-carrying `EntryKind`, a
  no-mutating-constructor tripwire that enumerates constructors from the package's own
  source, and a field-by-field classification of every wrapper type), and the two
  adjacent findings it turned up are **pinned, not fixed**: an iceberg evades
  `Config.MaxOrderQty` by exactly its hidden quantity, and `Config.MinOrderQty` refuses
  a 90-lot iceberg shown 3 for a floor it exceeds 18×. Replication followers rebuild
  icebergs from the same records and had been diverging from their primaries silently,
  with no iceberg anywhere in `examples/`; that is repaired and now drilled.
  [ICEBERG-DURABILITY.md](docs/ICEBERG-DURABILITY.md).

- **A failing fill-or-kill no longer corrupts an iceberg it consumed.** A fill-or-kill
  that exhausted a resting iceberg's reserve and was then refused left that client's
  order with a **negative `FilledQty`**, its entire hidden reserve **displayed** in the
  open, a shown size larger than the order's own quantity, and its refill registration
  dropped so it never reloaded again. Measured on the two-command reproduction: 9 total
  / 3 displayed became `FilledQty -6`, `RemainingQty 9`, reserve 0, best ask `100:9`.
  An iceberg exists to hide size; one rejected order from an unrelated account leaked
  all of it, permanently.

  The cause was two paths disagreeing about who owns the rewind. `reverseTrade` assumes
  a maker still carries the fills a print made against it, and `IcebergOrder.Refill` is
  the only code in the engine that breaks that: it resets `Quantity`, `FilledQty` and
  `RemainingQty` on the *same* order object and re-adds it under the same id, so three
  prints against three slices are three prints against one object whose counters
  describe only the last. The code that breaks a precondition pays to restore it, so
  the walk now **saves an iceberg's whole state the first time it is about to trade
  against it** and the failure branch **restores it whole** — slice, fill counters,
  status, reserve, refill counter and registry entry — with those prints never passed
  to `reverseTrade` and their refill `ACCEPTED`s dropped alongside the reversed trades.
  `reverseTrade` itself is unchanged; its precondition is now written on it.

  A rejected fill-or-kill now leaves an iceberg exactly as it found it — with ±20% peak
  jitter on a 30/5 iceberg, the state after `[iceberg, failed FOK, buy, buy, buy]` is
  identical in every field to `[iceberg, buy, buy, buy]`. The published L2 aggregate and
  the L3 stream both stop reporting size the book does not have.

  **Queue position included**, and it took a second pass to get right. The restore
  happens at the iceberg's *first* reversed print rather than ahead of the whole
  reversal, so the slice re-enters its level exactly where an ordinary maker put back by
  the same reversal would: behind everyone whose print came earlier, ahead of everyone
  whose print came later. Restoring it up front instead — which is what the first cut of
  this fix did — handed the iceberg time priority over makers that had been resting *in
  front of* it, a change no counter, aggregate or event could witness because the level
  keeps the same order ids and only their order is wrong. The bound that remains is the
  one `reverseTrade` already had for every maker: a maker left resting mid-level by a
  self-trade-prevention decrement keeps its place, and everything the walk removed
  re-enters behind it.

  **It is not retroactive.** `EngineSnapshot` carries each iceberg's reserve and refill
  count, so an order already corrupted on a running venue survives a snapshot round
  trip unrepaired — a snapshot is a state, not a program. The remedy is to cancel and
  re-enter the order, and the corruption is recognisable: `FilledQty < 0`, or a
  displayed size larger than the order's `Quantity`. Replay is a different matter and
  is covered by the semantics bump below.

  Nothing in the invariant suite caught this, which was the second half of the defect:
  `filled + remaining == quantity` still held (−6 + 9 = 3), and the check only ever ran
  on the order just submitted — the taker, the one order a reversal defect cannot be
  on. `checkInvariants` is now a whole-book check that also asserts `filled >= 0`, that
  a resting order has remaining size and an active status, and that each level's
  maintained aggregate equals the orders actually resting there;
  `FuzzExoticOrders` gained a fill-or-kill symbol in the same change, because an
  invariant with no reachable input is decoration. Design, alternatives and sabotage
  runs: [PINNED-DEFECTS.md](docs/PINNED-DEFECTS.md).

- **`OrderBook.Add` no longer refuses a duplicate id when the book is full**, and no
  longer advances the book sequence for an add that changes nothing. The method
  documents duplicate ids as ignored, but it checked capacity and bumped
  `sequenceNum` *before* looking up a caller-assigned id, so the same duplicate was
  silently ignored below capacity and rejected with `ErrOrderBookFull` at it — a
  documented contract whose outcome depended on unrelated capacity state.

  Reported and fixed by [@Taz33m](https://github.com/Taz33m) in
  [#8](https://github.com/intrepidkarthi/orderbook/issues/8) /
  [#9](https://github.com/intrepidkarthi/orderbook/pull/9). Both halves were
  reproduced against `main` before merging, and the strengthened
  `TestDuplicateAddIgnored` fails against the unfixed code with the reported error.

  **Not a semantics bump, and the fingerprint is what decided that.** The changed
  branch needs a duplicate *caller-assigned* id, and `pkg/matching` never produces
  one: it re-adds an order only after removing it (`reverseTrade` restoring a maker,
  an iceberg refilling its visible slice), so the duplicate arm is unreachable from
  the engine. `internal/semcheck` stayed green across the merge, which is the
  enforcement declining to fire on a change it should not — the false-positive half
  of [SEMANTICS-VERSION.md](docs/SEMANTICS-VERSION.md) §5, exercised by an outside
  contribution rather than by a sabotage run.

- **A rejected fill-or-kill no longer moves `LastTradePrice`.** The three defects the
  reference matcher found were pinned rather than repaired, because each had more than
  one defensible answer; [DIFFERENTIAL-FINDINGS.md](docs/DIFFERENTIAL-FINDINGS.md) is
  the document that picked between them, and this is the first of the three.

  `match` recorded the last price from the final print, `settleInto`'s fill-or-kill
  branch then reversed every one of those prints, and nothing put the price back. The
  rule now stated in `recordLast`'s doc comment is that **`LastTradePrice` is the price
  of the last trade this venue PUBLISHED** — every consumer of it wants that reading
  (the collar's reference when no mark is set, stop and trailing-stop triggering, the
  band-tick depth window, the "last" a subscriber prints, the mid fallback in
  `pkg/study` / `pkg/sim` / `pkg/backtest`) and none wants "a price at which a match was
  attempted and undone". An IOC's partials are published and stand; a busted print was
  published and stands; a reversed fill-or-kill print reached no sink, no
  `MatchResult`, no book and no journal, so `settleInto` restores the pre-match value
  alongside `reverseTrade`.

  **This is a behaviour change and the two consequences are visible from outside.**
  *Stops that used to fire no longer fire*: with a stop resting at trigger 100 and
  nothing ever traded, the rejected order's phantom price fired it and really printed 2
  lots at 100 between two accounts that had not sent the rejected order. *A collar that
  used to be armed by a rejected order is no longer armed by it*: with `PriceBand` at
  10% and nothing ever traded, one rejected fill-or-kill armed the band and it then
  refused an unrelated buy at 150.

  `tradeSeq` is still burned, deliberately: an id is a **name**, and a counter that
  goes backwards can name two prints once; a price is a **datum**, and restoring it
  makes it correct again. The capture is in `settleInto` and not in `Match` because
  `cascadeStops` re-enters `settleInto`, and a fill-or-kill fired as a stop must
  restore the reference its own walk started from —
  `TestNestedFOKRestoresItsOwnWalksReference` is what distinguishes the two placements,
  and it is hand-written because stops are tier 2 and no generated tape reaches one.
  `TestRejectedFOKStillMovesTheLastTradePrice` (inverted, keeping its name),
  `TestRejectedFOKDoesNotFireAStop`, `TestRejectedFOKDoesNotMoveTheBand`.

### Changed

- **The linear-scaling claim is corrected in all three places that made it**, now that
  there is a measurement: `pkg/matching/shard.go`'s type comment,
  [INTEGRATION.md](docs/INTEGRATION.md) "Multi-symbol scaling", and
  [MULTI-SYMBOL.md](docs/MULTI-SYMBOL.md) §2. Each states what it used to claim rather
  than quietly reading differently. MULTI-SYMBOL §2's argument survives the correction:
  a serialisation point takes 2.24× to 1×.

- **Every document in `docs/` is now reachable from the README.** Twelve were not,
  including MULTI-SYMBOL, LAG-AND-SHED, PINNED-DEFECTS and REFERENCE-MATCHER. They are
  listed as a "Design records" group rather than swelling the reference table to
  thirty-four rows. Also fixed: one broken anchor in [CONFIG.md](docs/CONFIG.md), and
  `examples/multisymbol`'s header comment, which claimed the reference gateway still
  served a single instrument — untrue since `cmd/obgw` grew `-symbols`.

- **The per-order size and notional caps measure the quantity the CLIENT submitted, so
  an iceberg is judged by its total and not by the slice it displays.**
  `matching.SemanticsVersion` moves **2 → 3**: a venue that sets any of these controls
  and accepts icebergs now accepts a different set of orders, and a log written by an
  older build will be refused on replay until an operator accepts the mismatch
  ([SEMANTICS-VERSION.md](docs/SEMANTICS-VERSION.md) §1.2 row 3,
  [RUNBOOKS.md](docs/RUNBOOKS.md) "An iceberg refused on replay by a cap that did not
  exist for it").

  `types.NewIcebergOrder` overwrites the order's `Quantity` with the display size before
  anything downstream sees it, and `checkOrderCaps` read `order.Quantity`. With
  `MaxOrderQty = 5` a plain sell of 9 was refused and **the same nine lots shown three
  rested with six in reserve** — the fat-finger cap evaded by exactly the quantity the
  venue cannot see, and evaded *at the client's option*, since anyone wanting to exceed
  the cap sets `displayQty = MaxOrderQty`. The mirror was worse to receive: an iceberg
  of 90 shown 3 was refused by a `MinOrderQty = 5` dust floor it was eighteen times
  above.

  An audit of every consumer of `order.Quantity` found **five** checks reading it, not
  the two that were pinned: both quantity caps, both notional caps, and the int64
  notional overflow guard — which has no configuration knob, which `Privileged` orders
  do not bypass, and which its own comment calls an arithmetic invariant. Measured:
  `1.84e17` lots at price 100 was `REJECTED:NOTIONAL_OVERFLOW` as a plain order and
  **rested on the book** as an iceberg shown 3. All five now measure
  `DisplayQty + Hidden` at submission — the same number the journal already records.
  `MaxOrdersPerAccount` and the price band deliberately did **not** move, and are now
  asserted rather than assumed: one counts orders (an iceberg is one order however many
  slices it shows) and the other tests a price.

  Nothing a market-data consumer sees has changed. `L2Feed` still publishes the
  displayed slice and prints are still prints — publishing the total there would
  announce every reserve on the venue, which is the failure mode a careless version of
  this fix has. `pkg/marketdata` gained the assertion that says so, because the
  sabotage that was supposed to catch a reserve leak was run and **nothing failed**:
  there was no iceberg anywhere in that package's tests.

  Two things adversarial review changed after the fix was written, both recorded where
  they went wrong rather than quietly corrected. **The exemption that lets a refill skip
  admission is a separate method, not a reserved quantity.** It was first built as
  `admitQty <= 0` and then as a named `-1`, and both are the same mistake: there is no
  `int64` an order cannot carry, and `pkg/wal` replays a decoded record's order
  directly, so an order of quantity exactly `-1` skipped every cap and rested on the
  book at `-1` lots — a regression introduced by the fix for a regression, missed by a
  guard that looped over `{0, -5}`. **And the fingerprint could not see the notional
  half at all**: reverting it alone left `internal/semcheck` byte-identical, so the
  corpus gained a `notional` scenario and three rejection kinds it had never reached.

  Spec, the audit, four decisions and eighteen sabotage runs:
  [ICEBERG-ADMISSION.md](docs/ICEBERG-ADMISSION.md).

- **Admission no longer runs again when an iceberg refills**, which repairs a defect no
  pin covered: the refill loop re-judged each refilled slice against the ingress
  controls and threw the verdict away. Measured with `MinOrderQty = 2`, ten lots of
  depth and an aggressive iceberg of 10 shown 3: nine lots traded, the tenth was refused
  inside the engine for being below the dust floor, no event was published, nothing
  rested — and the client was told `FILLED`. A lot of a client's order evaporated. The
  rule is that the ingress size, notional and account controls run when a **command**
  arrives, and a refill is not a command; the maker-side refill has never run them at
  all, so the two refill paths now agree. The visible consequence is that a refilled
  slice smaller than `MinOrderQty` rests: it is the tail of an order the venue already
  accepted, and refusing an order's own tail leaves the client holding a quantity the
  venue will neither trade nor return.

- **A cascade-fired stop or trailing stop whose order the venue refuses now publishes a
  `CANCELED` for that order after its `ACCEPTED`, and a consumer must apply it.** This
  is a new event in a stream consumers reconstruct the book from; a consumer counting
  `CANCELED` events will see more of them.

  When a stop fires from inside another command's walk, `cascadeStops` announces its
  order `TRIGGERED` and then `ACCEPTED` before settling it. If settling **refused** it —
  a fill-or-kill that cannot fill — the refusal reached nobody: the terminal event fired
  only on a cancelled status, and a cascade-fired order never reaches the submit path's
  event composer, so both its status and its rejection reason were discarded. The
  stream said an order entered the book and never said it left. Reproduced at six
  commands: a consumer's reconstruction held `3@200:50` forever while the engine held
  nothing, and `pkg/marketdata`'s L2 feed published 46 lots at 200 that the book did not
  have.

  The kind is `CANCELED` and not `REJECTED`, deliberately. `CANCELED`'s documented
  meaning is already "order removed: cancelled, **or terminated without resting**",
  every consumer already treats it as a delete and none treats a `REJECTED` as one — so
  `REJECTED` here would ship a new rule for every consumer inside the patch whose whole
  point is that consumers are already wrong about this order. The reason survives on the
  event: `Order.Status` reads `REJECTED` and `Reason` carries `ErrFOKCannotFill`, which
  `pkg/orderentry` turns into a client-facing cancel with a reason code — a message the
  client whose contingent order was refused has never received before and should have.
  Ordering is `TRIGGERED → ACCEPTED → (prints, dropped if reversed) → CANCELED`, in the
  same batch; the `CANCELED` deliberately comes *after* the `ACCEPTED`, because before
  it a reconstruction deletes an order it has not yet added and the phantom returns.

  **One widening beyond the defect, named rather than discovered:** a cascade-fired
  *market* order that finds no liquidity already ended cancelled and already published a
  `CANCELED`; that event now carries `ErrMarketOrderNoLiquidity` where it previously
  carried none. It is strictly more information on a field that is already optional.
  [PINNED-DEFECTS.md](docs/PINNED-DEFECTS.md) §4.

- **`matching.SemanticsVersion` is 1 → 2**, covering the two entries above (the iceberg
  fix under *Fixed*, and the cascade `CANCELED` here). Only the iceberg fix is
  replay-visible — a journal containing a fill-or-kill that failed after consuming at
  least one full slice of a resting iceberg replays to a **different, and correct,** book
  on this build, and `wal.Recover` will refuse a pre-upgrade segment whose records it is
  about to apply. RUNBOOKS' "Upgrading across a semantics change" is the procedure,
  unchanged. Registry row: [SEMANTICS-VERSION.md](docs/SEMANTICS-VERSION.md) §1.2.

  That replay sentence holds for a recovery that starts from a **snapshot**, and the
  bound is stated here because a reader would otherwise assume it holds for both paths.
  A snapshot carries each iceberg's reserve and refill count; a journal does not, because
  `AppendIceberg` logs an order whose quantity has already been shrunk to the display
  size, so a **log-only** recovery rebuilt every iceberg with an empty reserve and the
  fixed path was never reached on it. That was pre-existing and unchanged by this release
  — it reproduced identically on the previous build with no fill-or-kill involved — and it
  was pinned by `TestLogOnlyRecoveryLosesAnIcebergsReserve`, the first test covering
  iceberg journal recovery at all. [PINNED-DEFECTS.md](docs/PINNED-DEFECTS.md) §13.7.
  **Since fixed, above:** the record now states the client's total, so the sentence holds
  on both paths for logs written by this build, and a log written before it is refused
  rather than replayed small. The pin is inverted and keeps its name.

  The bump is worth a sentence of its own, because it nearly did not happen.
  `internal/semcheck` was **green** on both fixes: its corpus reached icebergs and
  reached stops and never crossed a fill-or-kill with either, so neither defect was in
  the fingerprint and under Rule 22 neither could be bumped for. The corpus gained
  thirteen appended commands first — a fill-or-kill that exhausts an iceberg's reserve
  and fails, a follow-up buy proving the restored reserve still reloads, a stop fired by
  a cascade whose order is refused, and a maker resting *in front of* an iceberg that the
  same failing fill-or-kill sweeps, whose golden line names which of the two kept
  priority — and the number moved on the strength of the thirteen golden lines that
  appeared. Two coverage counters, `IcebergRestores` and `CascadeTerminals`, fail the
  corpus guard if anyone removes them.

- **A journal now declares which matcher wrote it, and recovery refuses rather than
  replaying it into a different book.** The three changes below alter what matching
  does with the same input, so a log recorded before them and recovered after them
  produces state that never existed on the venue that wrote it. Pro-rata is the widest:
  a taker that used to rest across the spread is now cancelled, so every command after
  it replays against a book that differs from the one the live venue had. Until this
  release nothing on disk said so — recovery replayed the log, started, and the book
  was wrong in a way nothing downstream flagged. Spec, and the reasoning behind every
  rule: [SEMANTICS-VERSION.md](docs/SEMANTICS-VERSION.md).

  **`matching.SemanticsVersion` was introduced at 1** (it is **2** on this branch — see
  the bump entry above), and it is neither a release version nor a
  format version. It identifies an equivalence class of BUILDS: two builds share a
  version if and only if, for every command sequence and every configuration, they
  produce the same trades, events, verdicts and book. A release version used as the
  stamp would refuse journals that replay identically on every upgrade, and the
  response to a check that cries wolf is a permanent override; a format version is
  blind, because all three changes below are byte-identical on disk. §1.2 of the spec
  is the registry, and every future row needs a number, a release and a link to the
  changelog entries that justify it.

  It is written in **both** places, for different jobs. Segment headers are now
  **`OBWAL\x03`, 22 bytes**, with the semantics as a big-endian `uint32` at offset 14
  and the header CRC extended to cover base *and* semantics as one twelve-byte field —
  four bytes per segment, zero per record. `EngineSnapshot` gains an `omitempty`
  `Semantics` field, which is a REPORT and never a gate: restoring a book an older
  build actually had is the documented upgrade procedure, so gating on it would refuse
  the procedure. `Digest()` normalises it away exactly as it does `WALSeq`.

  **`wal.Recover` refuses if and only if it is about to APPLY a record from a segment
  whose declared semantics is not this build's** — new sentinel `wal.ErrSemanticsMismatch`,
  new `wal.RecoverWithOptions` and `wal.RecoverOptions.AcceptSemantics`, new
  `-wal-accept-semantics` on `obgw`. A mismatched segment the snapshot already covers is
  read, CRC-verified, skipped, reported and never refused: it contributes nothing to the
  recovered book, so refusing on it is refusing on a file that could be deleted with no
  effect. **A venue that checkpoints before upgrading therefore starts with no ceremony
  at all**, which is what keeps the check credible — one that fires on the happy path is
  one operators learn to switch off. The refusal falls on a venue that crashed across an
  upgrade and on a replay from an archive. Sealed segments are gated from the directory
  before a byte is read; the newest waits for the walk, because gating it optimistically
  would refuse in the ordinary `SnapshotAhead` case a power loss produces.
  `Open`, `ReadAll`, `Restore` and `RestoreAfter` never refuse, per
  [BOUNDED-RECOVERY.md](docs/BOUNDED-RECOVERY.md) §9.1.

  The override **names the versions it accepts** rather than being a boolean, and that
  is the most important detail in it: `-wal-semantics-mismatch-ok` goes into a unit file
  during one incident and stays for the life of the deployment, so the next mismatch —
  a different mismatch, for a different reason — is accepted silently by a flag nobody
  remembers. `-wal-accept-semantics 1` stops working the moment the number moves again.
  It relaxes the semantics gate and nothing else: `ErrCorrupt`, `ErrLogGap` and the
  retention floor are untouched.

  **A pre-stamp log declares nothing, and nothing is not "compatible".** Treating
  unknown as compatible would turn detection off for the entire installed base, and it
  would be affirmatively FALSE right now, since a pre-stamp log is exactly the one that
  does not have the three changes below. So an unstamped segment is refused when its
  records would be replayed and accepted when they would not, and
  `-wal-accept-semantics 0` is the deliberate override. `obgw` does not checkpoint on a
  clean shutdown, so **the ordinary upgrade path meets this refusal once**; the
  two-line procedure is RUNBOOKS' new "Upgrading across a semantics change". `Open`
  seals a mismatched active segment before appending — migrating a legacy stem into the
  set first, and replacing a record-free segment's header in place rather than rotating
  into its own filename — so the condition is self-healing after one restart instead of
  recurring on every crash until the segment fills.

  **The number is not maintained by discipline.** `internal/semcheck` is
  `internal/apicheck` applied to behaviour instead of to signatures: it drives a fixed
  corpus — the differential and pro-rata tapes at pinned seeds, the recovery profile
  with its uncrosses, a hand-written tier-2 script for stops, OCO, icebergs, pegs,
  trailing stops, busts, marks, band breaches and expiry, and an admission-control
  script — through the engine's PUBLIC API with a deterministic clock, and renders one
  line per command: verdict, trades field by field, published events with payloads,
  state, last trade price, best bid and ask, both id counters and the snapshot digest.
  A body diff with an unchanged version fails and does not offer regeneration first;
  **`SEMCHECK_UPDATE=1` refuses to write unless `matching.SemanticsVersion` is strictly
  greater than the golden's**; and a bump with an identical body fails too, because a
  version that fires when nothing changed is the useless version this design opens by
  rejecting. `matching.eventKindCount` is added in the shape of `entryKindCount` so the
  coverage guard enumerates the enum rather than a list, and `pkg/wal` asserts the
  corpus reaches every `EntryKind`.

  Fourteen sabotages were run against it. Reverting the pro-rata change is caught and
  the diff names the pro-rata scenario's command 13; regenerating after it without a
  bump is refused; bumping with no behaviour change is refused; a literal in place of
  the constant fails; gating the whole set instead of the replay set fails; treating
  unknown as compatible fails on all three pre-stamp shapes; deleting the `Open`-time
  seal fails on the second crash; and putting `Semantics` back into `Digest()` makes the
  no-change bump silently accepted, which is the circularity §2.4 exists to prevent.
  One sabotage found a real gap and it was fixed rather than filed: a stop-trigger
  comparison relaxed from `>=` to `>` was invisible, because no scenario sat a stop
  exactly ON its trigger. Two now do.

  What this deliberately does **not** cover: engine CONFIGURATION. Two builds at the
  same semantics version with different `ProRata`, `SelfTradePrevention`, `MaxOrders`
  or `PriceBand` replay the same log into different books and nothing anywhere notices,
  before or after this change. That is arguably larger than the gap this closes; it is
  a different design and it is named in §6 rather than implied to be handled.

- **A `REJECTED` command's event batch may now carry further events, and a consumer
  must apply them.** `EventSink` consumers — `cmd/obgw`, `pkg/marketdata` and anything
  built on the interface — could reasonably have assumed a rejected command publishes
  exactly one `REJECTED`, because that was true of every command this engine had ever
  processed. It is no longer true: a `REJECTED` may be followed by `CANCELED`,
  `REPLACED`, `ACCEPTED` and `TRIGGERED`.

  The rule is that **a rejection drops only the events describing state the engine
  actually undid**. The only place that undoes anything is `settleInto`'s fill-or-kill
  branch, which now removes its own reversed `EventTrade` entries from the pending
  batch; `emitResult` no longer clears the batch wholesale. A reversed print still
  reaches nobody, which is unchanged and correct.

  What that repairs: a fill-or-kill taker under `CANCEL_OLDEST` or `CANCEL_BOTH`
  removed a resting maker mid-walk, failed to fill, and was rejected — and the book
  lost an order with nothing on the stream to say so, so every consumer rebuilding L3
  from the stream kept a phantom resting order forever. `DECREMENT` did the same, and
  also mutated the taker of an order that ends REJECTED. An OCO stop leg destroyed by a
  stranger's rejected fill-or-kill was equally silent. The maker is **not** restored,
  and that was decided on a measurement rather than a principle: restoring is not
  composable with the other four non-trade mutations the walk makes, one of which
  already leaves an iceberg with a negative `FilledQty`, and it would tell an account
  its `CANCEL_BOTH` had been withdrawn because of an unrelated liquidity condition.

  `EventKind`'s reconstruction claim is narrowed to what is actually proven and its
  citation moves from a hand-written scenario list to a generated-path check:
  `TestDifferentialTape` now rebuilds a book from the event stream alone and compares
  it against the engine's own after **every one of 2,240 commands**, which is the
  assertion that catches this class without anyone predicting it. `internal/wire`'s
  `LeavesQty` justification survives and cites the same check.
  `TestSTPCancelledMakerVanishesWithNoEvent` (inverted, keeping every assertion it
  had), `TestRejectedFOKAnnouncesTheDecrementedMaker`,
  `TestRejectedFOKAnnouncesAStandingCancellation`, and three fill-or-kill × STP
  scenarios added to `TestEventStreamReconstructsBook`.

- **Under `ProRata`, a taker that meets its own resting liquidity is no longer
  skipped: self-trade prevention decides, and the verdict changes.** With the default
  `CANCEL_NEWEST` a taker that used to rest is now cancelled. The five outcomes at a
  level whose remaining liquidity is all the taker's own: `CANCEL_NEWEST` cancels the
  taker and ends the walk; `CANCEL_OLDEST` removes the maker, publishes its `CANCELED`
  and re-allocates what is left at the level; `CANCEL_BOTH` does both and ends the
  walk; `DECREMENT` shrinks both by their overlap, publishes `REPLACED` or `CANCELED`,
  and continues; `ALLOW` includes the order in the allocation and trades with it.

  `matchProRata` never called `takerSTP` at all, so **all five modes** left the taker's
  remainder resting across the spread — bid 100 / ask 99 on a continuous book. A venue
  configured `ALLOW` did not get the self-trade it had asked for and one configured
  `CANCEL_BOTH` cancelled neither order, which means pro-rata was silently overriding
  the venue's self-trade-prevention configuration. It also declined unrelated accounts'
  liquidity on the way: a taker bidding 100 with a stranger offering 5 at 100 printed
  nothing, because the walk ended at the taker's own order at 99 instead of resolving
  it.

  The allocation arithmetic is untouched, and in every case where the taker's own
  liquidity is not needed the fills are byte-for-byte what they were. The rule is
  written as prose in [REFERENCE-MATCHER.md](docs/REFERENCE-MATCHER.md) §2.3, which
  `internal/refmatch` implements — the paragraph is the artefact under review, not the
  loop. On the committed differential sweep the pro-rata profile was in a crossed state
  after 107 of its 700 commands and is now 0; its prints rose 77 → 82 and its
  cancellations 71 → 79. The `crossingIsExpected` narrowing that let that profile run a
  weaker crossed-book assertion is **deleted**, so `checkInvariants` now runs at full
  strength on every profile. `TestProRataSelfSkipCrossesTheBook` (inverted, keeping its
  name), `TestProRata_STPModeDecides`,
  `TestProRata_ReachesUnrelatedLiquidityUnderCancelOldest`,
  `TestProRata_MixedLevelTradesEligibleBeforeSTPFires`.

- **The crash-boundary sweeps now drive a wider alphabet**, from the shared generator:
  reduce, replace, cancel-all, halt and cancel-only at every boundary, none of which
  the old tape could reach. The trade is measured and recorded rather than assumed — on
  the same 400-command tape, continuous prints fell from 202 to 117 and peak resting
  depth from 72 to 45, while auction prints rose from 11 to 16 and the runtime fell
  from 0.77 s to 0.70 s. `TestCrashAtEveryBoundary` now asserts **floors** on all three
  numbers instead of "greater than zero", so the next alphabet change has to argue a
  number down.

  Widening it also revealed the failure mode this project keeps meeting from the other
  direction: with halts and cancel-alls drawable everywhere, commands landing inside a
  pre-open or closing auction refused the very orders the uncross was going to clear,
  and the sweep went to **zero auction prints while every assertion still passed**. The
  generator now draws submits and nothing else inside an accumulating phase.

  **And it was still not the tier-1 alphabet, which adversarial review caught.** The
  widening above is on the command-*kind* axis. On the order-*payload* axis the
  recovery profile set `Exotic: false`, so all 287 submits on the 400-command sweep
  were plain GTC limits — no market orders, IOC, FOK, post-only, per-order STP, trade
  groups or privileged orders — while the profile's own comment called it "deliberately
  a SUPERSET of Differential" and the spec said the tape "now speaks the tier-1
  alphabet". So the replay oracle never carried a rejected FOK's reversed prints or an
  STP-cancelled maker across a crash boundary: the two paths the spec named **in
  advance** as defect-bearing, and the two this slice then confirmed as live defects.
  Every assertion passed throughout, because they count prints, auction prints and
  depth, none of which move if every submit is a plain limit. That is an exhaustive
  check over an incomplete alphabet reporting completeness, in the sweep written to
  apply that lesson.

  Fixed rather than reworded. `walOrder` now carries the whole tier-1 payload onto the
  journal, and the alphabet is asserted **by outcome** — a rejection reason a consumer
  was actually told — wherever an outcome is observable. The depth cost is a real trade
  and is recorded as numbers: at the differential draw rate the sweep fell to 52 prints
  and 25 peak depth, because market orders, IOC and FOK never rest and the three
  cancelling STP modes remove liquidity that already has. `Profile.ExoticDamp` divides
  the exotic draw *rate* — a density knob, never an on/off one, since an on/off one is
  what caused this — and at damp 4 the sweep holds 110 prints, 17 auction prints and
  peak depth 32, clearing every existing floor with no number argued down.

### Fixed — holes in the oracle, found by adversarial review

Three of these are worse than a missing feature, because a hole in an oracle looks
like coverage. Each was confirmed by running the mutation, not by argument, and each
is written up in [REFERENCE-MATCHER.md](docs/REFERENCE-MATCHER.md) §10.5.

- **Snapshot-restore was a digest round-trip, not a restore comparison.**
  `restoreMatchesLive` did `restore(snap).Digest() == snap.Digest()`, which can only
  see state `EngineSnapshot` itself carries and is structurally blind to everything
  `LoadSnapshot` *rebuilds*. `PriceLevel.TotalQty` is exactly that: adding one line to
  `LoadSnapshot` that double-counts each restored order into its level ships a venue
  serving **double** the true L2 depth at every price — live best bid 7, restored best
  bid 14 — and `go test ./... -count=1` exited **0 across all 23 packages**. The spec
  had argued this was impossible by definition. It now compares the restored engine's
  visible book against the model, and the mutation fails at the first command of every
  seed.

- **The compared "event stream" carried no trade payload.** It was an ordered list of
  (kind, order id, user, reason), so the price and quantity a consumer reads off a fill
  were unchecked. Attaching a corrupted trade to every published fill (price + 7,
  quantity doubled) left the whole of `pkg/matching` green; only gateway and marketdata
  tests reading the payload downstream noticed. The harness's own trade comparison
  comes from `MatchResult.Trades`, a different source, so the two could disagree
  indefinitely.

- **The nightly fuzz campaign ran a weaker oracle than the committed sweep.**
  `FuzzDifferential` passed `full=false`, dropping the invariant check, the
  snapshot-restore comparison, the duplicate-order-id check and the trade-quantity
  balance check — the last two being the checks two recorded mutations are credited to
  by name. Thirty minutes of fuzzing could not see what a 0.25-second test saw.

- **An untested property read as covered.** The spec credited the ranked L3 comparison
  with catching "an iceberg refill that lands in the wrong place". It cannot: icebergs
  are tier 2, so the generator never emits one. A mutation re-adding the refilled slice
  at the head of its level passed all 23 packages, with the hidden order taking free
  queue priority. The sentence is corrected and the property now has a test.

- **Smaller, same shape.** `TestSameSeedSameObservations` collected two streams of
  *engine* observations and threw the model's away, so nondeterminism in the model
  would have surfaced as an unexplained intermittent divergence. The STP guard counted
  modes *drawn* rather than modes that actually decided a match. Nothing asserted that
  trade groups or privileged orders were generated at all — the two attributes that
  decide whether self-trade prevention fires across accounts or is bypassed. The
  failure message mixed the shrunk tape's command index with the original tape's
  length, printing "first at command 2 of 140" when the real first divergence was at
  118. `modelOrder` cast the tape's STP byte numerically while the engine side used a
  refusing switch, the one place the harness broke its own no-catch-all rule.

### Known limitation

- **The model's independence is mechanical, not derivational.** No shared code, no
  shared types, no shared arithmetic — enforced by a `go/parser` guard with teeth. But
  the model was written *alongside* `engine.go` rather than from the specification, and
  the evidence is in the files: several comment lines are byte-identical across the two
  implementations, and the decision sequences match. Any rule the engine author got
  wrong and the model author reproduced is invisible — the harness reports agreement.
  That is not hypothetical: the model deliberately canonises the engine's position on
  two of the three findings above, which is why both are pinned by hand-written tests
  instead, and why those tests now name the model edit a fix must make in the same
  commit. Measured, not assumed: correcting either defect in the engine alone turns
  `TestDifferentialTape` red across three profiles and four seeds. Closing this
  properly needs a second model written from the spec by someone who has not read
  `engine.go`. [REFERENCE-MATCHER.md](docs/REFERENCE-MATCHER.md) §2.2.

### Added

- **The write-ahead log rotates into segments, and a prefix of them can be deleted.**
  This is the piece that makes restart cost a number an operator chooses rather than a
  property of how long the venue has been up. Design, the decision that makes it work,
  and every place the code disagreed with its own spec (§12):
  [LOG-ROTATION.md](docs/LOG-ROTATION.md).

  A `-wal` path now names a SET: the stem plus `<stem>.<16 zero-padded digits>`
  siblings, the digits being the first sequence that file holds. Each name is
  cross-checked against an 18-byte header inside the file (`OBWAL\x02`, the base
  sequence, a CRC over it), and a disagreement refuses to start the venue naming both
  numbers — a renamed, copied or restored segment cannot quietly put records into the
  wrong sequence space.

  **The decision the whole slice turns on is that a segment DECLARES its base rather
  than having one inferred from its records.** The covered-prefix skip added in the
  previous entry treats a record's position in the file as its sequence and verifies
  that assumption, falling back to a full re-read when it does not hold. A rotated
  segment starts at position 1 carrying a sequence far above 1, so a naive
  segmentation makes that fallback fire on every rotated log: correct book, two full
  passes instead of one, every restart slower than it was before the skip existed, and
  the only trace one log line. With a declared base the arithmetic is
  `seq = base + ordinal - 1`, which for an unrotated log is character-for-character
  what it was. `TestADeclaredSegmentDoesNotFallBack` asserts `FellBack == false` on a
  properly rotated set; reverting the arithmetic fails it and three others.

  | total history | retention off | with a 4 MiB budget |
  |---|---:|---:|
  | 60,000 records (11 MiB) | 18.1 ms, 10.7 MiB read | **5.95 ms, 3.7 MiB read** |
  | 600,000 records (110 MiB) | 84.1 ms, 106 MiB read | **6.28 ms, 4.1 MiB read** |
  | 6,000,000 records (1.1 GiB) | **2.21 s**, 1,068 MiB read | **5.66 ms, 3.3 MiB read** |

  A hundred times the history, the same restart (`BenchmarkRestartWithRetention`, on
  the churn fixture so the book term is out of it). The "retention off" column is what
  every earlier release did and what a venue that does not set a budget still gets.

  **Deletion is off by default.** `-wal-retain` unset means keep everything — today's
  behaviour with better file names — and the venue says so at startup. Rotation is on
  (`-wal-segment-bytes`, 128 MiB). A segment is deleted only when it is sealed, when a
  snapshot **read back from disk and verified** covers its LAST sequence rather than
  its first, when `-wal-retain-segments` (4) sealed segments remain newer than it, when
  every older one is already gone, and — if `-wal-archive` is set — when a
  byte-identical copy is durable elsewhere. Deletion is oldest-first, so a crash
  part-way through leaves a shorter prefix, which is a valid set.

  **Recovery refuses a set with a hole in it**, and that refusal is the tripwire every
  retention bug trips: `wal: log gap: snapshot covers through sequence 412000 but the
  oldest retained segment ... starts at 610422`. A missing middle segment, an overlap,
  a sealed segment truncated below its recorded span, and a snapshot below the
  retention floor are all refusals naming both sequences, because each one otherwise
  recovers into a plausible book that is missing commands with every remaining record
  verifying perfectly. A WAL path that is a directory is now `ErrNotALog`, where it
  used to recover as a clean empty log and start an empty venue in silence.

  **`Open` no longer appends behind a torn tail.** It seals that segment as it stands
  and starts the next one at `lastSeq + 1`, which closes
  [BOUNDED-RECOVERY.md](docs/BOUNDED-RECOVERY.md) §6.1 — crash mid-write, first restart
  succeeds, second restart refuses — without truncating anything. The fragment stays on
  disk.

  **A full disk is defined behaviour.** It used to be worse than undefined: ENOSPC
  surfaces at the flush inside the 20 ms group commit, whose whole error handling was
  to log and continue, so a full disk gave a venue that kept accepting orders, kept
  acknowledging them, kept matching them and stopped journalling, with `/readyz` still
  green. Now `-wal-min-free` (2 GiB) warns and runs retention immediately,
  `-wal-min-free-stop` (256 MiB) puts every book into cancel-only so participants can
  get flat, and a sync that fails halts the book, latches until a restart and fails
  readiness. New gauges: `orderbook_wal_bytes`, `orderbook_wal_segments`,
  `orderbook_wal_disk_free_bytes`.

  **A rotation costs 12.4 ms on the appending goroutine** (mean, measured on the
  appends that rotated; 21.2 ms worst) — two fsyncs, a `link`, an `unlink` and a
  directory fsync on APFS. At the 128 MiB default that is every four minutes at
  2,500 msg/s. At 1 MiB it is every two seconds and the mean cost per append triples,
  which makes small segments a test fixture rather than a configuration.
  `BenchmarkRotationAppendTail` publishes it rather than calling it negligible.

  **`-wal-retain` is a budget, not a bound.** `-wal-retain-segments` is checked after
  it and wins, so the retained set never falls below
  `(-wal-retain-segments + 1) x -wal-segment-bytes` — **640 MiB at the shipped defaults
  of 4 and 128 MiB**, whatever byte number is set. The sizing advice was published as
  though the floor were not there; a 500 MiB budget against the defaults yields 640 MiB
  and about 1.3 s, not 1.0 s. The arithmetic is now stated wherever the advice is, and
  `cmd/obgw` logs which term stopped a retention cycle when it changes, so an operator
  watching a disk fill under a configured retention learns why nothing is being
  deleted. `TestTheByteBudgetIsFlooredByMinSegments` pins it.

  **`wal.Open` is not an integrity check, and Rule 9 used to claim it was.** It reads
  only the newest segment, so a set with a missing middle segment, an overlap or a
  CRC-damaged sealed segment opens cleanly and is refused by `Recover` and `ReadAll`.
  Contiguity needs the previous segment's last record sequence, which only comes from
  reading its records — so the claim was never implementable, and it was asserted as
  fact in a godoc rather than argued for. Corrected in the rule, the godoc, and
  `examples/replication`, which opens without recovering. `cmd/obgw` is unaffected: it
  calls `Recover` first on the same path.

  Four defects an adversarial review of the finished code found, all silent, none
  reachable by any test that existed (§12.12, §12.13):
  **rotating a pre-checksum v1 log** framed the rotation-triggering record without a
  CRC and wrote it into a segment declaring every record checksummed, leaving the set
  permanently unreadable from that point while `Open` kept succeeding;
  **an existing zero-byte file at the `-wal` path** took CRC-framed records at offset 0
  behind no magic, which every reader then classified as a headerless v1 log —
  `ReadAll` returning nothing with a nil error, and each restart rewriting sequence 1;
  **archiving into the log's own directory** reported every segment archived and
  deleted every one of them, because the archive target was the segment and the
  idempotency check agreed it was already there; and **a set whose stem was missing**
  — the one downgrade shape §2.5 exists to prevent, reachable by a kill or an ENOSPC
  inside the first rotation's migration — was never repaired, surviving 46 further
  rotations. Also: the active segment's file descriptor kept the `.tmp` name it was
  built through, so every ENOSPC on a live segment named a file that does not exist;
  retention was skipped on a full disk, which is the only time it is the last mechanism
  left; and the low-water warning said "running retention now" against the default
  configuration, in which retention deletes nothing.

  New API: `wal.OpenWith`, `wal.Options`, `wal.Retain`, `wal.Stat`, `wal.ReadAfter`,
  `wal.ArchivedSegments`, `wal.FreeBytes`, `wal.CheckArchiveDir`, `wal.ErrLogGap`,
  `wal.ErrNotALog`, `wal.ErrBelowFloor`, `wal.ErrArchiveIsTheLog`, `Writer.Failed`,
  `Writer.Rotations`, `Writer.ActiveSegment`,
  and three fields on `wal.RecoverReport`. `wal.Open`, `wal.ReadAll`, `wal.Recover`
  and `wal.RestoreAfter` keep their signatures and their behaviour on a log that has
  never rotated. The frozen API surface is updated accordingly.

### Fixed

- **A trading-phase transition was applied and never written to the command log, so a
  venue that ran an opening or closing auction and crashed before its next checkpoint
  came back in the wrong phase with an un-uncrossed book.** `Runner.SetPhase` reached
  `logCommand`'s `default` branch — the branch whose comment claimed to hold only
  read-only commands — for three releases. Design, blast radius and the ten sabotage
  runs that had to fail before it counted:
  [JOURNAL-COMPLETENESS.md](docs/JOURNAL-COMPLETENESS.md).

  **The damage did not stop at the enum, which is why this is a Fixed and not a
  footnote.** A lost `SetPhase(StatePreOpen)` makes replay MATCH orders the live venue
  rested, so the recovered tape holds trades that never happened and — because trade
  ids come from a monotonic counter — every later id names a different trade than it
  did before the crash. A lost `SetPhase(StateOpen)` skips the uncross, so the venue
  reopens onto a bid resting above an ask and the auction's prints are missing from a
  tape subscribers already received. And because the price collar falls back to
  `LastTradePrice`, a skipped uncross leaves every later admission decision measured
  against a stale reference: an order the live venue **accepted and acknowledged** is
  refused on replay and is absent from the recovered book.

  `wal.KindSetPhase` is appended to the end of the `EntryKind` block, never inserted,
  so no existing record is renumbered. The phase travels as a **name** (`"PRE_OPEN"`)
  rather than an ordinal: a snapshot is rewritten every checkpoint but a log segment
  may be archived for years, so reordering the `EngineState` block — a change nobody
  would think of as a format change — would otherwise silently reinterpret every
  archived phase record, and an unknown ordinal decodes as a valid-looking state
  nobody defined where an unknown name can be refused. Replay **re-runs the uncross**
  rather than restoring the state field, because restoring the field alone leaves the
  crossed book unresolved and the prints missing. `Entry.Phase` is `omitempty`, so
  every pre-existing record encodes byte-identically and old logs replay exactly as
  before.

  **The third escape of this exact kind**, after `Reduce` (an order came back at its
  original size) and `Halt` (a deliberately halted venue came back Open). So the other
  half of the work is a guard over the SHAPE: every `cmdKind` must now be classified
  either as journalled — naming the `CommandLog` method it must reach — or read-only
  with a written reason, and a kind classified read-only must leave
  `EngineSnapshot.Digest()` untouched when driven through a `Runner`. A prose reason is
  no longer enough to hide a mutating command, which is precisely how all three
  escaped. A second guard enumerates `wal.EntryKind` and requires every kind to have a
  replay arm, because journalled is not the same as replayed.

  **Why no test caught it, which mattered more than the bug.**
  `TestCrashAtEveryBoundary` crashes at all 401 boundaries of a 400-command tape and
  compares a book digest *and* the trade tape at every one — and its tape contained
  only limit orders and cancels, so the most exhaustive property in the project was
  proven over an alphabet that excluded the one command that escaped the journal. An
  exhaustive check over an incomplete alphabet reports completeness. The tape now
  contains phase transitions, and both sweeps fail outright if the auction ever
  disappears from it again.

- **A checkpoint taken after a restart and before the first command stamped the
  snapshot with log sequence 0, over a complete book.** `matching.NewRunnerFor` builds
  a Runner over a recovered engine and had no way to be told where in the log that
  engine already stood, so `lastApplied` — the number every checkpoint writes into the
  snapshot as `WALSeq`, and the number the NEXT recovery replays from — started at zero
  however the engine came to exist. `matching.RunnerConfig.LastApplied` now carries the
  position, and `cmd/obgw` seeds it from the recovery report with
  `max(SnapshotSeq, LogLastSeq)`.

  The window is one checkpoint tick with no command in it, which at the shipped
  30-second `-checkpoint` cadence is any restart into a quiet market: out of hours,
  before the open, a maintenance window, a venue restarted and watched for a minute
  before the flow is pointed back at it.

  What it cost depends on the configuration, and neither answer is good. Without
  `-wal-retain` it is **silent**: the next recovery re-applies the whole log on top of
  a snapshot that already contains it, and the only thing standing between that and a
  doubled book is the duplicate-client-order-id ring — which is bounded (4,096 keys in
  `cmd/obgw`, and zero by default in `pkg/matching`) and, more to the point, does not
  cover an order that carries no client order id at all. The wire accepts one. Fifty
  such orders in over TCP, a restart, one quiet checkpoint, another restart, and the
  book holds a hundred. With `-wal-retain` set it is **fatal**: the retention floor climbs past the
  sequences the zeroed stamp claims not to cover, and the venue refuses every
  subsequent start with `wal: log gap: snapshot covers through sequence 0 but the
  oldest retained segment ... starts at 385`, naming segments that have already been
  deleted, with `-wal-archive` off unless set.

  The defect predates segmentation and retention and reproduces at `ffd9a96`. The
  retention floor check is what made it visible rather than what caused it.
  `RUNBOOKS.md` gains "A snapshot stamped sequence 0", and the first step of "A gap
  between the snapshot and the log" now sends an operator there instead of to a
  fail-over. Three tests pin it, the two in `cmd/obgw` end to end through a real
  restart: the stamp, the count of records the following recovery re-applies, and that
  a venue running retention still starts after a quiet checkpoint tick.

### Changed

- **BREAKING: `matching.CommandLog` gained a sixteenth method,
  `AppendSetPhase(phase EngineState) (int64, error)`.** Every implementer outside this
  repository stops compiling until it is added.
  [COMPATIBILITY.md](docs/COMPATIBILITY.md) names this exact case — "`CommandLog`
  gained five methods in v0.21.0 and this is the case that taught the lesson" — so the
  price was already written down and is being paid rather than avoided. Four
  implementers inside this repository needed the method: the gateway's `syncingLog`
  and three test fakes.

  **The narrow optional interface was considered and rejected, and the reasoning is
  the point.** A `PhaseLog` that `logCommand` type-asserts would break nobody — and
  would mean a `CommandLog` that does not implement it **silently drops phase
  records**, which is the precise failure the fix above exists to eliminate,
  reintroduced as the mechanism of its own fix. Durability an implementer can decline
  by omission is not a guarantee, it is a default. Where the compatibility promise and
  the durability promise collide, the durability promise wins and the compatibility
  promise's job is to make the collision visible.

  Also added, all non-breaking: `matching.ParseEngineState` and
  `matching.ErrUnknownEngineState` (`String`'s inverse, for decoding a phase name off
  the log — an unknown name is an error, never a fallback to `StateOpen`),
  `wal.KindSetPhase`, `wal.Entry.Phase` and `wal.(*Writer).AppendSetPhase`. The frozen
  API surface is updated accordingly: six additions, no removals.

- **A halted or cancel-only venue now refuses new orders with `ReasonHalted`
  (wire 10), where it used to say `ReasonOther` (wire 1).** `orderentry.ReasonFor` had
  no case for `types.ErrTradingHalted` or `types.ErrNewOrdersHalted`, so a reason code
  that was defined, documented, frozen in the wire numbering and handled by
  `cmd/obsoak` was never actually sent by anything. Found while asserting the
  disk-full behaviour above, whose design declines to invent a `ReasonDiskFull`
  precisely on the argument that clients already receive `ReasonHalted` — an argument
  worth nothing while it was false. No wire constant changed and no test asserted the
  old value. Every other mapping is unchanged, including `matching.ErrShuttingDown` to
  `ReasonShuttingDown` (wire 10 was added beside wire 15, not over it), and the whole
  mapping is now pinned by a table with one row per sentinel — the failure mode here is
  a case going missing, which a test per case cannot catch.

- **`ReadAll` returns what is still on disk.** Its signature, its ordering and its
  errors are unchanged, and it is unchanged in every respect for a log that has never
  had a segment deleted. Once retention has fired, the first entry's `Seq` is the
  retention floor rather than 1 — the runbook and the godoc now say so, and the floor
  is legible from `ls` without tooling.

- **`RUNBOOKS.md` §"A corrupt snapshot" now branches on the retention floor.** The old
  procedure — delete the snapshot, restart, replay from the beginning — is still
  exactly right for a venue whose floor is 1, which is every venue running the default
  configuration. Above 1 it destroys the book, because the snapshot is the only base
  the retained log can be joined to. A venue running retention without archival has a
  recovery point objective equal to its newest snapshot, and that sentence is now in
  `PRODUCTION-READINESS.md` rather than waiting to be discovered in an incident. Two
  new sections: "A gap between the snapshot and the log" and "The disk filled up".

- **A restart no longer parses the part of the log its snapshot already covers.**
  `wal.Recover` walks every record in the file and verifies every CRC, and decodes and
  retains only the records past the snapshot's sequence. Measured on
  `BenchmarkRecoverBehindACoveredChurnPrefix`, which grows the log while keeping the
  recovered book near empty so the log term is on its own — 1,000 records to apply in
  every row, Apple M4, medians of 5:

  | covered prefix | log on disk | before | after | allocated before | after |
  |---:|---:|---:|---:|---:|---:|
  | 50,000 | 8.93 MiB | 161 ms | **11 ms** | 70.6 MiB | **2.0 MiB** |
  | 200,000 | 35.4 MiB | 639 ms | **37 ms** | 277 MiB | **2.0 MiB** |
  | 500,000 | 88.4 MiB | 1.66 s | **64 ms** | 772 MiB | **2.0 MiB** |

  **Allocation is now flat in the covered prefix** — that column does not move between
  50,000 records and 500,000. Time fell ~26×, which is more than the design predicted:
  it planned from a `BENCHMARKS.md` row labelled "`ReadAll` — read + CRC-verify the
  log", and `ReadAll` decodes every record too. Between the 50,000- and
  500,000-record rows the marginal cost of a covered record went from ~3.33 µs to
  ~106 ns, so the decode was ~97% of it. That label is corrected. The saving is
  proportional and not a large-log effect: a 1,000-record covered prefix in front of a
  1,000-record tail skips half the file and goes 6.2 ms → 3.4 ms.

  **Every byte is still read and every checksum is still checked**, wherever in the log
  the damage is. Seeking past the covered prefix would be faster still and would stop
  detecting bit rot behind the snapshot — permanently, since each checkpoint buries it
  deeper — and no test in the suite would have caught the change, because none of them
  corrupted a record behind a snapshot. `TestCorruptionInTheCoveredPrefixStillRefuses`
  now does, and it fails against a seek-based skip.

  **Restart time is reduced, not bounded, by this change on its own.** The file it
  measures never shrinks (~44 GiB a day at 2,500 msg/s) and reading it is O(total log).
  Rotation and retention are what remove that, and they are the entry above — in the
  same release, and still off by default. Design, sabotage runs and the three places
  the code disagreed with its own spec:
  [BOUNDED-RECOVERY.md](docs/BOUNDED-RECOVERY.md).

  `wal.Open` was paying for a *second* full parse of the log, inside `lastSeq`, to
  learn one number. It now walks the frames and decodes one record, and still refuses a
  log with a damaged frame or a failed checksum. `ReadAll`'s behaviour, signature and
  errors are unchanged — it is the same walk with the boundary at zero — and it stopped
  allocating a fresh buffer per record on the way to `json.Unmarshal`. That is worth
  three allocations and ~328 bytes per record on the paths with no skip in them at all:
  `ReadAll` of 100,000 records goes 175.2 → 142.4 MB, and a snapshotless `Recover` of
  the same log (`BenchmarkReplayTail`) 235.0 → 202.2 MB. Useful, and no more than that
  — a first draft of this entry credited buffer reuse with "457 MiB to 107 MiB", which
  is a different fixture (`BenchmarkRecoverBehindACoveredPrefix/covered200000`, where
  the reduction is the covered-prefix skip) in different units.

  **One thing a venue used to refuse now starts.** A record that is complete on disk,
  passes its CRC and is not valid JSON — a writer bug or a format mismatch, never bit
  rot — is only detected where recovery decodes it, so it is refused at and past the
  snapshot's boundary and walked past strictly behind it. The recovered book is
  identical either way, because a covered record is dropped for its sequence whether or
  not it decoded; `ReadAll` still decodes every record and still reports it. Found by
  differential fuzzing after the change was written, documented in
  [BOUNDED-RECOVERY.md](docs/BOUNDED-RECOVERY.md) §5.2 rather than reverted, and pinned
  by `TestUndecodableRecordBehindTheSnapshotStartsTheVenue`. `wal.Open` is deliberately
  the most permissive reader of the three: recovery's strictness moves with the snapshot
  boundary and `Open` does not know where the boundary is, so an `Open` stricter than the
  laxest `Recover` could fail on a log that had just recovered successfully.

  API: adds `wal.RecoverWithReport` and `wal.RecoverReport`, which name two conditions
  that leave the recovered book correct and were previously silent — a log whose record
  sequences are not their file ordinals (recovery re-reads the file whole rather than
  guessing), and a snapshot stamped ahead of its log (reachable after an ordinary
  crash, because a checkpoint does not sync the log first). `obgw` logs both. `Recover`
  keeps its exact signature.

  `TestRestartCostTracksTheWholeLogNotTheTail` asked in its own failure message to be
  inverted when this landed; it is now `TestRestartCostsNoAllocationForTheCoveredPrefix`
  and asserts the ratio stays under 1.5× for a 4× larger log. It asserts allocation and
  not time on purpose: allocation goes flat, time does not. It uses a churn fixture to
  isolate the log term, so `TestARealRestartAllocatesForItsBookAndNotForItsLog` covers
  the restart whose book grows with its log — 355 bytes of allocation per additional
  covered record with the skip, 2,105 without. `TestV1UndecodableRecordBehindTheSnapshotStillStops`
  pins the one predicate that keeps headerless logs decoding every record, which the
  clean-log v1 test does not.

### Added

- **A restart-cost benchmark that can actually see the problem.**
  `BenchmarkRecoverBehindACoveredPrefix` holds the work constant at 1,000 records to
  apply and grows the already-snapshotted prefix behind it: **8 ms / 253 ms / 664 ms**
  and 5 MB / 141 MB / 457 MB allocated for 1k / 50k / 200k. `BenchmarkRecoverSnapshotPlusTail`
  cannot show this — it builds a log that is *only* the tail, so the prefix it exists
  to skip is never present. A test pins the shape so it cannot silently worsen, and
  its failure message says how to invert it once `Recover` learns to skip. *(It has
  since been inverted; see the Changed entry above. A second fixture,
  `BenchmarkRecoverBehindACoveredChurnPrefix`, was added alongside it because this one
  grows the recovered book with the log and so measures two terms at once.)*

- **A nightly soak on a machine that does not sleep**
  (`.github/workflows/soak.yml`). Two of the first three four-hour runs on a laptop
  were lost, one to the process supervising it and one to macOS sleeping for 172 of
  302 elapsed minutes. It asserts **only** structural findings — orphans, errors,
  goroutine and descriptor trends — and publishes no timings, because a shared runner
  cannot produce a throughput figure worth comparing. Verified against the real
  four-hour report (passes) and against synthetic reports with orphans and goroutine
  drift (fail).

- **[RUNBOOKS.md](docs/RUNBOOKS.md) — "Debugging a live venue."** Seven read-only
  steps for when something is wrong and you do not yet know what: is it the venue or
  you, what the book thinks it is, what it is refusing and whose fault that is, who
  is being dropped, whether it is growing, what it is actually holding, and what the
  log says. Every step is safe on a live venue, and the one endpoint that is not is
  flagged.

- **A disclaimer** at the top of the README and in
  [PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md): this is an experiment, not
  a product. No independent review, no production deployment, an API and a wire
  protocol that both broke inside a single week, and at least one known defect
  documented rather than fixed. If running it costs you something, that is yours.

- **A way into the site that is not the top of a long page.** The home page and
  `web/docs.html` open with four paths — learn it, watch it run, embed it, operate it —
  because the previous first screen asked a visitor to read everything to find the one
  thing they came for. The console gets a four-step first run, the tutorial says in one
  line how it works, and a seed in the console URL (`?seed=42`) opens the same market
  for whoever you send it to: the determinism the page claims, now shareable. Tutorial
  chapters are deep-linkable (`learn.html#chapter-4`) and the chapter rail is clickable
  rather than decorative.

  The quickstart on both pages now matches [examples/basic](examples/basic/main.go):
  it builds an `Instrument` and passes decimals, which is what a caller writes. The old
  snippet passed raw ticks and lots to `types.NewOrder` — still a valid call, and still
  the wrong first thing to show someone.

### Fixed

- **"Snapshots bound restart time" was wrong in seven places.** A snapshot bounds
  what a restart *applies*; `wal.Recover` reads and parses the entire log regardless,
  so restart cost scales with total log size and not with the tail. Measured: 4 ms at
  1,000 records, 428 ms at 100,000, **1.47 s at 500,000 — with nothing to apply in
  any of them** — and 611 MiB of allocation for 87.7 MiB of log.

  Combined with a log that nothing rotates or truncates (~220 bytes per client
  message, 44 GiB a day at 2,500/s), **a venue left running becomes slower and
  hungrier to restart every day it stays up, and finds out at the worst moment.**
  One day of continuous operation is roughly 59M records: about three minutes of
  reading and on the order of 100 GB of allocation to come back up.

  Corrected in `wal.Recover`'s doc comment, `obgw`'s config field and `-snapshot`
  flag, `checkpointLoop`, PROTOCOL.md, INTEGRATION.md and the README. Both halves are
  fixable and neither is done — the record ordinal is the sequence, so a covered
  prefix could be skipped without parsing, and a log could be archived once a
  snapshot covering it is durable. `BenchmarkRecoverSnapshotPlusTail` cannot see the
  problem: it builds a log that is only the tail, so the prefix it exists to skip is
  never present.

  *The first half has since been done — see the Changed entry at the top of this
  release. The figures in this entry are what was measured before it. The second
  half, rotation, is still not done.*

### Added

- **A four-hour soak across three books** ([SOAK.md](docs/SOAK.md) §1e): 14,400,199
  messages at 1,000/s, durable, zero errors, and **goroutines, descriptors and the
  book's own size flat across 240 samples**. No orphans. The longest run this project
  has done, and the first on the multi-symbol path.

  The heap floor rose 26.7%, which the harness calls growth. Across three runs of the
  same workload the trend reads +50.9, +6.0, **+4.3 MiB/hour** — an order of
  magnitude of decay, which is caches filling rather than a leak. Left idle
  afterwards the heap fell to 59.6 MiB against a 69.8 MiB end-of-run floor. What
  four hours could *not* settle is stated in the doc: the machine's own probe read
  307% apart between a busy evening and an idle night, and GC pacing follows CPU.

- **`obgw -pprof`** mounts `net/http/pprof` on the admin listener, off by default. A
  34-minute soak reported the heap floor up 28.8% with goroutines and descriptors
  flat, and there was no way to ask the process what it was holding — metrics can
  count things, not name what retains them. Off by default because a heap dump is a
  snapshot of everything the venue holds including account identifiers, and
  `/debug/pprof/profile` costs 30 seconds of CPU; on the admin listener because that
  is already the operator's side of the venue.

- **The exported surface of `pkg/...` is frozen**
  ([COMPATIBILITY.md](docs/COMPATIBILITY.md)). `internal/apicheck` renders all 1,113
  exported declarations — names, signatures, struct fields, interface methods — to a
  golden file, and a test fails when any of it moves. Same device `internal/wire`
  uses for the protocol, for the same reason.

  It does not prevent a breaking change and does not try. It makes one impossible to
  ship without a human reading a diff headed *"REMOVED or CHANGED — this breaks code
  that compiles today"*. Verified against both shapes: adding a method to an exported
  interface (which reads as an addition and breaks every outside implementer) and
  deleting an exported method.

  **From v0.26.0, a breaking change to a covered package requires a minor bump, a
  changelog entry naming what breaks, and a regenerated `surface.txt` in the same
  commit.** Pre-1.0 licence to break anything at any time is hereby declined — it is
  what produced two breaking wire bumps and four other breaks in two days. 1.0 is
  explicitly not claimed yet, and the reason is in the doc.


- **`obgw -symbols` and `-datadir`; `obsoak -symbols`.** The gateway grew multi-symbol
  config fields in v0.22.0 and neither reached a command-line flag, so the binary
  could not be told to serve two books. The soak harness now picks an instrument per
  *order* rather than per connection, so one session holds live orders on every book
  while every cancel it sends names only a `ClOrdID`.

- **The multi-symbol path has been under sustained load** ([SOAK.md](docs/SOAK.md)
  §1d): 5m29s at 1,200 msg/s across three books, durable. Goroutines and descriptors
  did not move by one; heap floor up 3.6 MiB; no errors.

  Measured against a control, because the interesting question was not "does it
  survive" but "does the cancel routing miss". Multi-symbol added three pieces of
  per-order state with eviction, which is the shape that passes tests and fails under
  load. Paired against an identical single-book run the refusal rate is **8.347% vs
  8.405%**, all of it the harness's own optimistic bookkeeping — and a cancel routed
  to the wrong book would land in exactly that counter.

### Fixed

- **The soak harness's orphan detector got less sensitive under load** — the one
  condition it exists to watch. Driving the venue to saturation returned a
  believed-resting count of 2,180 against a hard ceiling of 800, which the client's
  own bookkeeping cannot produce.

  A refused command was put back on the resting list whether it was a cancel or an
  enter. A refused cancel leaves its order resting and belongs back; a refused enter
  never rested, and `act` had already added it optimistically, so putting it back
  files the same id twice. Below saturation almost nothing is refused and the drift
  is invisible; at 42% rejection it compounds every second.

  That count is the orphan check's baseline, so inflating it disables the check
  twice: it shrinks the venue-minus-believed gap and raises the `believed/10`
  threshold the gap is tested against. Orphaned orders are the one serious defect
  this project has actually had. Nothing was wrong with the engine — the instrument
  was wrong, in the direction that hides failures rather than inventing them.

- The gateway's startup line logged one instrument while serving three, because it
  printed the single-symbol config field. Cosmetic, and the sort of thing that makes
  an operator distrust the rest of the log.

## [0.25.0] - 2026-08-11

A documentation release, and every item in it is this project checking its own
claims rather than adding new ones. Two published allocation ratios were stale, one
of them flattering by 2×; a test count corrected yesterday was stale again today;
and the rule that catches all of this finally got written down instead of being
folklore in a test-file header.

### Fixed

- **The published test count goes stale by construction, so it is now a floor.** It
  read 480 for several releases after it stopped being true, was corrected to an
  exact 584 — and was stale again within a day. It now reads "over 600" with the
  command to count them, for the same reason v0.19.0 deleted the hardcoded "latest
  version" from the docs page rather than updating it. A floor can only ever become
  an understatement.

- **Two stale allocation ratios in [BENCHMARKS.md](docs/BENCHMARKS.md).** `Add` into
  a growing book was published as 1.05 allocs/op against a measured **2.01** — the
  page understated the cost of growing a book by half, which is the third correction
  this file has taken and the third in the flattering direction. Cancel + replace
  read 0.009 against a measured **0.0000**, understating the engine instead.

  Neither is a regression: both reproduce identically on pre-session code. Both are
  deterministic allocation counts rather than timings, and both are printed by
  `pkg/orderbook/alloc_test.go` on every run — the figures were in the test log the
  whole time and nobody re-read them.

- `Config.ShardIndex` moved to the end of the struct. Appending cannot shift the
  offsets of fields the match path reads; inserting can.

### Added

- **[TESTING.md](docs/TESTING.md)** — the rule the rest of the documentation rests
  on, written down: *a test does not count until it has been run against code
  deliberately broken in the way it claims to detect.* It was already the standing
  rule for the replication drills, whose file header names the sabotage each was
  verified against; this generalises it, because the same mistake has now been made
  six times across five tests, in code written carefully by people who knew about
  the rule.

  The case studies are the point, and they are all from this repository: a digest
  test satisfied by a sequence counter, a checksum test satisfied by a magic number,
  a timing test that first passed against a short-circuit and then failed against
  correct code, a drill that blamed the wrong follower, and a test double more
  permissive than the venue it stood in for. Each read correctly and would have
  passed review. Linked from CONTRIBUTING's quality bar, where it changes what a
  contributor is asked to have *done* rather than merely believed.

## [0.24.0] - 2026-08-11

Two things the previous release named as unfinished, and one it did not know about.
The gauge work was the stated item; making it exposed that `cmd/obdash` had been
unable to connect to the gateway since the wire went to v4, and that every one of
its tests passed anyway because its own test double never checked the field the real
venue refuses on. That is the more useful of the two findings, and it is the one
that was not on the list.

### Fixed

- **A constant-time-auth test that flaked, and would have taught people to ignore
  it.** `TestAnUnknownAccountDoesTheSameWorkAsAWrongSecret` summed wall clock across
  rounds, so it measured the work *plus* however long the scheduler kept the
  goroutine off a core — and the second term is unbounded. Running the package
  alongside the rest of the repo pushed the ratio to 4.19 against a threshold of 4,
  twice, on code with no short-circuit in it. It now takes the **floor** of five
  rounds: noise only ever adds time, so the minimum estimates the real cost while a
  sum estimates the real cost plus the worst interference either arm happened to
  meet. Against a genuine short-circuit the ratio is now ~8000 rather than barely
  over 4. [SOAK.md](docs/SOAK.md) reached the same conclusion about heap growth for
  the same reason: watch the floor, not the trend.

- **`cmd/obdash` could not connect to a wire-v4 gateway.** It sent no `Symbol` on
  its market-data subscribe, so the venue would have refused it outright — and its
  own test double never validated the field, which is why every test passed. The
  double is now as strict as the real venue: **a test double more permissive than
  the thing it stands in for certifies the wrong system.**

### Changed

- **Price gauges are a labelled family, one series per book.**
  `observability.Collector.GaugeFamily` registers a gauge that has one reading per
  label set, with HELP and TYPE written once; `orderbook_best_bid`,
  `_best_ask`, `_spread`, `_last_trade_price` and `_phase` now carry `symbol="…"`.
  Countable gauges (queue depth, resting orders) stay bare and sum across books,
  because a venue's queue depth is its queue depth — but a last trade price across
  two instruments is not a number.

  **They carry the label at a one-instrument venue too**, which is a breaking change
  for anything scraping the bare names. A metric whose label set depends on how the
  venue happens to be configured is one no dashboard can be written against, since
  series would appear and disappear as instruments were added. `cmd/obdash` selects
  the series for its `-symbol`.

## [0.23.0] - 2026-08-10

The reference gateway catches up with the core. `cmd/obgw` was the last thing
standing between the multi-symbol design and a venue anyone can run, and converting
it found the bug that refactor was most likely to hide: `buildOrder` validated the
incoming symbol and then stamped the *configured* one onto the order, so at a
two-book venue every order silently landed on the first book. A refactor that
compiles is not a refactor that works.

### Added

- **`cmd/obgw` serves a set of instruments.** `Config.Symbols` and `Config.DataDir`;
  one book per symbol — matching goroutine, command log, market-data feed, rate gate
  — under a venue-wide account layer: one `Registry`, one publisher, one stream per
  account. A client holds one session and sees one ordered conversation whatever mix
  it trades, which is what makes a client id enough to name an order without also
  naming a symbol.

  **A one-book venue is unchanged**: same config fields, same `WALPath` and
  `SnapshotPath`, same small dense ids, and the entire existing test suite passes
  untouched.

- Mass cancel and `Query` fan across every book and aggregate — an account's orders
  are its orders. Readiness takes the **worst** book, since a venue with one wedged
  matcher is not ready however healthy the others are.

### Fixed

- **Routing a cancel needed the session to remember, not the registry to be asked.**
  `wire.Cancel` names an order by `ClOrdID` alone, so the gateway must pick a book
  without being told which. Resolving the engine order id up front to read its shard
  field is the obvious move and it is wrong: the naming index is written by the
  *matching* goroutine, so a cancel arriving while its own Enter is still queued
  resolves to nothing and is refused for an order that is about to exist — the
  orphaned-order defect [SOAK.md](docs/SOAK.md) measured at 12,843 orders in thirty
  seconds. The session already knows: it read the Enter, and the Enter carried the
  symbol.

### Known limits

- **Price gauges do not aggregate across books** and report the first one. A last
  trade price averaged over two instruments is not a number; the right answer is one
  series per symbol, which needs label support `pkg/observability` does not have.

## [0.22.0] - 2026-08-10

The multi-symbol release, and the third in a row where writing the spec first found
a defect that predated the feature. `PRODUCTION-READINESS.md` called a multi-symbol
venue "a routing layer you write." Checking the code instead of the sentence:
`ShardsConfig` had no way to supply a `CommandLog`, and durability, recovery and
replication all hang off it — so **a sharded venue could not survive a restart at
all.** That is not a routing layer, it is most of a venue.

The design decision everything else follows from is a refusal: there is no order of
events across symbols, because a venue-wide sequence needs a serialisation point
every command passes through, which is the bottleneck sharding exists to remove.
Each symbol is its own timeline; what that costs is listed in the spec rather than
discovered later. Ids are the one thing shared across books, and they are
*partitioned* rather than centralised precisely so that sharing costs no
coordination — a shared counter would have made a shard's ids depend on how its
traffic interleaved with every other shard's, so replaying one log alone would no
longer reproduce its own ids.

`cmd/obgw` still serves one instrument, and is now the only thing between this
design and a multi-symbol venue anyone can run.

### Added

- **Multi-symbol identity** ([MULTI-SYMBOL.md](docs/MULTI-SYMBOL.md), deliverables
  all six). Order and trade ids partition into a 15-bit shard index and a 48-bit
  per-shard counter, so an `int64` names an order at a venue with many books. A
  shared counter would have been simpler and wrong: it makes a shard's ids depend on
  how its traffic interleaved with every other shard's, so replaying one log alone
  would no longer reproduce its own ids — trading the property this project is most
  confident about for one it merely wants. Shard 0 composes to the sequence itself,
  so every single-symbol deployment keeps the ids, snapshots, logs and golden vectors
  it already had.

  `matching.Manifest` is the price: a durable, CRC-checked `symbol -> index` mapping,
  refused rather than repaired. Losing it is worse than losing a snapshot, which
  costs only a replay.

- **`ShardsConfig.NewLog`** — the finding that turned a one-line gap into a spec.
  `ShardsConfig` had no way to supply a `CommandLog`, and durability, recovery and
  replication all hang off it, so **a sharded venue could not survive a restart at
  all.** `PRODUCTION-READINESS.md` called multi-symbol "a routing layer you write";
  it was most of a venue. One log per shard, so recovery and the replication drills
  are the existing single-symbol code paths run N times.

- **Venue-wide `ClOrdID` admission** (`Registry.IsLiveClOrdID`). The naming index is
  keyed by account and client id with no symbol, so at two instruments a repeat
  overwrites the first and the account's next cancel retargets.

- **`examples/multisymbol`** — a two-book reference venue with a feed and a log per
  shard, serving both books over sockets. It exists for the reason
  `examples/replication` does: the multi-symbol seams are each plausible on the page,
  and this repository's record with seams claimed but never consumed is documented
  and bad. Replication drill **D8** covers a two-symbol venue, with a negative
  control — a follower on the wrong shard index rebuilds the same orders under
  different numbers, and the digest catches it.

  `cmd/obgw` is deliberately **not** converted: it still serves one instrument, and
  it is now the only thing between this design and a multi-symbol venue anyone can
  run. Converting it is one runner, one feed, one gate, one recovery path, sixteen
  call sites and fifteen test files — its own arc, not a rider on a protocol change.

### Changed

- **BREAKING: wire protocol v3 → v4.** `MDSubscribe` gains `Symbol`. It named an
  incarnation and a sequence but no instrument, so a market-data connection could
  only ever mean "the one book this venue serves". A subscription now selects
  exactly one symbol and every message on that connection belongs to it, so **no
  other market-data payload changed** — the regenerated golden vectors differ only
  in their version byte. A subscription for an unserved instrument is refused with
  `MDRejectUnknownSymbol` rather than quietly given the wrong book, which a
  subscriber cannot detect for itself.

- **BREAKING: `Shards` refuses a second symbol without a `Manifest`.** It previously
  gave every shard index 0 and served colliding ids silently — a failure whose only
  symptom is two orders nobody can tell apart, discovered much later. Anyone running
  `Shards` multi-symbol today was doing this unknowingly.

- **`Engine.Bust` validates the shard field**, so busting another symbol's trade is
  `ErrUnknownTrade` instead of annulling whichever local print shared the low bits.

### Fixed

- **Replication drill D6 blamed the wrong follower, about one run in twelve.** The
  drill drove traffic until `Shed() != 0` and assumed the follower cut was the
  wedged one. A follower that actually *applies* commands is slower than a wedged
  socket — which merely fills a kernel buffer and costs the primary nothing until it
  is full — so driven flat out, the healthy follower's own ship buffer overflowed
  first and it was the one shed. The drill then reported "shedding the wedge broke
  the healthy follower", the opposite of what had happened.

  `Primary.ShedPeers` now attributes each cut to a peer address, which is the part
  worth having beyond the test: a bare drop counter cannot tell an operator whether
  a client stopped reading or a follower is merely running behind, and those need
  opposite responses. D6 waits for the wedge specifically, asserts no other follower
  was cut, and paces the tape against the healthy follower so there is one candidate
  rather than two. 0 failures in 40 runs, against roughly 8% before; still fails
  against a fanout that blocks instead of shedding.

## [0.21.0] - 2026-08-10

The trade-bust release, and a reminder of why this project writes specs first: the
spec found a defect that had been shipping for four releases before a line of the
feature existed. An operator halt issued after the last checkpoint was never written
to the log, so a venue somebody had deliberately stopped came back open. Trade bust
needed a durable seam for control commands, went looking for one, and there wasn't
one.

The feature itself is mostly a list of things it refuses to do. A bust annuls a
print; it does not put the orders back, does not un-fire the stops the print
triggered, does not rewind the reference price, and does not amend the event that
reported the trade. Each of those is a test, because each looks like a bug until you
notice the book at bust time is not the book at trade time.

### Added

- **Trade bust** (`Engine.Bust`, [TRADE-BUST.md](docs/TRADE-BUST.md)) — annulling a
  print that has already been published. It is an appended `EventBusted` referring
  backwards by trade id, never a rewrite, because the tape a follower replays has to
  stay identical to the tape the primary produced. The surprising part is what it
  deliberately does *not* do, and each of the four has a test: the busted orders are
  not re-rested, the stops the print fired stay fired, `LastTradePrice` is not
  rewound, and the trade event is not amended. A bust arrives after the market has
  moved, and each of those undos would be a second wrong rather than a correction of
  the first.

  The registry lives in the snapshot and therefore in the digest — two engines that
  applied the same commands are equal only if they also agree on what settled. Drill
  D7 is why that matters: a follower that drops the bust has a byte-identical *book*
  and a different digest, which is the only reason the divergence is detectable at
  all. `marketdata.Feed` is the consumer, publishing `UpdateBust` alongside the trade
  id that `UpdateTrade` never carried.

  Validation is identity-only: the engine refuses ids it never issued and says
  nothing about price, size or counterparty, because it does not retain the trades it
  printed. Duplicate busts are refused rather than swallowed.

### Fixed

- **Control commands were applied but never written down.** `Runner.logCommand`
  ended with `default: return // control commands carry no book state; the snapshot
  covers them`. The snapshot covers them *as of the snapshot* — so a halt, resume,
  cancel-only or mark-price change issued after the last checkpoint was in no log,
  recovery did not replay it, and a venue an operator had deliberately halted came
  back **Open**, ready to trade, with nobody told. Shipped in every release since
  control commands existed.

  It is the same reasoning error as the durability comment corrected in v0.20.0: a
  guarantee stated against the wrong reference point. It surfaced because trade bust
  needed a durable seam for control commands and the seam turned out not to exist —
  `CommandLog` now carries `AppendHalt`/`Resume`/`CancelOnly`/`SetMark`/`Bust`, and
  `TestControlCommandsSurviveRecovery` fails against the old code. **Breaking** for
  anyone implementing `matching.CommandLog` outside this repository.

- **The threat model claimed a trade-bust path that did not exist, and named the
  wrong mechanism for it.** [THREAT-MODEL.md](docs/THREAT-MODEL.md) credited the WAL
  spine with "clean trade-bust / replay" while
  [PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) said there was no way to
  amend a published trade. Writing the spec settled it: nobody had built one, and
  replaying a log without the busted trade — the mechanism the row described —
  rewrites history and hands every downstream consumer a tape that never happened.

- **The published test count was 100 short.** `PRODUCTION-READINESS.md` said 480 test
  functions for several releases after the suite passed it; it is 584, and the line
  now carries the command that produces the number so the next reader can check it
  instead of believing it. The event-conformance suite is 23 scenarios, not 22.

### Changed

- **Wire protocol v2 → v3: a trade now has a name.** `Executed` and `MDTrade`
  reported price, quantity and aggressor but no identifier, so no message could ever
  refer back to one specific print — which meant a venue with trade bust could annul
  a fill it had never named, and no client could be told which one. Both payloads
  gain `TradeID` (+8 bytes each), and two messages use it: **`Busted`** (`U`) on
  order entry, private to the two counterparties, and **`MDBust`** (`u`) on market
  data, public. Every other payload is byte-identical to v2 apart from the version
  field itself — the discipline a bump is supposed to carry, and what the regenerated
  golden vectors show.

  **Breaking**: `internal/wire` is not importable, but any client built against v2
  must be rebuilt. `pkg/orderentry.Msg` gains `TradeID` and `KindBusted`.

  Routing the bust turned out to be harder than encoding it, and for a reason that
  is the whole shape of this feature: **by the time a bust arrives, both orders have
  usually left the book.** `orderentry.Registry` forgets an order the moment it fills
  or cancels, so the obvious implementation — look the trade up among live orders —
  delivers a bust to nobody in the common case. The Registry now keeps a bounded
  memory of recent prints (`SetFillMemory`, default 65,536, about 26 seconds of tape
  at the SOAK.md rate) purely so a bust can be routed, and one older than that memory
  increments `UnroutableBusts` rather than vanishing — "we could not tell the client"
  is an operational fact somebody has to act on. Size it to your bust window; CME's
  is eight minutes.

## [0.20.0] - 2026-08-10

The first release whose corrections came from outside. A reader on
r/highfreqtrading read the WAL's durability comment closely enough to see it was
ordered against the wrong thing, and proposed the recovery test that would have
caught it; both are below, credited, because the alternative is pretending the
audit was internal. Shipping alongside them: the interactive tutorial, which
teaches the order book by making you every player in it, on the real engine.

### Fixed

- **The WAL's durability claim was ordered against the wrong thing.** The package
  comment said records are written write-ahead "so no acknowledged command is
  lost." Write-ahead is ordered against *apply*, not against the acknowledgement a
  client receives: append writes into a buffer, only `Sync` survives the process,
  and with `obgw` group-committing every 20ms an order could be acknowledged and
  then vanish. The comment stated the opposite for four releases. A reader on
  r/highfreqtrading pushed on exactly this and was right. The window is now stated
  rather than denied, and `obgw -sync-every-command` closes it for anyone who
  wants acknowledgement to follow durability — correct, and ~210× the cost,
  because the fsync lands on the matching goroutine.

### Added

- **Crash-at-every-boundary recovery test.** The suite sampled five checkpoints
  across a 2000-command tape and compared only the book; the new test kills at
  every write and emit boundary and compares the trade tape too. Both halves are
  verified against deliberately broken code: dropping every replayed cancel fails
  the book assertion, and publishing replayed event batches in reverse order —
  same event count, so the digest is untouched — fails the tape assertion while
  the old test passes against the identical sabotage. Also proposed by the same
  reader.

- **The interactive tutorial** (`web/learn.html`,
  [TUTORIAL-SPEC.md](docs/TUTORIAL-SPEC.md)) — learn the order book by being
  every player in it, on the real engine. Two devices no static explainer has:
  the ladder assembles itself as concepts arrive (chapter 1 opens on an empty
  market, because a market is a list of intentions and the list starts empty),
  and every objective is verified against the engine's actual book state — "get
  filled before the rival" completes when the book proves it, not when a Next
  button is pressed. Six chapters: the first seller, the wall builder, the
  taker, the queue jumper, the whale (the same 8-lot order against a thin and a
  deep book, with the measured slippage as the lesson), and the market maker,
  quoting both sides of a live market with honest P&L — including the loss
  case, when the price moves through the quotes. Browser-verified end to end;
  the pacing fixes came from watching an impatient automated learner reset its
  own queue position by re-quoting — which is itself the chapter's lesson.

## [0.19.0] - 2026-08-03

The showcase release: the library learns to show itself — a live console running
the engine, signals and surveillance in the browser, and an operator dashboard
that is an ordinary subscriber of the venue's own feed — and stops calling
itself production-grade, for the reasons below.

### Changed

- **"Production-grade" is retired from every header.** The README tagline, the
  site's hero and section headings, and the docs-page intro no longer carry the
  adjective, because it fails this project's own tests three ways: the readiness
  doc's thesis is that production readiness is a property of a deployment, not of
  code; the claim asserts an outcome nobody has verified (independent review:
  none; production deployments: zero); and claim-in-the-header,
  truth-in-the-footnote is the pattern this repository criticizes everywhere
  else. The headers now carry the verifiable properties — deterministic,
  integer-exact, drilled — and the README says plainly that nobody runs this in
  production today, until someone does.

### Fixed

- **The docs page published pre-correction benchmark figures and a stale "latest"
  version — for twelve releases.** web/docs.html still showed the v0.6.0-era
  numbers (cancel 253 ns, p999 292 ns, match 352 ns) that v0.13.0/v0.14.0
  re-measured, and named v0.6.0 as the latest release. Flagged, credit where due,
  by a knowledge-graph pass over the repository that marked the page's link to
  the benchmark corrections AMBIGUOUS. Figures now match docs/BENCHMARKS.md, and
  the hardcoded version references are gone rather than updated — a hand-written
  "latest" is a claim that goes stale by construction.

### Added

- **`cmd/obdash` — the operator dashboard** ([CONSOLE-SPEC.md](docs/CONSOLE-SPEC.md)
  phase 2). Deliberately a sidecar: an ordinary market-data subscriber over the
  venue's own wire protocol plus a reader of the admin `/metrics` page, re-served
  to browsers as one embedded page and one Server-Sent Events stream. obgw gains
  no code, no port and no attack surface, and the market-data protocol gets what
  PROTOCOL.md always claimed it supports — a subscriber written from the format
  alone, living outside the venue's test tree. SSE rather than websockets is a
  recorded decision: strictly one-way traffic, native reconnection, plain HTTP,
  zero dependencies. The page draws RUNBOOKS.md's two first-look signals — queue
  depth against capacity with the 75% alert threshold on the meter, and the
  sequence rate — and never shows a stale number as a healthy one: a dead feed, a
  failed scrape and an unpublished venue state each say exactly what they are. A
  slow browser is shed, not buffered without bound — the venue's own rule, applied
  to the venue's dashboard. Verified end to end against a live obgw under obsoak
  load at ~400 md messages/second.

- **The live console** (`web/console.html`, [CONSOLE-SPEC.md](docs/CONSOLE-SPEC.md))
  — the showcase VisualHFT points at, in the shape this library earns: the real
  engine compiled to WebAssembly matching a `sim.NoiseTrader` market in the page,
  with a depth ladder, tape, OFI/CVD/imbalance/Kyle-λ panels computed by the
  shipping `pkg/signals` code, and `pkg/surveillance` watching every event. Every
  panel is titled by the exact library call that produces it — the page's second
  job is adaptability. The spoof button places layered size and pulls it, and the
  shipping `SpoofDetector` names the account in the alert feed: a visitor
  manipulates a market and watches surveillance catch it, in a browser tab.
  `cmd/obwasm` grows `obStep`/`obSignals`/`obAlerts`/`obCancel`/`obSpoof`; the
  bridge holds all market logic and the page is a renderer. Honesty carried over
  from the research docs: the OFI panel says *contemporaneous, not predictive*,
  λ ships with its R², and latency numbers are deliberately absent — browser-WASM
  timings would be noise presented as measurement.

## [0.18.0] - 2026-08-03

The replication release: the HA seams get the consumer that would have noticed if
they were phantoms, and the edge stops holding plaintext.

### Added

- **Replication: the consumer the HA seams never had**
  ([REPLICATION.md](docs/REPLICATION.md), spec'd before any code existed).
  `wal.SetOnAppend` is the live tail — each appended record's payload bytes, in
  log order, under the writer's lock, with CommandLog's obligations.
  `examples/replication` is the reference primary-backup topology: a primary
  shipping its log over TCP through bounded per-follower buffers (a follower that
  falls behind is cut, never waited on — a skipped entry would be a silent gap,
  and a book missing one command is not behind, it is different), a follower that
  bootstraps from a snapshot taken mid-stream and never stops replaying, and
  promotion into a live venue with a fresh log, a base snapshot, and a new
  incarnation as the client fence. Drills D1–D6 run on every CI pass, each
  verified to fail against deliberately broken code. RUNBOOKS.md gains the
  failover procedure; PRODUCTION-READINESS moves high availability from *seams
  only* to *seams proven — topology still yours*, and not one word further.

  What building it found, recorded in the spec's §8: the four documented seams
  held — including mid-stream bootstrap, which no recovery test covered — and the
  fifth phantom was in the *new* seam. The hook's first shape handed subscribers
  the `wal.Entry`, which holds a pointer to the order the engine keeps mutating;
  every drill passed and the first `-race` run did not. The hook now hands the
  record's payload bytes, making the wire byte-identical to the log.

- **`matching.EngineSnapshot.Digest`** — the crash-recovery suite's book
  fingerprint, promoted from a test helper to a contract
  ([REPLICATION.md](docs/REPLICATION.md) deliverable #1). Covers everything
  recovery must reproduce; normalises exactly the fields a legitimate replay may
  differ in (order timestamps, pause deadline, guard window start, WALSeq); does
  not mutate its receiver. A comparison fingerprint between processes running the
  same release, deliberately nothing stronger — pinning a canonical encoding would
  turn every snapshot change into a wire-format negotiation. A perturbation test
  asserts every meaningful field class moves it, and the recovery tests consume
  the public method, so a digest regression fails crash recovery.

- **`orderentry.HashedAccounts`** — StaticAccounts with the plaintext removed: a
  SHA-256 digest table, constant-time compare, deny by default, and the blank-secret
  rule kept in digest form (`HashSecret("")` is refused at construction, because by
  authenticate time it is a real credential that a blank password matches). The hash
  is deliberately fast and the doc comment defends that: these are machine
  credentials a venue issues from a CSPRNG, where SHA-256 is enough — and a
  memory-hard hash on the pre-auth login path would hand every unauthenticated peer
  a CPU-and-memory amplification primitive aimed at the accept loop. Human-chosen
  passwords are the seam's job, not a slower hash's. The digest is computed before
  the account lookup, unconditionally, with a timing test sized so that skipping it
  for an unknown account fails by an order of magnitude.

- **`sha256:` credential entries and `obgw -hash-secret`.** The gateway's credential
  table now holds digests whichever form the file used: `user:sha256:<64 hex>`
  entries load as-is, `user:password` entries are hashed at load and counted, and
  the count is logged so an operator can watch it reach zero. `-hash-secret` reads a
  secret on stdin (never argv — the process list again) and prints the entry form.
  A malformed digest is refused rather than treated as a very strange password — one
  mistyped hex digit must not manufacture an account whose real secret nobody knows —
  and is reported by line number, never content, since the likeliest malformed digest
  is a password misfiled under the prefix. Stated rather than implied: a plaintext
  entry is still plaintext *on disk*, parsing leaves transient copies that are
  garbage rather than zeroed, and an inline `-accounts` string stays reachable in the
  flag package for the life of the process.

- **TLS on every listener** (`obgw -tls-cert -tls-key`), TLS 1.2 floor, `crypto/tls`
  and therefore no new dependency. The handshake runs on the connection's own goroutine
  inside the login deadline that already bounds a silent peer, so connect-and-stall
  cannot hold up the accept loop — asserted, not assumed.

- **`orderentry.Authenticator`**, the seam for where credentials actually live. Storage,
  hashing, rotation and who may read them are properties of a deployment and a
  regulator, and a library that picked would be wrong for most of the people using it.
  The interface is one method.

  `StaticAccounts` is the built-in default and is correct about the things a default
  can be correct about: constant-time comparison, deny by default, and an account with
  a blank secret is not an account. It is explicit that it is not a credential store —
  plaintext in memory, no hashing, no rotation, and a core dump contains every password
  on the venue.

- **`-accounts-file`**, preferred over `-accounts`, because anything on a command line
  is in the host's process list for every user on the box. Permissions are checked and
  a world-readable file draws a warning.

### Fixed

- **The credential parser logged secrets.** A malformed entry was reported by printing
  it, so one typo in a credential list wrote a password to the log — and unlike a
  process list, a log is kept, shipped and indexed. Malformed entries are now reported
  by line number, never by content, and a test asserts no secret reaches the log.

- **Password comparison was not constant time.** `want != req.Password` short-circuits
  on the first differing byte. More useful in practice: an unknown account returned
  before comparing anything, so a bad username was measurably faster than a bad
  password — which turns the login endpoint into a way to enumerate the venue's
  participants, one connection at a time, with nothing in an audit trail that looks
  like an attack.

  The test for this was itself wrong first time round: at 64-byte secrets it passed
  against a deliberately short-circuiting implementation, because the comparison costs
  less than the map lookup preceding it. Resized until it fails by 7860× against the
  broken version, and the comment says what that does and does not prove.


## [0.17.0] - 2026-08-01

### Fixed

- **The snapshot had no integrity check, for four releases.** Every log record carries a
  CRC-32C and a complete record that fails it refuses to start the venue. The snapshot
  is the *base* those records are replayed on top of — so a wrong snapshot is strictly
  worse than a wrong record — and it had nothing. A torn snapshot was already impossible
  (`WriteSnapshot` renames a fully-synced temp file into place), but media corruption
  was not: most bit flips break the JSON and the parser catches them, while a flip
  inside a number parses perfectly and silently restores a book that never existed.

  Now the same shape as the log — magic prefix, CRC, body — and refused the same way. A
  file without the magic is a pre-checksum snapshot and is read without one, so an
  upgrade does not cost a venue its ability to recover; the next checkpoint rewrites it.

  Found while writing the runbook for "corrupt snapshot" and discovering the honest
  procedure would have been *"you cannot detect this"*.

- **Orders that no client could cancel.** Under sustained load the reference gateway
  refused a cancel for an order that was live in its own book. A client does not retry
  a definitive "no such order", so the order stayed there, addressable by nobody, until
  the venue restarted — and the book filled to `MaxOrders`, at which point the venue
  stopped accepting liquidity while reporting itself healthy. Measured at 12,843
  orphaned orders in thirty seconds at 10,000 messages a second; none below 4,000.

  Two causes, and fixing the first made the second visible. The naming index was
  maintained by the publisher's pump, which may lag — a late acknowledgement is still
  an acknowledgement, but a late answer to "which order do you mean?" is a wrong one,
  because the question is not asked twice. And the lookup happened *before* the command
  queue, so a cancel could be refused while the `Enter` that creates it was still queued
  ahead of it. `orderentry.NameIndex` moves naming onto the matching goroutine;
  `matching.Runner`'s `TryEnqueueCancelBy` / `TryReduceAsyncBy` / `TryReplaceAsyncBy`
  resolve the target when the command reaches the front of the queue.

  Found by `cmd/obsoak` within its first hour. Not by 480 tests, two fuzz targets, the
  race detector or any benchmark — it is not a race and not a wrong answer, it is a
  right answer arriving after the question stopped being asked.

- Four further defects, each created by the fix before it: a name that could outlive its
  order once naming and forgetting had different owners; a caller-supplied resolver that
  could panic and take the matching goroutine — and therefore the market — with it; an
  allocation added to the hot path by a composite map key; and a concurrent map write
  from the one `byClOrd` writer the lock split missed, which `-race` never reached and
  a soak crashed the process on in thirty seconds.

### Added

- **[docs/RUNBOOKS.md](docs/RUNBOOKS.md)** — procedures for a torn log, a corrupt log
  record, a corrupt snapshot, a stuck matching goroutine, a mass cancel that pauses the
  venue, an evicted subscriber, a publisher dropping batches, and a book at its ceiling.
  Each written from the code that produces the failure: the signal, what the engine has
  already done by the time you look, what to do, and what makes it worse. Alert
  thresholds are tabulated, and it ends with what has *no* runbook — failover, trade
  bust, credential revocation, clock disagreement.

  Operational readiness stays **weak** despite it. None of this has been rehearsed, and
  a procedure nobody has practised under pressure is a document, not a capability.

- **`pkg/observability`** — a Prometheus collector attached to the engine as an
  `EventSink`, so it counts what the book saw rather than what a gateway believed it
  sent. No new dependency. Expiries are counted apart from cancels, rejections are
  broken down by reason, and `orderbook_last_event_sequence` is exported because a
  stalled matcher reads zero in every rate metric — which is exactly what a quiet market
  reads.

- **An admin edge on `cmd/obgw`** (`-admin`) serving `/metrics`, `/healthz` and
  `/readyz` on its own port. Nothing a scrape touches goes through the command queue.
  `/healthz` deliberately does not probe the matcher: a failed liveness check means
  "restart me", and restarting a venue that is holding a book because a probe was slow
  is worse than the stall.

- **`cmd/obsoak`** — a soak harness. Sustained load, client-observed latency, and growth
  judged on the heap's floor rather than its trend, because live heap saw-tooths under
  GC pacing. Below five minutes of steady state it declines to conclude in either
  direction.

- **[docs/SOAK.md](docs/SOAK.md)** — what it measures, the methodology that took three
  wrong versions to get right, and what it found.

- **[docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md)** — what a venue
  actually needs, with an honest status for each item and the evidence named so it can
  be checked rather than trusted.

  It opens by refusing the premise: production readiness is a property of a deployment,
  not of code. A matching engine is production-ready when a named team runs it, on
  hardware they have capacity-planned, behind controls they have tested, with runbooks
  for failures they have rehearsed — none of which can ship in a Go module.

  Three gaps are named as the difference between "a correct engine" and "a venue you
  could run", and all three are outside what further library work can fix:
  observability (seams exist, nothing ships), operational readiness (no runbooks, no
  health checks, no rehearsed drills), and **sustained load testing** — every
  performance figure in this repository is a microbenchmark over seconds, and nobody
  has run this at volume for a day.

- **The closing auction, and indicative pricing through an auction.** v0.16.0 shipped
  pre-open and the opening uncross; this completes the session at both ends.

  `StateClosingAuction` accumulates orders on top of the live continuous book without
  matching them, and leaving it resolves everything at one closing price. It needed
  almost no new code: the transition rule was generalised from "pre-open into open" to
  **"leaving any accumulating phase resolves what accumulated"**, which is what stops a
  third accumulating phase arriving one day without an uncross.

- **`Engine.IndicativeAuction`** reports what an auction would clear at right now —
  price, volume, and the imbalance left over — without changing anything. A pre-open
  that revealed nothing until it printed would be a sealed-bid auction, which is a
  different market design with different incentives.

  The imbalance is buy minus sell interest at that price, and its sign is the number a
  participant acts on: the price says where the auction is, the imbalance says which
  way it moves if nobody responds. There is a test for the sign in both directions,
  because getting it backwards would be worse than not publishing it.

- **`MDIndicative` on the market-data feed**, published by the venue on its own cadence
  via `Feed.PublishIndicative` rather than derived from every order. During an auction
  the indicative price moves on essentially every message; broadcasting that would be
  several times the traffic of the order flow itself, for information nobody can act on
  at that granularity. Real venues publish on a timer, and the timer is a venue
  decision — the same reasoning that keeps the calendar out of the engine.

### Changed

- **Corrected: the capacity figures published in `docs/SOAK.md` did not reproduce.**
  5,000 msg/s comfortable, 7,000 clean and 10,000 saturating became 3,500 clean and
  5,000 saturating four hours later, on the same machine and the same code. Ruled out
  as a code regression by an interleaved A/B against a worktree at the earlier commit —
  three rounds, alternating, indistinguishable. The difference was a desktop that had
  been idle in the morning and was running a window server and browsers by the evening;
  the measurement controlled for none of that and recorded none of it.

  This project therefore does not know what rate the venue sustains. It knows the shape
  — the durable path through a socket and a protocol runs three orders of magnitude
  below the in-process benchmarks, and the command queue gives first — and it knows two
  numbers taken under conditions it failed to write down. Third published figure
  corrected against itself; second where the documentation was flattering.

  What held at every rate and on both arms: a bounded book, no orphaned orders, no
  dropped batches, no leaked goroutines or descriptors, p50 of 5 ms below saturation.
  Timing figures are a property of the host; correctness findings are a property of the
  code. `obsoak` now runs a fixed-work probe before and after each run and prints it
  first, so a run carries the evidence of its own conditions.

- `docs/BENCHMARKS.md` now states what its numbers cannot tell you: no sustained load,
  no memory or goroutine growth over a long session, no multi-client gateway test, and
  therefore no capacity plan. The figures answer "did this change make matching
  slower", not "can this run a venue".

## [0.16.0] - 2026-07-31

The session: a venue that opens, runs, and closes, and orders that know when they end.

**Phases.** The engine knew open, cancel-only and halted, which is not a session. It
now has pre-open and closed too. Pre-open accepts orders and does not match them, so
the book may legitimately cross — the single deliberate exception to an invariant held
everywhere else — and the transition to open resolves it at one clearing price. A
venue never opens onto a crossed book.

**Time-in-force that ends.** DAY and GTD, with the venue holding the deadline instead
of the client remembering to cancel. Deadlines live in a min-heap rather than a
per-command sweep, because a sweep would put an O(book) scan in front of every cancel
and the tail-latency figures published two releases ago would have stopped being true
the day it shipped.

**Two things kept deliberately outside.** The engine holds no calendar — it knows
which phase it is in and what that permits, not when phases change — because a trading
calendar is a venue's business, the same reasoning that keeps consensus out of this
library. And auction prints are marked, because both sides were resting and there is
no aggressor; feeding a conventional one to the repository's own delta/CVD study would
corrupt exactly what that study measures.

**The bug this cycle found.** The order-entry handler carried its own copy of the
side/type/TIF mapping, separate from the one every conditional entry uses. They
diverged the moment DAY and GTD existed: a plain `Enter` carrying the new bytes fell
through to the default and would have rested as GTC — an order the client believed had
a deadline, living forever. Caught because the first version of the test asserted only
that the order was *accepted*, which it was, as the wrong time-in-force.

### Added

- **Trading phases, and the opening auction that joins them.** The engine knew three
  states — open, cancel-only, halted — which is not a session. It now has `PreOpen` and
  `Closed` as well, and `SetPhase` to move between them.

  **Pre-open accepts orders and does not match them**, so the book may legitimately
  cross: a bid above an ask simply rests there. That is the point, and it is the single
  deliberate exception to an invariant the engine holds everywhere else. Everything
  that accumulates is resolved at one price by `Uncross` — every buy at or above it and
  every sell at or below it trades, in price-time priority, at that single price. The
  clearing price maximises executed volume, reusing `pkg/auction` rather than
  reimplementing it, and moving from pre-open to open runs the uncross first so a venue
  never opens onto a crossed book.

  Market orders are refused in pre-open: an unpriced order has nothing to rest at, and
  holding it to execute at whatever the auction decides is not what it asked for.

  **Auction prints carry `Trade.Auction`.** Both sides were resting, so there is no
  aggressor and `TakerSide` is not meaningful on them — anything inferring order flow
  from aggressor side has to exclude them rather than treat the convention as data.
  That matters here specifically, because this repository's own delta/CVD study is
  built on aggressor ground truth.

  **The engine holds no calendar.** It knows which phase it is in and what that phase
  permits; it does not know when phases change. The venue calls `SetPhase`, because a
  trading calendar is a venue's business and embedding one would force every embedder
  to accept somebody else's — the same reasoning that keeps consensus out of this
  repository.

- `orderbook.FrontOrder`, returning the highest-priority resting order on a side in
  O(1). The uncross needs "the next order that should trade" repeatedly, and deriving
  that from `Orders()` would allocate and walk the whole book per fill — turning an
  uncross of a large book into O(book²) at exactly the moment a venue is opening.

- **DAY and GTD time-in-force, with the venue holding the deadline.** The engine
  supported GTC, IOC and FOK; DAY is what most real order flow uses, and without it a
  client wanting an order gone at the close had to remember to cancel it — a job the
  venue should be doing.

  DAY rides the existing `Enter` as a new TIF byte, because the venue's session close
  is its deadline and no field is needed. GTD gets its own message (`EnterDated`),
  because `Enter` has nowhere to put a timestamp and adding one would move every byte
  after it. Sending the GTD byte on a plain `Enter` is refused rather than quietly
  downgraded to GTC, which would leave an order the client believes is dated resting
  forever.

  Deadlines live in a **min-heap**, not a per-command sweep of the book. A sweep would
  be O(book) in front of every cancel, and the tail-latency figures this repository
  publishes would have stopped being true the day it shipped. The cost when nothing is
  due is one comparison.

  A DAY order's deadline is **resolved on intake** and stored on the order, not
  re-derived at expiry. That is what makes replay exact: the log carries the resolved
  instant, so a recovery expires it exactly when the live engine did rather than at
  whatever the session close happens to be during replay.

  Expiry ignores `MinRestingTime` — a floor that could hold an order past its own
  stated lifetime would be the venue inventing liquidity the client never offered — and
  the `Canceled` carries `ErrOrderExpired` so a consumer can tell it from a cancel the
  client issued. Expired orders leave *before* matching, so nothing ever trades against
  liquidity that should already be gone.

  A venue with no session close configured refuses DAY orders (`ErrNoSessionClose`)
  rather than treating them as GTC.

### Fixed

- **The order-entry handler carried its own copy of the side/type/TIF mapping**, and
  it diverged the moment DAY and GTD were added: a plain `Enter` carrying the new TIF
  bytes fell through to the default and would have rested as GTC, so an order the
  client believed had a deadline would have lived forever. `Enter` now builds through
  the same path as every conditional entry.

  Caught because the first version of the DAY test asserted only that the order was
  accepted — which it was, as the wrong time-in-force. The test now asserts the
  resting order's actual TIF and deadline.

## [0.15.0] - 2026-07-31

The second edge. The venue could take orders and could not publish a market.

**What ships.** `marketdata.Feed` — one sequenced broadcast of book deltas, trade
prints and venue-state changes — and `cmd/obgw -mdaddr`, a listener that serves it.
A subscriber joining with nothing gets a snapshot and then the live stream; one
holding a cursor gets the gap-fill and no snapshot.

**The contract, stated so it can be falsified.** For one venue incarnation the
sequence is dense and gap-free from 1, and `Snapshot(at Seq S)` plus every update
after `S` equals the engine's book. A subscriber can join at any instant and be
exactly right. Asserted from many starting points across a random tape, and again
end to end over a socket against the venue's own view.

What makes it true is that `Snapshot` takes the book **and** its sequence under one
lock. Reading them separately is exactly the bug the order-entry side shipped in
v0.12.0, where a report claimed consistency with a sequence the client had not
reached and a change got applied twice. The same mistake was available here.

**What is deliberately not here.** Conflation. A slow subscriber is disconnected
rather than being served a conflated stream and a fresh snapshot when it catches up,
which is the better answer for market data and is a feature with its own failure
modes. Absent rather than half built.

**Two bugs found by writing the tests, not by testing the feature.** A manual halt
told nobody — `Engine.Halt`, `Resume` and `SetCancelOnly` set the state and emitted
nothing, so only the automatic transitions ever reached a consumer, and the one halt
a venue most needs to broadcast reached none. And the feed was not seeded from a
recovered book, so after a restart a subscriber's first snapshot would have shown
only what changed since — almost nothing.

That second one is the third time in this repository that recovery has been found not
to restore something *above* the book. The pattern is worth naming: replay rebuilds
the engine, and every consumer that derives state from the event stream needs its own
explicit adoption path.

### Added

- **A market-data edge.** `cmd/obgw -mdaddr` serves the public feed on its own
  listener from the same engine: snapshot-plus-delta for a new subscriber, gap-fill
  for one that holds a cursor, trades and venue-state changes in the same ordered
  stream. Seven message types, numbered separately from order entry.

  One process, two edges, because that is how a venue is shaped — order entry is
  authenticated and per-account, market data is anonymous and identical for everyone,
  and sharing a port would put an unauthenticated subscriber on the order-entry code
  path.

  A snapshot is a run of `MDLevel` followed by an `MDSnapshotEnd` carrying the
  sequence it is consistent with, rather than one variable-length message: every
  payload stays fixed-width, and the terminator's count means a truncated snapshot
  cannot look like a complete one. Same shape as the `Query` reply, same reasons.

  An evicted or wrong-incarnation subscriber is refused explicitly rather than
  quietly resynchronised, so it learns its own picture had a hole in it. A slow one is
  disconnected; conflation is the better answer for market data and is deliberately
  absent rather than half built.

- **`marketdata.Feed`** — the venue's public market-data stream: one sequenced
  broadcast of book deltas, trade prints and venue-state changes, with
  snapshot-plus-delta recovery.

  The guarantee, stated so it can be falsified: for one incarnation the sequence is
  dense and gap-free from 1, and **`Snapshot(at Seq S)` plus every update after `S`
  equals the engine's book**. A subscriber can start anywhere and be exactly right.
  Asserted from many different starting points across a 2,000-command random tape,
  including from sequence 0.

  What makes it true is that `Snapshot` captures the book *and* its sequence under one
  lock. Reading them separately is precisely the bug the order-entry side shipped in
  v0.12.0, where a report claimed consistency with a sequence the client had not
  reached and a change got applied twice.

  It is deliberately **not** `pkg/orderentry`. Order entry is private and per-account,
  and a client that falls behind is owed the messages it missed — an execution report
  it never sees is a position it does not know it has. Market data is public and
  identical for everyone, so a subscriber that falls too far behind is simply told the
  current state. One stream instead of a registry, and a fresh snapshot instead of a
  refusal.

  Retention is bounded, because an unbounded ring turns one stalled subscriber into a
  venue-wide memory leak, and an evicted cursor is refused explicitly rather than
  served a truncated slice that looks complete.

### Fixed

- **The market-data feed was not seeded from a recovered book.** A restart left it
  aggregating from an empty state against a non-empty venue, so a subscriber's first
  snapshot would have shown only what changed since the restart — almost nothing.
  `Feed.Adopt` seeds it, publishing no deltas, because those levels are the starting
  state rather than changes to it. The same shape of bug the session layer's index had
  before v0.12.0, caught this time by a test that restarts a venue and subscribes.

- **A manual halt told nobody.** `Engine.Halt`, `Resume` and `SetCancelOnly` set the
  state and emitted nothing. Only the *automatic* transitions — a guardrail trip, a
  band-breach pause — reached the event stream, so the one halt a venue most needs to
  broadcast, an operator deliberately stopping trading, reached no consumer, no
  market-data feed and no client.

  All three now emit, and only on an actual transition: halting an already-halted
  venue produces nothing, because an event that describes no change is worse than
  none. Cancel-only gets its own kind (`EventCancelOnly`) rather than being reported
  as a halt — a subscriber told the venue is halted when it is still accepting
  cancels would draw the wrong conclusion about whether it can get out of a position.

  Found by writing a market-data test that asserted a halt appears in the feed.

## [0.14.0] - 2026-07-30

A depth bug that had been wrong in public, and the measurements that found it.

**The bug.** A price level's aggregate quantity was not reduced when a resting order
was fully consumed, so L2 depth was over-reported after every complete fill — three
sells of 5 swept by a 12-lot buy left the level reporting 13 lots with a single 3-lot
order at it. `Snapshot` is the read model for `pkg/signals`, the research studies, the
WASM demo and any L2 feed, so all of them saw inflated depth. It was present at least
as far back as v0.12.0.

**How it was found, which is the part worth repeating.** Not by testing the book. By
building an incremental L2 feed above the engine and asserting the derived levels
equalled the engine's own `Snapshot` after every command of a random tape. Two views
of the same state, forced to agree. Every existing test checked the orders *or* the
level; none checked that the two matched, which is exactly the gap a bug like this
lives in.

**What it cost.** Order-flow imbalance is computed from depth, so the published study
moved: mean contemporaneous R² was 0.1685 and is 0.2357, with the predictive gap going
from ~540× to ~577×. The conclusion is *strengthened* — OFI explains more of the
same-interval move than reported and still essentially none of the next one. Kyle's λ
keeps λ, R² and trade counts identical and moved only its depth column. The delta/CVD
study is untouched. `cmd/ofistudy` also carried its verdict as literal text and printed
figures its own table contradicted; it now computes them.

**Two claims corrected by measuring them.** Publishing one tail-latency scenario
understated the tail by ~30×, and the mass cancel — never measured — blocks the
matching goroutine for ~872 µs per 5,000 orders, which makes the kill switch a
venue-wide pause. And recovery was documented as "bounded to O(recent)": true of the
replay, false of the restart, since loading a 100,000-order snapshot is ~174 ms
against ~21 ms for a 10,000-record tail. Restart is O(book) + O(tail).

### Fixed

- **A price level's aggregate quantity was wrong after every full fill, and nothing
  checked it.** The matching engine fills a maker — dropping `RemainingQty` to zero —
  and only then removes it from the book, and `PriceLevel.unlink` subtracted
  `RemainingQty`. So it subtracted nothing, and the maker's entire original size
  stayed in the level total forever. Three sell orders of 5 at one price, swept by a
  12-lot buy, left the level reporting **13** lots with a single 3-lot order resting
  at it.

  `Snapshot` is the read model for `pkg/signals`, the research studies, the WASM demo
  and any L2 feed, so **all of them saw over-reported depth**. Present at least as far
  back as v0.12.0 — confirmed by running the same check against that tag.

  Fixed by making the total the book's own property: each node records what it
  contributed (`node.contributed`), and `unlink` subtracts that. The level total was
  previously a discipline every removal site had to remember, and forgetting it at two
  sites is precisely what this bug was.

  No test caught it because every existing assertion checked the orders *or* the
  level, never that the two agreed. There is now an invariant — level total equals the
  sum of the `RemainingQty` resting at it — asserted after a full fill, after a
  removal with no prior update, and after every step of a 2,000-operation churn. It
  was verified to fail against the old code, after a first attempt at it passed for
  the wrong reason: with a single order at the level, the emptied level is deleted
  outright and takes the error with it.

- **The research numbers were re-measured, and two of the three studies moved.**
  Order-flow imbalance is computed from level depth, so it was affected: mean
  contemporaneous R² was published as 0.1685 and is **0.2357**, with the gap to the
  predictive figure going from ~540× to ~577×. The conclusion is *strengthened* —
  OFI explains more of the same-interval move than reported and still essentially
  none of the next one. Kyle's λ study keeps λ, R² and trade counts byte-identical
  (λ is estimated from trades, not depth) and only its depth column and λ·depth
  product moved. The delta/CVD study is unchanged: it reads trades and aggressor
  inference, and never touches depth.

  `cmd/ofistudy` carried its verdict as literal text — "~17% … a ~540x gap" — and so
  printed numbers its own table contradicted the moment the measurement changed. It
  now computes them from its results, and prints the slope columns the write-up
  needs so the table can be regenerated from the program rather than assembled by
  hand.

### Added

- **Tail latency for six more scenarios, and the mass cancel measured for the first
  time.** One scenario was published — a cancel-heavy mix — and it was the friendliest
  of the six. Its p99.9 is 958 ns; an aggressive sweep's is 6,875 ns with a p99.99 of
  31 µs, so **publishing only that one understated the tail by roughly 30×**. Each
  scenario now states its preload, reports out to p99.99, and covers the operations
  that were never measured: a thin-book walk, all five self-trade-prevention modes
  (all cheap), and the bulk cancel.

  The mass cancel is the one worth knowing: pulling one account's 5,000 resting orders
  takes **~872 µs at p50 and ~1.26 ms at p99**, on the matching goroutine, so nobody
  else's orders are processed for that whole time. The kill switch is a venue-wide
  pause proportional to the account's book. It scales, so a 100,000-order account is
  on the order of 18 ms.

  Methodology borrowed from `joaquinbejar/OrderBook-rs`, whose HDR bench suite does
  this properly: name the scenario, state the preload, report the upper quantiles.

- **Recovery time, which was claimed and never measured.** A 100,000-order book
  restarts in **~174 ms** from a checkpoint; a 10,000-record tail on top adds ~21 ms,
  at ~2.1 µs per record.

  That corrects a claim rather than confirming one. The package doc said recovery is
  "bounded to O(recent)", which is true of the *replay* and not of the restart:
  loading the snapshot is O(book) and dominates by an order of magnitude. Restart is
  O(book) + O(tail). The doc now says so.

  It also locates the cost: **reading the log is ~90% of replay** — 2.1 µs of 2.3 µs
  per record, at ~15 allocations per record, because records are JSON. A binary record
  format would not help write throughput, which is fsync-dominated, but it would cut
  restart time. That is a better-founded reason to consider one than the encode
  benchmark that first suggested it.

- **`marketdata.L2Feed`** — an incremental depth feed derived from the event stream:
  aggregated level changes, coalesced per command, with absolute quantities so a
  subscriber that misses one recovers on the next rather than staying wrong.

  This is where `matching.EventBookDelta` was heading, and the engine was the wrong
  place for it. L2 is a pure function of L3, the event stream is already tested to
  reconstruct the L3 book exactly, and computing deltas on the matching goroutine
  would add work to a path whose whole design goal is to allocate nothing and return.
  The constant stays declared but unemitted, with that reasoning attached, because
  removing a value from the middle of an iota block silently renumbers every kind
  after it.

  The test that matters compares the derived levels against the engine's own
  `Snapshot` after every command of a 3,000-command random tape — the two views
  cannot drift without it failing. It is also what found the depth bug above.

- **`EventTriggered` is now emitted**, on both paths where a conditional order fires:
  the cascade after a trade, and the immediate fire when a stop arrives already
  through its trigger. It precedes the `Accepted`, because a consumer seeing only the
  `Accepted` cannot tell a fired stop from an order a client just submitted, and "a
  stop fired" is the event a risk system actually wants.

## [0.13.0] - 2026-07-30

Completes the client's order lifecycle, makes the log trustworthy, and doubles
cancel throughput.

**The protocol.** A client can now express every order type the engine implements
and every operation it supports: six order types (stop, stop-limit, OCO, iceberg,
pegged, trailing joining limit and market), atomic cancel/replace, mass cancel, and
cancel-on-disconnect. Before this release the wire carried two of six order types and
had no replace, so a reprice meant two messages with the client naked in between.
Fourteen new message types, and the protocol stays at **version 2** — every golden
vector from a released version is byte-identical, which is the third dividend from
the type byte v0.11.0 spent a version freeze on.

**The log.** It was not recording five commands that mutate the book — every
conditional order type — so a stop entered after the last checkpoint did not survive
a restart. And it had no checksums at all: a record altered on disk but still valid
JSON was replayed as truth, silently. Both fixed, with the torn-tail and
media-corruption cases now treated differently on purpose.

**The speed.** Cancel went from ~47 ns to ~22 ns and the match path lost 27%, from
profiling this engine against a C++ one and two other Go books. Reading the wall
clock was 46% of the match path; a stop cascade ran over an empty stop book after
every match; and the order index was a Go map where a purpose-built one is 12×
faster. On the same machine and the same 200,000-order book, cancel is now ~3×
faster than liquibook's C++ book and at parity with `geseq/orderbook`.

**A note on how most of this was found.** Six of the ten fixes below came from
building the next feature and asking what it would be sitting on, not from testing
the feature itself: `QueryEnd.Seq` asserting a boundary the client had not reached,
two message types assigned the same byte, five commands missing from the log, an
accessor that raced. The comparison work in particular paid for itself twice — the
WAL checksum gap came from reading a Rust order book's dependency list.

### Performance

- **Cancel is ~2× faster: the order index is no longer a Go map.** A
  `map[int64]*node` was the largest single cost in a cancel. It is now a
  purpose-built table using Fibonacci (multiplicative) hashing over power-of-two
  buckets, with chained entries recycled through a free list.

  Go's map is general-purpose: it hashes arbitrary key types through a runtime
  call, keeps tophash bytes for probing, and handles concurrent-access detection
  and incremental growth. None of that is needed for a dense, monotonically
  increasing `int64` assigned by the book itself, in a structure already serialised
  by the book's own mutex. Measured in isolation, get + delete + put costs ~45 ns
  through Go's map and ~3.7 ns through this.

  Book-level cancel, measured by alternating baseline and patched builds in one
  session: **~47.7 ns → ~23.3 ns**. All ten patched measurements came in below all
  ten baseline ones.

- **A cancel no longer hashes an account string.** The per-account admission
  counter was a `map[string]int`, hashed on every add *and* every cancel. Accounts
  are now interned to a dense `int32` on first sight and the id is cached on the
  book node, so a cancel reads it off the node it already holds. An add still hashes
  once, which is unavoidable — the order arrives carrying a string.

  Prompted by measuring against `geseq/orderbook` (15–21 ns cancel) and
  `joaquinbejar/OrderBook-rs` (41 ns at a 1 M book). Two independent libraries
  beating this one at the same operation was the signal; both did it with a
  purpose-built index rather than the language's map.

### Added

- **Atomic cancel/replace.** `ReplaceOrder` (`Z`) cancels a resting order and enters
  another in one command. Without it a reprice is two messages and the client is naked
  between them: if the connection dies in the gap it cannot tell whether it holds zero
  orders or one, and another participant can take the price meanwhile.

  Priority is forfeited by design — an order that could reprice in place would let a
  participant reserve a place in line — so `Reduce` remains the way to shrink at the
  same price. There is no new outbound message: a successful replace is the `Canceled`
  for the old id followed by the `Accepted` for the new one.

  The failure semantics are asymmetric and deliberate. If the original cannot be
  cancelled, **the replacement is not entered** and the refusal names the original id;
  a client replacing an order it no longer holds did not ask to open a new position.
  If the cancel succeeds and the replacement is then refused, the client holds neither
  and is told so within the same command — reported rather than discovered, which is
  what the two-message sequence could not offer. Restoring the original instead would
  hand it back at the tail of the queue without saying so, and a silent loss of
  priority is worse than a reported refusal.

  It is subject to the minimum resting time, like cancel and reduce: it withdraws
  displayed size, and a verb that escaped the floor would leave the anti-spoofing
  control guarding two routes out of three.

- `Engine.Replace`, `Runner.TryReplaceAsync`, `wal.KindReplace` and
  `CommandLog.AppendReplace`. The log records a replace as **one** record rather than a
  cancel plus a submit, so replay cannot apply half of it.

- **Conditional orders on the wire: stop, stop-limit, OCO, iceberg, pegged and
  trailing.** The engine has supported all of them since v0.5.0 and the protocol
  could express none: a client could place a limit or a market order and nothing
  else, so four of the six order types the engine implements were reachable only by
  an embedder calling it in-process. Same shape of gap as `Reduce` before v0.12.0 —
  real, tested capability with no way for a client to ask for it.

  Five messages (`EnterStop` `S`, `EnterOCO` `O`, `EnterIceberg` `I`, `EnterPegged`
  `P`, `EnterTrailing` `W`), each carrying the same 56-byte base-order block as
  `Enter`'s body plus its own parameters, rather than one message with a union of
  fields — three meaningless fields per message is what the v0.11.0 audit spent its
  time removing.

  Design points worth stating: an OCO's stop leg inherits symbol, side, quantity and
  TIF from the primary, so legs of differing size (which would leave a residual
  position behind whichever fired) are not expressible; a pegged order must send
  price 0 and is refused otherwise, rather than having a price it supplied silently
  overwritten; and an iceberg carries no jitter field, because reload-size jitter is
  set from the engine's own configuration and a client value would be decoded and
  discarded — the exact `Symbol` bug from v0.10.0.

- `Runner.TryEnqueueStop` / `TryEnqueueOCO` / `TryEnqueueIceberg` /
  `TryEnqueuePegged` / `TryEnqueueTrailing`, and `Runner.TrailingStopCount`. The
  conditional types had synchronous entry points only, which a network ingress must
  not use: those hand back the engine-owned order.

- **Mass cancel on the wire.** `MassCancel` / `MassCancelAck` (`F` / `G`).
  `Engine.CancelAllForUser` has existed since v0.9.0 with no way for a client to
  invoke it, which is the difference between a venue you can test against and one you
  would quote on. Each removed order still produces its own `Canceled`; the ack
  follows them and carries the count, so a completed sweep of zero is distinguishable
  from a connection that died mid-sweep.

- **Cancel-on-disconnect.** `CancelOnDisconnect` / `CODAck` (`B` / `V`), a message
  rather than a `LoginRequest` field because adding a field there would move every
  byte after it and invalidate a committed vector. Acknowledged explicitly: a client
  must never be guessing about a control that decides whether its book survives.

  A **venue** shutdown deliberately does not fire it. A graceful shutdown drops every
  connection at once, and sweeping there would journal a cancel for every order of
  every enabled session, permanently destroying books that are meant to come back
  after the restart. There is a test for exactly that.

- `matching.Runner.TryCancelAllAsync`, the ingress-shaped path for the sweep: the
  enqueue happens on the caller's goroutine so it cannot overtake an order submitted
  just before it, while the wait moves off that goroutine.

Four more message types, still **no version bump** — every pre-existing golden
vector is byte-identical. `MassCancel` shares `Query`'s exact layout and
`MassCancelAck` shares `QueryEnd`'s, separated by nothing but the type byte, which is
the third time that has paid for itself.

### Fixed

- **Two message types were each assigned twice.** Adding the conditional-order
  messages gave `'O'` to both `EnterOCO` and `OpenOrder`, and `'P'` to both
  `EnterPegged` and `Replaced`. Nothing failed, because the payload widths differed and
  `checkHeader` tests width first — which means those messages were being told apart by
  *length*, the exact v1 dispatch the type byte was introduced to replace. A collision
  between two messages of equal width would have silently decoded one as the other.

  `EnterOCO` is now `'N'` and `EnterPegged` `'Y'`, and
  `TestMessageTypesAreDistinct` asserts every `Msg*` constant is unique across both
  directions so this cannot come back. Two golden vectors changed as a result — both
  added earlier in this same unreleased cycle, so no released client has seen them.

- **The conditional order types were never written to the log.** `cmdStop`,
  `cmdOCO`, `cmdIceberg`, `cmdPegged` and `cmdTrailing` all mutate the book and none
  of them reached `CommandLog`, so a stop or iceberg entered after the last
  checkpoint did not survive a restart. Snapshots captured them (v0.9.0 fixed that);
  the log tail did not.

  Invisible while those commands were reachable only in-process, and about to become
  client-facing. `CommandLog` gains five methods, the log five entry kinds, and
  replay reconstructs each wrapper from its recorded parameters. The `countingLog`
  test double implements the whole interface deliberately, so adding a mutating
  command without logging it fails to compile — which is how this was caught.

- **`Engine.TrailingStopCount` was unsafe to call from another goroutine**, and a
  `Runner` accessor briefly exposed it as if it were not. The other read accessors
  delegate to the book and stop book, which carry their own locks; trailing stops
  live in a plain map owned by the matching goroutine. `Runner.TrailingStopCount`
  now goes through the command queue, and the engine method documents its
  single-writer requirement. The race detector caught it.

- **`QueryEnd.Seq` asserted a boundary the client had not reached.** Draining the
  publisher only moves events into the account's *stream*; the connection receives
  them from a separate polling goroutine, so a report written straight to the
  outbound queue could overtake stream messages it claimed to come after. A client
  applying them in arrival order would apply the same execution twice — the exact
  failure the drain was introduced to prevent.

  Found while building the mass-cancel ack, whose test asserted the ordering
  directly and failed. Both paths now wait until the connection has actually queued
  everything through the reported sequence.

- **The write-ahead log had no checksums, so silent corruption was replayed as
  truth.** `ReadAll` stopped at a record that failed to *parse*, which meant a
  record altered on disk but still valid JSON — a flipped digit inside a price —
  was handed to the engine and booked. The recovered venue was wrong about what
  had happened and nothing anywhere said so.

  Records now carry a CRC-32C, and the file carries a magic header so the framing
  is identifiable rather than guessed. The two failure modes are deliberately
  separated: a **torn tail** (a short final record from a crash mid-write) still
  stops cleanly, because that command was never acknowledged, while a **complete
  record whose checksum disagrees** is refused with `ErrCorrupt`. Stopping quietly
  on the second would be indistinguishable from a clean end of log while discarding
  every acknowledged command after it. A venue that cannot trust its log should
  refuse to start, not serve a book that does not match it.

- **A corrupt length prefix could ask recovery for a 4 GiB allocation.** The
  4-byte record length was read off disk and passed straight to `make([]byte, n)`,
  so one flipped bit requested up to 4 GiB at the moment a venue can least afford
  it. Bounded by `MaxRecordBytes` (8 MiB, far beyond any real record), and a zero
  length is refused rather than looping.

  Logs written before this change have no header and are still read — without
  verification, since there is nothing to verify against — and appending to one
  keeps its original framing rather than switching format mid-file. Rotate to get
  checksums on an existing log; `Writer.Checksummed` reports which framing is in
  use.

  Found by reading `joaquinbejar/OrderBook-rs`, which checksums its journal.

- **Published benchmark figures that did not reproduce.** Re-measured every number
  in [docs/BENCHMARKS.md](docs/BENCHMARKS.md) and the README on the stated hardware
  (median of 5 runs, idle machine, `go1.23.5`), and corrected what disagreed. The
  engine did not change; the documentation was wrong, in both directions.

  - `OrderBook_Cancel` was published at 253 ns. Five runs gave 265–301 ns, median
    273 — the old figure is outside the measured range. Corrected, and then
    corrected again for a bigger reason: see the next item.
  - `Runner.Process` was published at **4 allocs/op**; it is **3**. Checked against
    the v0.11.0 tag to confirm this was a recording error rather than a change.
  - "Group commit costs roughly an order of magnitude" — it costs **~30×**
    (18,260 ns against 613 ns). "Syncing every command costs three further orders
    of magnitude" — it costs **~210×**, not ~1000×.
  - "`Checkpoint` is on the order of a millisecond" — it is **~0.3 ms** over a
    5,000-order book.
  - Tail latency p999 was published at 292 ns; five of six runs give **250 ns**.
    The "p999 within ~3.5× of the median" claim becomes ~3×.
  - Several figures were *conservative* rather than wrong (`BestBid`, `MatchInto`,
    `Process`) and have been brought to their measured medians too, so the table is
    internally consistent rather than a mix of vintages.

- **The cancel benchmark's book size was an undisclosed parameter.**
  `OrderBook_Cancel` inserts `b.N` orders and cancels all of them, so `b.N` is also
  the book depth — and Go picks `b.N` by wall-clock. The published figure was
  therefore a **ten-million-order book**, a depth no real symbol reaches, and it
  understated the engine by 4×: cancel is ~65 ns at 200,000 resting orders and
  ~273 ns at 10 M. Both are now published, with the depth stated, plus the scaling
  curve between them. `OrderBook_Add` has the same property (92 ns at 200 K, 206 ns
  at 10 M); `CancelReplace` and `LevelChurn` do not, because their working sets are
  fixed, which is why those two barely move.

  Stated rather than quietly corrected, because it generalises: for any book-level
  benchmark the book size is part of the result, and a figure quoted without it is
  not comparable to anything.

- **"0 allocs/op" was being reported as stronger than it is.** Go computes that
  column by integer division, so anything under 1.0 prints as `0` — which is how
  `OrderBook_Cancel` could publish `0 allocs/op` and `41 B/op` on the same line
  without the contradiction being visible.

  Measured directly against `runtime.MemStats`, cancel allocates **0.0002**
  objects/op and market-maker churn **0.009**, so the claim holds in substance —
  but `Add` into a growing book allocates **1.05**, which is what "pooled" means
  rather than "allocation-free". All three are now asserted by tests in
  `pkg/orderbook/alloc_test.go`, including a floor on `Add`, so the claim can fail
  instead of being a rounded-down column.

- **A scope note that had gone stale.** Both the README and BENCHMARKS said the
  figures exclude "any session or order-entry protocol — none of which exist in
  this repository". Those layers have existed since v0.10.0 (`internal/wire`,
  `pkg/orderentry`, `cmd/obgw`). They are still unmeasured, which was the real
  point, and it is now stated that way.

- `README.md` linked no protocol documentation, and described the release history
  as "v0.1.0 → v0.8.0".

### Changed

- **Breaking (embedders):** `matching.CommandLog` gained six methods —
  `AppendReduce`, `AppendCancelAll`, `AppendReplace`, `AppendStop`, `AppendOCO`,
  `AppendIceberg`, `AppendPegged` and `AppendTrailing`. `pkg/wal.Writer` implements
  all of them; a custom log will not compile until it does, which is the intended
  outcome — silently continuing to drop those commands is the bug being fixed.
- **Breaking (embedders):** `Engine.Reduce` and `Runner.Reduce` now fail with
  `ErrCancelTooSoon` inside a configured `MinRestingTime`, as does the new
  `Engine.Replace`. Venues that leave the floor at zero — the default — see no change.
- `Engine.TrailingStopCount` is documented as single-writer only; use
  `Runner.TrailingStopCount` from any other goroutine.

## [0.12.0] - 2026-07-30

Completes the client's side of the order lifecycle, and repairs three things found
underneath it.

v0.11.0 spent a version freeze on a message-type byte. This release is what that
bought: four new message types, no bump, every pre-existing golden vector
byte-identical. `Query` / `OpenOrder` / `QueryEnd` give a client a way back to a
correct picture in-protocol, and `Reduce` lets it shrink an order without going to
the back of the queue — a capability the engine has had since v0.10.0 and no client
could ask for.

The three repairs are the more interesting half, and all three were found by asking
what the new message would be sitting on rather than whether it worked. The command
log was not recording two commands that mutate the book. The anti-spoofing floor
guarded `Cancel` and not `Reduce`, which would have handed the Coscia pattern to
every authenticated client the moment the wire carried a reduce. And recovery
rebuilt the book without the index over it, so a restart left recovered orders
unreachable and — quietly, with nothing logged anywhere — stopped reporting their
fills to the makers who owned them.

### Fixed

- **Two mutating commands were never written to the log.** `Reduce` and
  `CancelAllForUser` both change the book, and `CommandLog` recorded only submits
  and cancels — so recovery restored a reduced order at its original size, and
  handed a pulled account its whole book back. Neither failed loudly; the
  recovered venue was simply wrong about what was resting.

  `KindReduce` and `KindCancelAll` now exist in the log, and the interface
  documents the rule the omission broke: a mutating command missing from
  `CommandLog` is not "not yet logged", it is a book the log cannot reproduce.
  `CancelAllForUser` logs the *intent* rather than the ids it removed, which is
  what makes replay correct — the log is written before the sweep, so the same
  point in the command stream holds the same book and removes the same set.

- **Recovery restored the book and nothing else, so recovered orders were
  unreachable — and their fills went unreported.** The session layer's `ClOrdID` →
  order-id index started empty against a non-empty venue. A client could see its
  recovered orders in a `Query` reply and be told `2` (unknown order) for every
  `Cancel` or `Reduce` naming one, which are contradictory answers from the same
  venue about the same order.

  The severe consequence was quieter. With no record of the order, `publishTrade`
  found nothing to attribute a fill to and dropped the execution report entirely: a
  maker whose resting order filled while the venue was down would never have been
  told, and its position would have been wrong with no way to notice. That is
  precisely the failure the stream-outliving-the-connection design exists to
  prevent, reintroduced through recovery.

  `Registry.Adopt` and `Engine.RestingOrders` now rebuild the index on start.
  Adoption seeds from `RemainingQty`, not `Quantity` — a recovered order can be
  partly filled and `fill()` decrements from that number, so the original size would
  over-report `LeavesQty` by exactly what had already traded. It delivers nothing to
  any stream: those orders were acknowledged in a previous incarnation, and
  replaying them into a fresh sequence space would be inventing history.

- **`Reduce` bypassed the minimum resting time.** `Cancel` enforces it; `Reduce`
  did not. The control targets the Coscia pattern — post size, pull it before it
  can fill — and a reduce from 1000 lots to 1 withdraws 999 of them, so the whole
  pattern was available behind a different verb. That was nearly harmless while
  only an embedder could call `Reduce`; shipping it on the wire would have handed
  it to every authenticated client. Now refused with `ErrCancelTooSoon`, with the
  same exemptions `Cancel` has (replay, and privileged liquidation orders).

  **Behaviour change for embedders:** `Engine.Reduce` and `Runner.Reduce` now fail
  with `ErrCancelTooSoon` inside a configured `MinRestingTime`. Venues that leave
  the floor at zero — the default — see no change.

### Added

- **Reduce over the wire.** `Engine.Reduce` and the outbound `Replaced` that
  reports it both shipped in v0.10.0, but no inbound message could ever
  ask for one: a client's only route to a smaller order was cancel-then-new, which
  sends it to the back of its price level. That is the exact cost `Engine.Reduce`
  exists to avoid, and the capability was unreachable from the only place a client
  can speak.

  `Quantity` is the new **total**, not a delta. A delta cannot be made safe
  against a concurrent fill: the two sides would be subtracting from different
  numbers, and the result would depend on which the venue believed.

  Unlike a cancel, a refused reduce is reported. It fails for reasons the client
  caused and can correct — asking to grow, or to shrink below what is already
  filled — and silence is indistinguishable from a reduce still in flight. Zero is
  refused rather than treated as a cancel, because one message with two meanings
  is how a client ends up cancelling an order it meant to trim.

- `matching.Runner.TryReduceAsync`, which is the shape a network ingress needs and
  neither existing path had: the enqueue happens on the caller's goroutine so a
  reduce cannot overtake the order it names, while only the wait for the outcome
  moves off it so the matcher can never stall a connection's ingress. Only the
  error crosses the channel — the applied order is engine-owned.

- `ReasonInvalidQuantity` (16). Distinct from `Malformed` (14): malformed means the
  venue would not look at the message, this means it looked at a real order and the
  size asked for is not one it can take.

- `ReasonTooSoon` (17), the one refusal in the vocabulary a client should simply
  retry.

- `Engine.RestingOrders`, returning deep copies of every active resting order
  across all accounts. `OpenOrdersFor` could not serve recovery: it is scoped to one
  account, and a recovering venue does not know the accounts until it has read the
  book.

- `orderentry.Registry.Adopt`, which rebuilds the client-order-id index over a
  recovered book.

- **In-band reconciliation.** A `Query` message returns one `OpenOrder` per live
  order followed by a `QueryEnd`. Resume can legitimately fail — an evicted
  cursor or a restarted venue — and a client refused at login previously had no
  in-protocol way back to a correct picture; "reconcile out of band" is telling
  someone to build a second integration.

  The report is read from the book on the matching goroutine, and the publisher
  is drained before it is written, so every event up to that instant has already
  reached the client. `QueryEnd.Seq` names that point: everything after it is a
  change to apply on top. Reading the book without draining first would let an
  execution from before the read arrive after the report, and the client would
  apply it twice.

  `QueryEnd.Count` exists so a truncated report cannot look like a complete one —
  otherwise "you have nothing open" and "the connection died mid-report" are
  indistinguishable.

- `Engine.OpenOrdersFor` / `Runner.OpenOrdersFor`, returning deep copies read on
  the matching goroutine.

### Changed

- **Breaking (embedders):** `matching.CommandLog` gained `AppendReduce` and
  `AppendCancelAll`. `pkg/wal.Writer` implements both; a custom log will not
  compile until it does, which is the intended outcome — silently continuing to
  drop those commands is the bug being fixed.

### On the version

Four new message types this cycle, and **no version bump** — which is what the
type byte introduced in v0.11.0 bought. Every pre-existing golden vector is
byte-identical, and a test pins the eight original payload widths against values
derived by hand.

`Reduce` is the sharpest demonstration: it encodes to the same 30 bytes as
`Replaced`, with the same field at the same offset, and the two vectors differ in
exactly one byte — the type. Under v1's length-based dispatch they could not have
coexisted at all.

The protocol stays at **version 2**.

## [0.11.0] - 2026-07-29

A hostile re-read of v0.10.0, and the repairs. Everything below was found by
reviewing the new code the way the original critique reviewed the old — looking
for fields that exist but are never checked, constants declared and never used,
and claims the code does not back.

### Fixed

- **The protocol had no message type.** Eight `Msg*` constants were declared and
  never used; the server told Enter from Cancel by payload length. Any future
  message sharing a length with an existing one would have been silently misread
  as it, and a dead `switch payload[0]` block carried a comment describing a type
  byte that did not exist. Every payload now leads with an explicit type, both
  ends verify it, and the protocol is at **version 2** — which is what spending a
  version freeze is supposed to look like.
- **`Symbol` was decoded and thrown away.** The gateway built every order with
  its own configured instrument and never looked at the one the client sent, so
  an order naming any symbol was booked here. Now refused.
- **The reference server had no durability.** The library ships a write-ahead
  log, a checkpoint and a documented seam, and `cmd/obgw` wired up none of it —
  the one artifact showing people how to use this demonstrated running without
  it. Now `-wal`, `-snapshot` and `-checkpoint`, with group commit, recovery on
  start, and a startup warning when durability is off.
- **`NewServer` recovered from disk and discarded the result**, building its
  runner from a bare config. Added `matching.NewRunnerFor`, which takes an
  already-recovered engine, because `NewRunner` silently starting empty is the
  trap that caused it.
- **No timeouts anywhere.** No read deadline, no idle timeout, and
  `PacketServerHeartbt` was declared and never sent. Connect and say nothing and
  a goroutine, a buffer and a stream lived forever. Now a 10s unauthenticated
  login deadline, a 30s authenticated idle timeout refreshed by any packet, and a
  5s server heartbeat so a client can tell a quiet venue from a dead one.
- **`Publisher.Close` deadlocked when `Pump` had never run**, waiting forever on
  a goroutine that did not exist. A publisher built and closed without serving —
  an aborted startup — hung the process.
- **A typed-nil `CommandLog` segfaulted the matcher.** Assigning a nil
  `*wal.Writer` to the interface field yields a non-nil interface holding a nil
  pointer, so the `!= nil` guard passed and the first command dereferenced nil.
  Fixed at the call site and documented on the field, since the API invites it.

### Added

- `matching.NewRunnerFor` and `Engine.SetEventSink`. The latter exists because
  recovery must replay with no sink attached — otherwise restarting republishes a
  lifetime of historical executions at whoever connects next.
- Tests for every item above, plus one asserting the reason-code vocabulary
  defined in `internal/wire` and `pkg/orderentry` still agrees. It was duplicated
  across two packages with nothing checking it.

### Changed

- `docs/PROTOCOL.md` documents v2, the timeout and heartbeat regime, and the
  durability flags. It also states plainly that the golden vectors were generated
  by running the encoder: they prove the layout has not changed *accidentally*,
  which is a real job, but they do not prove it is correct. A ratchet, not a
  specification.

## [0.10.0] - 2026-07-29

Adds the network edge. v0.9.0 fixed what was broken; this release adds what was
absent — a client-facing order-entry protocol, a session layer, per-account
message sequencing with gap-free resume, and a reference gateway that speaks it
over TCP.

With this, *production-grade embeddable matching core with a demonstrated network
seam* is a claim the tests support. Both qualifiers are load-bearing:
production-grade describes the core, embeddable concedes the venue is not here.

### Added

- **`cmd/obgw`** — a reference TCP order-entry gateway. Authentication defaults to
  deny, so an unconfigured venue rejects everyone rather than admitting them.
  One goroutine per connection, admission control before enqueue, drain on
  SIGTERM.
- **`internal/wire`** — SoupBinTCP 3.00 framing carrying fixed-width big-endian
  payloads, no new dependencies. Frozen by 12 byte-exact golden vectors: a field
  that moves silently reinterprets every message a deployed client sends, so
  changing a layout means bumping `Version`, not editing a vector. Documented in
  [docs/PROTOCOL.md](docs/PROTOCOL.md).
- **`pkg/orderentry`** — per-account outbound streams that outlive connections.
  A `Session` is a socket; a `Stream` is an account's sequence. That separation
  is what makes resume possible: a maker whose resting order fills while its
  connection is down still has the execution waiting when it returns.
- **Gap-free resume**, scoped to a venue incarnation. A restart mints a new
  incarnation id, so a stale cursor is refused rather than served different
  content under numbers the client believes it already has. A cursor older than
  the retention ring is refused explicitly; the client is never told it is up to
  date when it is not.
- **`Engine.Reduce` / `Runner.Reduce`** — in-place down-size retaining queue
  position. The one order-entry operation a gateway provably cannot build
  outside the writer goroutine: cancel-then-new sends the order to the back of
  its level, which for a market maker managing size is a material loss. Size
  increases and price changes are rejected rather than silently reinterpreted,
  because an order that could grow in place would let a participant reserve a
  spot in the queue.
- **`matching.MultiSink`** and **`Runner.TryEnqueue` / `TryEnqueueCancel`**.
  Fire-and-forget submission never hands the engine-owned order back to a
  connection goroutine, which removes the whole read-after-submit race class.
- **`Gateway.Allow`** splits the admission decision from forwarding, so the gate
  can sit in front of `TryEnqueue`.

### Security

- The wire carries **no account and no engine order id**. Orders are named only
  by the client's own `ClOrdID`, scoped to the authenticated session. The engine
  cancels by `(orderID, userID)` and self-trade prevention lets one account
  observe another's resting orders, so a wire carrying either field would let a
  client name an order it does not own — there is no field in which to express
  it. Two accounts using the identical `ClOrdID` cannot reach each other's
  orders, and a test asserts it.
- `STPMode` and the privileged flag are absent from the wire: the first is venue
  policy, the second a liquidation capability that must never be client-settable.
- A reduce or cancel against another account's order is refused
  indistinguishably from a missing order, so a probe cannot learn that someone
  else's order exists.

### Fixed

- `Server.Close` deadlocked against its own connection handlers. Closing a
  listener stops new accepts and does nothing to established sockets, so the
  drain waited forever on handlers parked in a read. Found by the integration
  test on its first run, which is the reason the reference server exists.

### Changed

- README now claims *production-grade embeddable matching core with a
  demonstrated network seam*, and states plainly what is still yours: TLS,
  credential storage, multi-symbol routing, clearing, and any HA topology. The
  library ships the seams for primary-backup and deliberately not the consensus.
- **Production-readiness remains a property of a deployment, not of a library.**
  That sentence stays in the README permanently.

## [0.9.0] - 2026-07-29

A correctness release. A production-readiness audit of the recovery, event and
concurrency paths found that several controls the documentation marked as
shipped did not actually hold, and one execution path lost fills outright. Each
item below was reproduced with a failing test before it was fixed.

The headline is uncomfortable and worth stating plainly: the parts most likely
to hurt an embedder were not the missing pieces the README already disclosed,
but the pieces it claimed were finished.

### Fixed

- **Lost executions on OCO entry.** `ProcessOCO` called `submitStopInto` and
  discarded all three return values. A stop leg triggering on entry still
  settled through the book — filling and removing real makers and moving the
  last trade price — while reaching neither the event stream nor any
  `MatchResult`. The counterparty's fill was gone permanently. The same discard
  meant the stop leg was never announced, so its later fills referenced an order
  id no consumer had seen.
- **The event stream did not reconstruct the book**, despite `event.go` saying
  it did. Eight distinct defects: market and IOC remainders announced as
  `ACCEPTED` and terminated with no delete; iceberg refills re-adding a slice
  under the same id silently, so the owner of a live iceberg went dark after its
  first slice; the surviving OCO leg removed silently; STP `CANCEL_OLDEST` and
  `CANCEL_BOTH` removing the maker with no delete; STP `DECREMENT` shrinking
  both sides with no trade and no event at all; cascade-fired stops settling
  without ever being announced; and a rejected FOK publishing trades that
  `reverseTrade` had already undone. Emission is now composed in causal order
  per command, with sequence numbers assigned at publish time — numbering at
  record time produced a stream whose `Seq` ran backwards.
- **Snapshot restore was lossy in five ways.** Trailing stops vanished outright
  (they live only in the engine's map, never in either book); icebergs came back
  as a bare displayed slice with the reserve gone; OCO pairings were lost,
  leaving two independent orders either of which could fill; `markPrice` reset
  to zero, and both manipulation clamps skip when the current mark is zero, so
  the first post-recovery update was unconstrained; and the self-output
  guardrail's window reset, handing back a full budget immediately after a
  restart.
- **The client-order-id duplicate guard was empty after recovery**, on both the
  snapshot and the replay path. It stopped enforcing precisely when a client is
  most likely to resend — after the venue restarts — which is the FIX PossDup
  case it exists to cover.
- **Snapshot and log could not be joined.** `EngineSnapshot` carried no log
  position, and `INTEGRATION.md` told callers to replay entries after `Seq` —
  the engine's *order* sequence, unrelated to log positions. Following the
  documented recipe replayed an arbitrary slice of the log.
- **Replay bypassed the deterministic admission checks**, including the int64
  notional overflow guard. Because the log is write-ahead it records commands as
  submitted, not accepted, so an order the live engine rejected rested on the
  recovered book.
- **`WriteSnapshot` was atomic but not durable** — no fsync of either the file
  or the parent directory, so a crash could leave a correctly-named empty
  snapshot that `Recover` would load.
- **`Runner.Close` panicked in-flight producers** by closing the shared command
  queue from the consumer side. Correct shutdown required proving every producer
  had stopped, which a server with a goroutine per connection cannot do.
- **`pkg/gateway` data-raced** at the topology its own package doc recommended.

### Added

- `wal.Checkpoint`, `wal.Recover` and `wal.RestoreAfter` — the snapshot/log join
  expressed once, in code, instead of as prose for callers to reimplement.
- `Runner.Checkpoint` and `RunnerConfig.Log`: a write-ahead seam that logs each
  mutating command before applying it, tracking the sequence of the last command
  *applied* rather than appended.
- `Engine.CancelAllForUser` / `Runner.CancelAllForUser` — the operator kill
  switch. It pulls resting orders, pending stops and trailing stops, ignores
  `MinRestingTime`, and announces every removal.
- `matching.ErrShuttingDown` and an idempotent, fenced `Runner.Close`.
- `EventReplaced` now carries in-place size changes that keep queue position.
- Crash-recovery property test over a 2,000-command generated tape, replacing a
  determinism gate that ran on six hand-written orders.
- `TestEventStreamReconstructsBook`: 22 scenarios asserting the event stream
  rebuilds the engine's own L3 book, order-for-order and lot-for-lot.
- Benchmarks for the durable path (`Runner` + `EventSink` + WAL), which had no
  published number while the README advertised the bare engine's.

### Changed

- **Docs corrected where they overclaimed.** `THREAT-MODEL.md` row 16 no longer
  marks the taker speed bump as an enforcing control — it is an observation hook
  that reports and does not delay. The README no longer implies the
  zero-allocation figure applies to the concurrent API: `Match` into a caller
  buffer allocates nothing, `Runner.Process` allocates 4/op, and durability adds
  roughly an order of magnitude.

## [0.8.0] - 2026-07-26

Completes the microstructure research agenda. All four roadmap items are now
implemented, measured, and written up in [docs/research/](docs/research) — and
most of the popular claims they test do not survive contact with ground truth.

### Added

- **Order-flow study** (research-roadmap.md §4), completing the research agenda:
  `signals.CVD`, `signals.TickRuleSide` / `signals.LeeReadySide` (aggressor
  inference, there to be measured against ground truth rather than used in place
  of it), `signals.AbsorptionConfig`, `signals.Divergence`, and
  `signals.WilsonInterval`; `study.RunInference`, `study.RunDivergence` (with a
  price-only control arm), `study.RunAbsorption`, `study.RunSqueezeDemo`, and
  `study.PoolSignals`; `cmd/flowstudy` runs them all. Write-up:
  [order-flow.md](docs/research/order-flow.md) — a 94.5%-accurate tick rule
  builds a CVD wrong by 169% (sometimes with the opposite sign); CVD divergence
  beats its base rate but loses to a price-only control, so the CVD half adds
  nothing; absorption predicts nothing, though the mechanism is demonstrably
  real.

## [0.7.0] - 2026-07-26

The research release: the microstructure agenda's first two items measured,
written up, and checked in — plus an honest account of what data the harness
runs on.

### Added

- **Kyle's λ price-impact study** (research-roadmap.md §2): `signals.SignedFlow`
  and `signals.EstimateLambda` (fits `ΔP = λ·y`, reporting λ with R² and N);
  `study.RunKyleLambda` (the λ that emerges from a real book, with none
  configured), `study.RunKyleDepth` (λ ∝ 1/depth sweep), `study.RunExecution`
  (block vs sliced execution, scored on implementation shortfall plus realized
  and permanent impact), and `study.RunLambdaCalibration` (estimator validation);
  `cmd/lambdastudy` runs them all.
- **`docs/research/`**, the results directory the methodology has always
  required, with two write-ups.
  [kyle-lambda.md](docs/research/kyle-lambda.md): λ ∝ 1/depth holds, slicing is
  ~8% cheaper per lot and completes where a block leaves ~8% unfilled, and
  permanent impact is unchanged either way — slicing buys back the temporary
  component, not the permanent one. [ofi.md](docs/research/ofi.md): the §1
  result written up properly over ten seeds, including that the predictive slope
  is negative in nine of them.
- **`docs/research-roadmap.md` §0 "Data and scope"**: what the engine emits
  (per-order L3/MBO event streams), what simulator ground truth adds, what is
  missing (real capture is L2-only), and why "L4" is a vendor label rather than
  an exchange tier.

### Fixed

- **Overstated OFI figure in the docs.** `LEARN.md` claimed a contemporaneous
  R² ≈ 0.33 and `cmd/ofistudy` called it "~a third" of the same-interval move.
  The measured mean across ten seeds is **0.1685** (range 0.0704–0.2397); the
  three seeds the CLI printed were the joint highest of the first ten. Both
  numbers corrected and the CLI widened to ten seeds. The verdict is unchanged —
  predictive R² was always ~0.0003.

## [0.6.0] - 2026-07-23

The market-integrity release: a research-grounded threat model
([docs/THREAT-MODEL.md](docs/THREAT-MODEL.md)) and the defenses it prioritized,
plus durable persistence.

### Added

- **Durable WAL** (`pkg/wal`): append-only, length-prefixed command log written
  write-ahead, torn-tail-safe `Restore` into a fresh engine, and atomic snapshot
  write/read — the LMAX/Binance journal-plus-snapshot recovery model.
- **Threat model** (`docs/THREAT-MODEL.md`): a 27-attack catalogue, top-ten deep
  dives, a what-we-defend inventory, and a prioritized roadmap — every entry tied
  to a real enforcement action or incident.
- **In-core pre-trade risk & anti-manipulation controls** (`matching.Config`,
  opt-in, cold-path, `Privileged`-exempt, bypassed on deterministic replay):
  `MaxOrderQty` / `MaxOrderNotional` (fat-finger) and `MinOrderQty` /
  `MinOrderNotional` (dust) caps with an int64 notional-overflow guard;
  `MaxOrdersPerAccount`; `MinRestingTime` (anti-spoofing); `DedupClientOrderIDs`
  (idempotency); `MaxMarkStep` **and** `MinMarkDepth` / `MarkDepthBand` (anti
  oracle-pump — single jump and patient drag); `MaxForceTradeQty` (chunked
  liquidation); `BandBreachPause` (timed halt + auto-resume); `IcebergPeakJitter`
  (anti-sniffing). `HALTED` / `RESUMED` events on guardrail trips and pauses.
- **Surveillance detectors** (`pkg/surveillance`, alert-only): `OTRDetector`
  (order-to-trade ratio), `CloseMarkingDetector`, `RampingDetector`,
  `PingingDetector`, and `CrossBookMonitor` (cross-product correlation), alongside
  the existing spoof and rate detectors.
- **Call-auction session** (`pkg/auction`): `AuctionSession` for the open, close,
  and halt recovery, with a replay-safe `RandomizedClose` that defeats marking the
  close.
- **Edge gateway** (`pkg/gateway`): an enforcing token-bucket `RateGate` (rejects
  over-quota orders; cancels never gated) and an asymmetric taker speed bump, with
  `examples/gateway` demonstrating them plus a CAT-style audit export.
- `OrderBook.OrdersByUser` and `OrderBook.DepthWithin`; `Engine.SetReplaying`.
- Docs: `docs/INTEGRATION.md` "Market integrity & pre-trade risk" section; every
  new knob in `docs/CONFIG.md`; refreshed `docs/SPEC.md` package layout and
  market-integrity section; README highlights and docs table.

### Changed

- **BREAKING:** `Engine.SetMarkPrice(price int64)` now returns `error` (it rejects
  a mark update that violates `MaxMarkStep` / `MinMarkDepth`, or a negative price).
  `Runner.SetMarkPrice` is unchanged (async, fire-and-forget).
- `EngineSnapshot` gained `PausedUntil` so a mid-pause snapshot restores exactly.

## [0.5.0] - 2026-07-23

Phase C — real-world features. Self-trade prevention with taker-decides,
`DECREMENT` mode, cross-account `TradeGroupID`, and a `Privileged` exemption; a
mark/index-driven price band (`SetMarkPrice`) plus a `ForceTrade` liquidation/ADL
primitive; a per-symbol `Shards` router; an event-stream adapter example
(`examples/eventfeed`); and a uniform-price batch-auction mode (`auction.BatchAuction`).

## [0.4.0] - 2026-07-22

Determinism & integration seam. **Phase A:** an injectable `Clock` (byte-identical
replay), replay-equivalence and zero-allocation CI gates, feature-flagged exotic
order types (`DisabledClasses`), degraded states (`Open` / `CancelOnly` / `Halted`),
and a self-output `Guardrail`. **Phase B:** a monotonic `Event.Seq` + typed
`EventSink` event stream, `TakeSnapshot` / `RestoreEngine`, and bounded
backpressure (`TrySubmit` → `ErrQueueFull`).

## [0.3.0] - 2026-07-22

Production-grade low-µs core (P0–P6). O(1) cancel via intrusive linked lists, a
zero-allocation `Match` path (pooled nodes/levels + caller trade buffer), and a
single-writer `Runner` (MPSC command queue, lock-free hot path). Tail-latency,
fuzz, soak, and WAL-replay-recovery suites.

## [0.2.0] - 2026-07-22

**BREAKING:** integer-exact pricing. Prices are `int64` ticks and quantities
`int64` lots; a per-symbol `Instrument` converts decimals only at the boundary.
Engine-assigned monotonic `int64` ids replace UUIDs.

## [0.1.0] - 2026-07-21

Initial release: a decimal-first CLOB and matching engine with the full order
surface (limit, market, stop/stop-limit, iceberg, post-only, pegged, OCO,
trailing), GTC/IOC/FOK, self-trade prevention, a price-band circuit breaker, FIFO
and pro-rata allocation, L1/L2/L3 market data, a surveillance starter kit, and a
market-microstructure research harness with a WebAssembly demo.

[Unreleased]: https://github.com/intrepidkarthi/orderbook/compare/v0.26.0...HEAD
[0.26.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.25.0...v0.26.0
[0.25.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.24.0...v0.25.0
[0.24.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.21.0...v0.22.0
[0.21.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/intrepidkarthi/orderbook/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/intrepidkarthi/orderbook/releases/tag/v0.1.0

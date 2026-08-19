# Iceberg Admission — A Fat-Finger Cap A Client Can Switch Off By Choosing An Order Type

Status: **built** — §13 is what building it changed, including three places this
document was wrong. The spec was written before the code, as this repository does it. §1 is an audit that had to run before anything could be decided, and
its result is what bounds the rest: **five admission checks measure the wrong quantity,
the two pins found two of them, and four further consumers of the same mutation are
wrong in ways this slice deliberately does not touch.** §3, §4, §5 and §6 each end in a
decision; §10 is how a reviewer checks it was carried out; §11 is the list of sabotages
that must be red before it counts; §12 is how it can still be wrong. Every number below
was measured against `main` with a probe built outside the repository and discarded ·
Author: Karthikeyan NG · Last updated: 2026-08-19

> **The measurement that decides this.** `Config.MaxOrderQty = 5` is the venue's
> fat-finger cap: the largest single order it will accept. One client, one price, one
> quantity, submitted two ways:
>
> ```
> sell 9 @100, plain              REJECTED   order quantity exceeds the configured maximum
> sell 9 @100, iceberg showing 3  ACCEPTED   rests 3 at the offer, 6 in reserve
> ```
>
> The cap is evaded by exactly the hidden quantity — the one quantity the venue cannot
> see. It is not a fault of the iceberg mechanism; it is `checkOrderCaps` reading
> `order.Quantity` after `types.NewIcebergOrder` has overwritten it with the display
> size. The same reading refuses a **90-lot** iceberg shown 3 for being below a 5-lot
> dust floor it is eighteen times above.
>
> And the same reading is what lets this rest on the book:
>
> ```
> sell 184,467,440,737,095,517 @100, plain              REJECTED   notional overflows int64
> sell 184,467,440,737,095,517 @100, iceberg showing 3  ACCEPTED   3 at the offer
> ```
>
> That last one is not a configurable policy. It is the arithmetic invariant
> `checkOrderCaps`'s own comment sets apart from ingress policy — *"a corrupt or
> hand-edited log entry must not be able to replay a notional that wraps int64 into the
> book"* — and it is evaded by the same three characters.

Companion documents:
- [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §1.3 and §8 — where both pinned
  defects were measured and deliberately deferred, with the reason: *"should an
  iceberg's cap be its total or its slice"* is a venue-policy argument with its own
  answer to justify. This document is that answer. §2 is the constructor mutation,
  stated once, and this document does not restate it.
- [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §9 — the bullet that declined this first, and
  §6.1's finding that `internal/semcheck` was blind to a change in exactly this area.
  §7 below is that finding recurring, in the same corpus, for the same reason.
- [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §2.1, §1.2 and §5.4 — the definition §7
  applies, the registry row this slice adds, and outcome 4, which refuses a bump the
  corpus cannot justify.
- [`THREAT-MODEL.md`](THREAT-MODEL.md) §5 row 6 and §3.5 — `MaxOrderQty` /
  `MaxOrderNotional` are the fat-finger reject, backed by the Knight Capital / SEC Rule
  15c3-5 enforcement case. §3.5 is the row about a crafted exotic reaching a path
  nobody checked. This defect sits exactly on the seam between the two and belongs in
  both.
- [`COMPATIBILITY.md`](COMPATIBILITY.md) §"The rule" — no exported signature moves; what
  moves is which orders a venue accepts, which is the semantics number's business and
  not the API surface's.
- [`TESTING.md`](TESTING.md) §"The rule" — a test does not count until it has been run
  against code broken the way it claims to detect. §11 is this slice's list.

---

## 1. The audit: every consumer of `order.Quantity` an iceberg reaches

This ran first, before any of §3–§6 was decided, because "fix the two caps the pins
found" and "everything downstream of the constructor sees a slice" are different slices,
and only the audit says which one this is.

The audit's second question is the one that keeps a fix from becoming a leak: **for each
consumer, is seeing the slice wrong, or is it right?** A market-data feed *should*
publish the visible size. A depth aggregate that started reporting an iceberg's total
would announce the reserve to every subscriber, which is the property the order type
exists to provide. Confusing "reads the slice" with "reads the wrong thing" is how a fix
to an admission control becomes a hidden-liquidity leak, so every row below carries a
verdict and not just a reading.

### 1.1 What was checked, and how

Every non-test read of `order.Quantity`, `Order.NotionalValue`, and every risk or limit
decision in `pkg/gateway`, `cmd/obgw`, `pkg/surveillance`, `pkg/marketdata` and
`pkg/orderentry` that takes a quantity from an order rather than from a trade. For each,
the same order was submitted twice — plain, and as an iceberg of the same total — and
the two outcomes compared. Reading the code was used only to explain a difference, never
to conclude there was not one: `MaxOrdersPerAccount` and the price band both *look* like
they might be quantity-sensitive and are not, and the notional overflow guard looks like
an invariant that could not be a policy hole and is one.

### 1.2 Results

Measured on `main`, price 100, iceberg total 9 or 90, display 3.

| Consumer | Sees | Verdict | Measured |
|---|---|---|---|
| `checkOrderCaps` → `MaxOrderQty` | slice | **WRONG** | cap 5: plain 9 `REJECTED`, iceberg 9×3 `NEW`, reserve 6 |
| `checkOrderCaps` → `MinOrderQty` | slice | **WRONG** | floor 5: plain 90 `NEW`, iceberg 90×3 `REJECTED:ORDER_BELOW_MIN_QTY` |
| `checkOrderCaps` → `MaxOrderNotional` | price × slice | **WRONG** | cap 500: plain 9 `REJECTED`, iceberg 9×3 `NEW`, reserve 6 |
| `checkOrderCaps` → `MinOrderNotional` | price × slice | **WRONG** | floor 500: plain 90 `NEW`, iceberg 90×3 `REJECTED:ORDER_BELOW_MIN_NOTIONAL` |
| `checkOrderCaps` → `checkedMul` overflow guard | price × slice | **WRONG** | `1.84e17` lots @100: plain `REJECTED:NOTIONAL_OVERFLOW`, iceberg `NEW`, resting |
| `checkOrderCaps` → `MaxOrdersPerAccount` | nothing — a count of resting orders | **CORRECT** | cap 2, a 90-lot iceberg resting: 2nd order `NEW`, 3rd `REJECTED:TOO_MANY_ORDERS`. An iceberg is one order however many slices it shows |
| `checkOrderCaps` → `isDuplicate` | nothing — `(user, ClOrdID)` | correct on quantity, **broken for another reason** (§1.4) | — |
| `outsideBand` / the price collar | nothing — a price | **CORRECT** | band ±10%, last 100: iceberg at 150 `REJECTED:PRICE_OUTSIDE_BAND`, identically to a plain order |
| `Guardrail` (`executeTrade`) | trade price × trade qty | **CORRECT** | `MaxNotional 400`: draining a 9×3 iceberg trips to `HALTED`. It guards the engine's *output*, and an iceberg's output is its prints |
| `pkg/marketdata` `L2Feed.remember` | `RemainingQty` of the slice | **CORRECT, and must stay** | L2 snapshot of a 9×3 iceberg: `[{100 3}]`. Publishing 9 here would announce the reserve to every subscriber and delete the order type |
| `pkg/marketdata` `Feed` trade prints | trade quantity | **CORRECT** | prints are what happened, not what is hidden |
| `pkg/gateway` `RateGate` / `Gateway.Allow` | nothing — account identity and, for the speed bump, price | **CORRECT** | the token bucket is per message; `IsTaker` compares prices. Neither reads a quantity |
| `cmd/obgw` `enterIceberg` / `buildOrder` | the wire total, before the constructor runs | **CORRECT** | `wire.EnterIceberg` carries the total and `DisplayQty` separately; the gateway refuses a display size that is zero, negative or larger than the total, and does so *before* `NewIcebergOrder` |
| `pkg/surveillance` `SpoofDetector` (`MinSize`) | whatever its adapter passes | **CORRECT in intent, unreachable in tree** | a spoof is bait that is *displayed* and pulled, so the slice is the right measure. No in-tree adapter feeds an iceberg: `cmd/surveil` is synthetic and `cmd/obwasm` has no iceberg path |
| `pkg/surveillance` `PingingDetector` (`MaxSize`) | whatever its adapter passes | **arguable, unreachable in tree** | a 90-lot iceberg showing 1 would be counted as a tiny order. Alert-only, no in-tree adapter, and §9 leaves it |
| `pkg/orderentry` `Registry.Publish` / `track` | slice, on the **private** stream | **wrong report, no leak** (§1.4) | `KindAccepted` tells the owner `Quantity: 3` for an order it sent as 9, and `LeavesQty` tracks the slice. Delivered only to `e.Order.UserID`, who already knows the total |
| `pkg/wal` `AppendIceberg` | `ib.TotalRemaining()` | **CORRECT, already fixed** | the previous slice; the record states the total and `TotalQty` witnesses it |
| `matching.LoadSnapshot` | `IcebergEntry.Hidden` directly | **CORRECT** | rebuilds the wrapper without calling `NewIcebergOrder`, so no mutation happens and no admission runs (§6.3) |

**So the answer is five, not two.** The pins found the two configured quantity controls.
The two configured notional controls are the same defect in the same function on the
next four lines, and the notional overflow guard — the one check `checkOrderCaps`'s doc
comment explicitly sets apart as *"an arithmetic invariant rather than ingress
policy"* — is the third pair's third member and the most surprising row in the table.

Everything outside `pkg/matching` that reads a quantity from an iceberg either reads a
**trade** (correct by construction), reads the **displayed** slice on purpose (correct,
and a fix that changed it would be a leak), or reads a quantity for something that is
not a quantity decision (an account's order count, a price test, a token bucket).

### 1.3 The third defect in `checkOrderCaps`, which no pin covers and which the fix repairs

Found while establishing that the caps run on refills, and it is the sharpest thing in
this audit because it destroys a client's quantity rather than merely misjudging it.

`ProcessIceberg` settles the entry slice and then, while that slice keeps fully
crossing, refills and calls `settleInto` again (`engine.go:1362-1367`). Each of those
re-entries runs `checkOrderCaps` **against the refilled slice**, and the loop discards
the result: `dst, _, _ = e.settleInto(ib.Order, dst)`.

Measured, `MinOrderQty = 2`, 9 lots of resting depth, an aggressive iceberg of 10 shown 3
— slices 3, 3, 3, 1:

```
result status  FILLED      trades 9 lots
ib.Hidden      0
ib.Order       RemainingQty 1, Status REJECTED
book           empty
```

The tenth lot was refused for being below the dust floor, the refusal was thrown away,
the slice never rested, no event was published, and the client was told `FILLED`. One
lot of a client's order evaporated inside the engine.

The **maker-side** refill does not do this. When a resting iceberg's slice is consumed
by someone else's order, `match()` refills and calls `e.book.Add(ib.Order)` directly
(`engine.go:1471-1477`) — no admission, ever. Measured on the same configuration, a
resting 10×3 iceberg works off all ten lots and the final 1-lot slice rests happily
below the floor.

**So the two refill paths disagree today, and the taker-side one is wrong.** This is not
a separate defect to schedule; it is the same question — *what is admission measuring?*
— answered inconsistently in two places, and §4.4 is where this slice answers it once.

### 1.4 Four consumers of the same mutation that are wrong for reasons this slice does not fix

Named, because a measured finding left unnamed is how it gets found a third time
([`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §1.3's own reason for pinning these
two).

- **`Engine.Reduce` reduces the slice and calls it the order.** `Reduce` guards on
  `newQty >= order.Quantity` (`engine.go:2208`), which for a 9-lot iceberg showing 3 is a
  guard against 3. Measured: `Reduce(id, 5)` on a 9-lot iceberg returns
  `ErrInvalidQuantity` — the client cannot reduce nine lots to five. `Reduce(id, 2)`
  succeeds and leaves the client with **8 lots working**: slice 2, reserve 6. A client
  that asked to be smaller got a number it did not choose. This is the same cause and a
  different verb, and the fix is a different design (does reducing an iceberg consume the
  reserve first, or the slice first?) with its own answer to justify.
- **Self-trade prevention under `DECREMENT` strands the reserve.** `decrement` shrinks
  both orders by the overlap; when the maker's `RemainingQty` reaches zero, `match()`
  removes it and emits a cancel — and unlike the `IsFilled` branch four lines below,
  **it does not refill**. Measured: a 9×3 iceberg meeting the same account's 3-lot buy
  under `STPDecrement` ends with the book empty, no trade, and `ib.Hidden == 6`. A
  stranger's later buy of 6 gets nothing. The reserve is durable, too: `TakeSnapshot`
  writes `IcebergEntry{Hidden: 6}` for an order that is not resting, and `LoadSnapshot`
  refuses that snapshot with *"iceberg N has no resting displayed slice"*. Six lots of a
  client's order are destroyed by an unrelated command, which is the family
  [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §2 is about, and it belongs in that document's
  successor rather than in an admission slice.
  **§13.4's registry cleanup does NOT reach this**, and that is deliberate — measured
  after the fix: `Hidden=6`, `registry=1`, `LoadSnapshot: iceberg 1 has no resting
  displayed slice`, unchanged. The cleanup runs in `ProcessIceberg`, and `decrement`
  removes the maker from inside `match()` long after that command returned. Making the
  snapshot loadable here would be the WRONG fix: it would silently discard the six lots
  instead of failing loudly about them, and the defect is the missing refill, not the
  registry entry. Loud is better until the refill is fixed.
- **An iceberg's `ClientOrderID` is never recorded for de-duplication.**
  `recordClientOrderID` is called from `Match` (`engine.go:714`), and `ProcessIceberg`
  calls `settleInto` directly, bypassing it — as do `ProcessStop`, `ProcessOCO`,
  `ProcessPegged` and `ProcessTrailingStop`. Measured with `DedupClientOrderIDs = 8`: an
  iceberg with `ClOrdID "X1"` followed by a plain order with `ClOrdID "X1"` is `NEW`. The
  replay double-book guard [`THREAT-MODEL.md`](THREAT-MODEL.md) §4 credits covers plain
  orders only. It is a real gap, it is not a quantity gap, and putting it in this slice
  would mean bumping the semantics number for two unrelated reasons at once.
- **`ProcessIceberg` reports `FILLED` for an order that is still working.** Measured:
  `MinOrderQty = 3`, 3 lots of depth, an aggressive iceberg of 10 shown 3 → `FILLED`,
  3 lots traded, 4 in reserve, 3 resting at the offer. `status` comes from the entry
  slice's settle and is never updated by the refill loop except when the whole order
  finishes. A status defect, not a quantity defect; noted because it was measured on the
  way to §1.3 and because it makes §1.3's evaporated lot harder to notice.

Also corrected on the way past, because it is a one-line documentation defect that would
mislead the next reader of exactly this code: **`Config`'s comment at `engine.go:213-217`
says the admission controls "are bypassed on deterministic replay". They are not**, and
`SetReplaying`'s own doc comment thirty lines earlier says at length that they are not.
§6.4.

### 1.5 The scope decision, stated rather than left to omission

This slice fixes **what admission measures**, for the five checks in §1.2 marked WRONG,
and nothing else. It does not generalise to a "wrapper orders carry their client total
everywhere" mechanism, because the audit shows there is no second wrapper with this
problem: `NewIcebergOrder` is the only constructor in `pkg/types` that mutates the order
it is handed, and `pkg/wal`'s `TestNoOrderWrapperConstructorMutatesItsOrder` already
holds that as a named exception. The four findings in §1.4 are separate slices with
separate arguments, and §9 says so again where a reader will look for it.

## 2. The defect, once

`types.NewIcebergOrder` (`pkg/types/iceberg.go:34-41`) takes an order whose `Quantity`
is the client's total, computes `hidden = Quantity - displayQty`, **overwrites
`Quantity` and `RemainingQty` with the display size**, and puts the remainder on the
wrapper. `Engine.checkOrderCaps` (`engine.go:543`) then reads `order.Quantity`.

The wrapper is not lost — `ProcessIceberg` is holding it — but `checkOrderCaps` is
called from inside `settleInto`, which has only the `*types.Order`. By the time the
venue's fat-finger cap gets to look at the order, the one number it exists to bound has
been replaced by a number the client chose precisely because it is small.

This is the same cause as the durability defect fixed in the previous slice, and the
third consumer that audit named. It differs from that one in a way that decides §7: the
journal defect was a **translation** error between bytes and commands, with the engine
executing a command the journal had corrupted. This one is the **engine's own verdict**
on a command that arrived intact.

## 3. Decision 1 — what quantity admission measures

### 3.1 The rule, stated once

> *Rule 1 — the per-order size and notional controls measure the quantity the CLIENT's
> command puts to work, not the part of it the venue displays.*

For every order type but one, that quantity is `Order.Quantity` and nothing changes. For
an iceberg it is `DisplayQty + Hidden` at the instant of submission — which is
`ib.TotalRemaining()`, and which is exactly the number `Writer.AppendIceberg` already
journals for the same reason, one layer down. The two layers now agree on what the
command was, which they did not before.

*Reason:* a fat-finger cap answers "how much can one instruction move". Hiding size
changes who can see the instruction; it does not change how much it moves. A venue that
caps single orders at 5 lots and accepts 9 has not applied its cap, and the
[`THREAT-MODEL.md`](THREAT-MODEL.md) §5 row 6 case behind that cap — Knight Capital,
SEC Rule 15c3-5 — is about quantity leaving the venue, not about quantity on a screen.

*Why it will look like a bug:* an order that shows 3 is refused by a cap of 5. The book
never displays anything the cap would have refused, and the client can see that it does
not, so the refusal appears to be measuring something that is not there. It is: the
reserve is a live obligation the venue has accepted, and admission is the only place it
is ever weighed.

### 3.2 Applied to all five, symmetrically

The pins cover the first two rows. Shipping those two alone would leave the notional
pair inconsistent with the quantity pair *in the same function, four lines apart*, and
that inconsistency is invisible: a venue that sets `MaxOrderQty` and `MaxOrderNotional`
together would find one enforced and one not, with nothing in any message or event to
say which. A partial fix here is worse than the defect, because the defect is at least
uniform.

| Control | Measures today | Must measure | Error today |
|---|---|---|---|
| `MinOrderQty` | `order.Quantity` (slice) | the client's total | refuses orders far **above** the floor |
| `MaxOrderQty` | `order.Quantity` (slice) | the client's total | admits orders far **above** the cap |
| `MinOrderNotional` | `Price ×` slice | `Price ×` the client's total | refuses |
| `MaxOrderNotional` | `Price ×` slice | `Price ×` the client's total | admits |
| int64 notional overflow (`checkedMul`) | `Price ×` slice | `Price ×` the client's total | admits an order whose notional cannot be represented |

The overflow guard moves with them and is not optional. It has no `Config` knob to turn
off, `Privileged` orders do not bypass it, and its doc comment already claims it is an
invariant rather than a policy. An invariant that a client can step around by choosing
an order type is not an invariant, and leaving it on the slice while the four policies
move to the total would be the one asymmetry no operator could configure away.

### 3.3 The two that must not change, and why they are in this table at all

- **`MaxOrdersPerAccount` counts orders, not lots.** An iceberg is one order that shows
  itself many times; each refill re-adds the *same* order id to the book. Measured: with
  the cap at 2 and a 90-lot iceberg resting, the account's second order is accepted and
  its third refused. Making it count slices would refuse a client for the venue's own
  refill, and making it count lots would be a different control with a different name.
  It is listed because a reviewer skimming §3.2 will ask why the fifth check in the same
  function did not move, and "it was never measuring a quantity" is the answer.
- **The price band tests a price.** Measured: a 9×3 iceberg priced at 150 with the last
  trade at 100 and a ±10% band is `REJECTED:PRICE_OUTSIDE_BAND`, identically to a plain
  order. Nothing to do.

### 3.4 The alternative rules, and why each is refused

- **"An iceberg's cap is its slice, and that is deliberate."** This is a real venue
  policy — the argument is that a cap exists to bound market impact and only displayed
  size has impact. It is refused on two grounds. First, it is not what the code says: the
  slice reading is an accident of a constructor's mutation, not a decision anybody wrote
  down, and `AppendIceberg`'s doc comment claimed the opposite for four releases. Second,
  it makes the cap *client-controlled*: any client wanting to exceed `MaxOrderQty` sets
  `displayQty = MaxOrderQty` and the cap is gone. A control whose subject chooses whether
  it applies is not a control.
- **"Cap the slice AND the total, with two knobs."** That is §5's question, and the
  answer there is that the second knob is a new control rather than a repair. Bundling it
  here would put a new venue policy inside a bug fix, and its lines in the fingerprint
  golden would be indistinguishable from the fix's.
- **"Measure `TotalRemaining()` continuously, so the cap tracks the working size."** This
  is the rule the obvious implementation falls into (§4.3) and it is wrong in a way that
  only shows up late: `TotalRemaining` shrinks as the order fills, so `MinOrderQty` would
  start refusing the tail of an order it admitted. Admission is a judgement on a command
  at the instant it arrives; re-judging it later against a smaller number is §1.3's
  defect with a bigger blast radius.
- **"Fix it in `NewIcebergOrder` by not shrinking `Quantity`."** It removes the cause and
  breaks everything: the slice is what rests, and an order whose `Quantity` is 9 rests 9
  at the offer. This is the mutation §11 row 9 runs, and `TestIceberg_ShowsOnlyDisplaySlice`
  is what must catch it.

## 4. Decision 2 — how `checkOrderCaps` learns the total

`checkOrderCaps` is called from two places inside `settleInto`, which has only an
`*types.Order`. `ProcessIceberg` holds the wrapper. There are four ways to bridge that,
and one of them is the one the task asks to be argued rather than assumed.

### 4.1 The four candidates

| Candidate | How the total travels | Why not |
|---|---|---|
| **A. A field on `types.Order`** | `NewIcebergOrder` writes the client's total to a new `Order` field; `checkOrderCaps` prefers it | Puts an iceberg concept on the base type every order carries. Worse, `Reduce` and `decrement` mutate `Quantity` and would not mutate the field, so the two drift silently. And `AppendIceberg` copies `*ib.Order` into the record, so the field lands in the journal beside `TotalQty` — a third statement of the same number, and `TestAPostFixIcebergRecordCostsFourteenBytesAndNothingElse` fails |
| **B. A lookup in `e.icebergOrders`** | `checkOrderCaps` probes the registry by `order.ID` | §4.3 |
| **C. Engine-scoped state set around the call** | `ProcessIceberg` sets `e.admitQty` before settling and clears it after | Hidden state across a re-entrant call. `cascadeStops` calls `settleInto` again from inside a walk, so the field would have to be saved and restored, and a fix whose correctness depends on nobody adding a third re-entry is not a fix |
| **D. An explicit parameter** | `settleInto` gains an admission quantity; the caller that knows passes it | **Taken.** §4.2 |

### 4.2 The rule

> *Rule 2 — the quantity admission measures is a PARAMETER of the settle, supplied by
> the command entry point that knows what the client sent.*

Concretely, and this is the whole mechanism:

```go
// settleAdmitting settles order, admitting it against admitQty — the quantity the
// CLIENT's command puts to work. It differs from order.Quantity for exactly one
// order type: an iceberg, whose visible slice is a fraction of what was submitted.
//
// admitQty <= 0 means this settle is NOT a client submission (an iceberg refill),
// and the ingress controls do not run again. See §4.4.
func (e *Engine) settleAdmitting(order *types.Order, admitQty int64, dst []types.Trade) (...)

// settleInto settles an order whose own Quantity IS what the client submitted —
// every order type except the iceberg.
func (e *Engine) settleInto(order *types.Order, dst []types.Trade) (...) {
	return e.settleAdmitting(order, order.Quantity, dst)
}
```

> **The `admitQty <= 0` line above is WRONG, and it was built twice before that was
> measured. §13.6 has the correction and the numbers; what shipped is a separate
> `settleRefill` method, because "this settle is not a client submission" cannot be
> encoded as a value of the quantity — there is no int64 an order cannot carry. The
> sketch is left as written so §13.6 has something to correct.**

`checkOrderCaps` takes the same value and applies it to all five checks in §3.2.
`ProcessIceberg` computes `total := ib.TotalRemaining()` **before** the first settle —
at that instant, and only at that instant, it equals the client's total — and passes it.

Eight of the nine `settleInto` call sites are untouched, which is the point: the default
is stated once, in one function, and every caller that has nothing special to say
continues to say nothing. The one caller that does say something says it at the call
site, in a named argument, where the next reader of `ProcessIceberg` will see it.

*Reason:* the total is data the caller has and the callee needs, and a parameter is what
that is. It has no ordering dependency, no hidden state, no re-entrancy hazard, no
lookup on a path every order takes, and it survives a refactor that moves the registry
insert — because there is nothing to move.

*Why it will look like a bug:* `settleInto` and `settleAdmitting` differ by one
argument, and a reviewer will ask why there are two. Because eight callers would
otherwise repeat `order.Quantity` eight times, and the ninth's difference would be
invisible in the noise.

### 4.3 Why the registry lookup is refused, and which objections are not the reason

Candidate B — `if ib, ok := e.icebergOrders[order.ID]; ok { qty = ib.TotalRemaining() }`
— is the shortest patch and it is refused. The objections in the order they matter:

1. **It answers the wrong question.** The map says *"this order is an iceberg"*. What
   `checkOrderCaps` needs to know is *"is this settle a client submission"*. Entry and
   refill pass the same `*Order`, and the map holds the same entry in both, so a
   lookup-based rule cannot tell them apart — which is precisely the distinction §1.3's
   defect turns on. Candidate B fixes the two pinned defects and leaves the third one
   live.
2. **The number it would read shrinks under it.** `TotalRemaining()` equals the client's
   total only at entry. On the third refill it is smaller, so a rule that re-reads it
   would let `MinOrderQty` refuse the tail of an order it admitted, at a moment that
   depends on somebody else's fills. §3.4's third bullet.
3. **The ordering dependency is real but is not the reason.** `ProcessIceberg` does
   register before settling — `e.icebergOrders[ib.Order.ID] = ib`, then the settle a few
   lines below it — so the lookup works today. The dependency is undocumented
   and invisible at the check site, and swapping those two lines would silently restore
   the defect. But it would not be silent for long: the inverted `TestIcebergEvadesTheMaxOrderSizeCap`
   fails on that swap, so the dependency would be *guarded*. Stating this honestly
   matters, because "it creates an ordering dependency" is the objection that sounds
   decisive and is the weakest of the three.
4. **The cost is not the reason either.** A map probe in `checkOrderCaps` runs for every
   order on every venue, but `match()` already solves exactly this with
   `len(e.icebergOrders) > 0` — *"guarded on a venue having icebergs at all, so the
   common path costs one length test per print"* — and the same guard would apply here.
   Refusing B on performance would be refusing it for the one thing about it that is
   fine.

So: refused on (1) and (2), which are correctness; not on (3) or (4).

### 4.4 Admission runs once, and the two refill paths stop disagreeing

> *Rule 3 — the ingress size, notional and account controls run when a COMMAND arrives.
> A refill is not a command.*

`ProcessIceberg`'s refill loop passes the "not a submission" sentinel, so the size and
notional checks, `MaxOrdersPerAccount` and the duplicate check do not run a second time.
This closes §1.3 — the tenth lot no longer evaporates — and it makes the taker-side
refill behave like the maker-side one, which has never run admission at all.

Three consequences, all deliberate:

- **The overflow guard does not run on a refill either.** It cannot fire there: a
  refilled slice is bounded by the total that was already checked, so `Price × slice ≤
  Price × total`, and the total passed. This is an argument, not a test, and §11 row 3
  is where it is attacked.
- **A refill can still be rejected, by the book.** `e.book.Add` can fail on `MaxOrders`,
  and `settleInto`'s GTC branch returns that as a rejection whose status the loop still
  discards. That is pre-existing, is not a quantity defect, and is left — but the
  discard is now the *only* way a refill can be silently lost, where before it was one
  of two, and §12 names it.
- **A venue that tightens `MinOrderQty` mid-session does not strand a resting iceberg's
  tail.** Under today's behaviour it would, on the taker-side path only, at whatever
  moment the reserve happened to be worked down. Nobody would have connected the two.

*Why it will look like a bug:* a refilled slice of 1 rests on a venue whose dust floor
is 5. It is the same order that was admitted at 90, continuing; refusing its own tail
would leave the client holding a quantity the venue will neither trade nor return.

## 5. Decision 3 — should a venue be able to cap the DISPLAY size independently?

**Out of scope, and the reasoning is not "no" but "not here".**

The question is real. An operator who caps total size may separately care how much of it
is shown — a minimum displayed quantity is a live rule on several lit venues, because an
iceberg showing one lot of ten thousand is a venue whose displayed book means nothing.
The mirror rule, a *maximum* display, is rarer and is usually expressed as a ratio.

Four reasons it is not in this slice:

1. **It is a new control, not a repair.** Everything else here is an existing check doing
   arithmetic on the wrong number. A display floor is a policy the venue does not have
   today and nobody has argued for in this repository.
2. **It has no single right shape.** An absolute floor in lots, a fraction of the total,
   a per-instrument minimum tied to lot size, or a floor that applies to the entry slice
   but not to the final remainder — real venues pick differently, and picking one inside
   a bug fix commits the project to it without the argument.
3. **It would make the fingerprint diff unreadable.** §7 bumps `SemanticsVersion` on the
   strength of a specific set of golden lines. Lines produced by a brand-new knob would
   sit in the same diff, indistinguishable, and the next person reading the registry row
   would not be able to tell which change the number is for. This is
   [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §8's refusal to extend the corpus in
   a non-bumping slice, applied in the mirror.
4. **It interacts with peak jitter, and that interaction needs thinking about.**
   `Config.IcebergPeakJitter` varies each refilled slice by up to ±`JitterBps` and clamps
   only at 1 lot (`iceberg.go:74`). A display floor would have to decide whether jitter
   may cross it — and a floor jitter respects is a floor that leaks the reserve's
   existence by never being crossed. That is a real design question and it deserves more
   than a paragraph in someone else's document.

**What a venue that wants it does today.** It refuses at ingress, before constructing the
wrapper, which is where every quantity is still the client's own. `cmd/obgw`'s
`enterIceberg` (`server.go:1581`) already has `m.Order.Quantity` and `m.DisplayQty` in
hand from the wire and already refuses a display size that is zero, negative or larger
than the total with `ReasonInvalidQuantity`; a floor is three lines beside that check.
An embedder using the library directly does the same before calling
`types.NewIcebergOrder`.

**The limitation of that answer, stated rather than glossed.** It is per-embedder, it is
not enforced by the engine, and it is bypassed by any caller that constructs an
`IcebergOrder` directly — including `pkg/wal`'s `restoreEntry` on replay, which means an
ingress-only display floor is not reconstructed from a log. That is exactly the
difference between an ingress convention and an engine control, and it is the argument
for eventually building the real one.

## 6. Decision 4 — a replay of a log written before the fix

### 6.1 Are these controls in `SetReplaying`'s bypass set? No, and that is already decided

`SetReplaying` suppresses exactly two things — the minimum resting time and the
band-breach pause — and its doc comment says why the admission controls are not among
them: the log is written **write-ahead**, so it records commands as *submitted*, not as
*accepted*; an order the live engine rejected is in the log like any other; and the caps
are pure functions of the order, the configuration and the replayed book, so re-running
them reproduces the live decision exactly. Skipping them rested live-rejected orders on
the recovered book.

That argument survives this change unchanged, and this slice does not touch it. What
changes is its stated corollary: *"configuration must not change across a recovery"*
gains a sibling — **the build must not change across a recovery either, unless the
semantics stamp says the change is safe.** Which is what the stamp is for.

### 6.2 What happens to a venue recovering a pre-fix log

Four cases, and only the third has a cost:

| Log | Snapshot | Outcome |
|---|---|---|
| Written by this build | any | unchanged. Replay reproduces the live verdict, including the new refusals |
| Pre-fix, entirely behind the snapshot | covers it | **starts with no ceremony.** The records are never applied, so no verdict is re-taken |
| Pre-fix, records past the snapshot | any | **refuses to start.** The segment declares semantics 2, the build is 3, and `wal.Recover`'s gate refuses before applying a single record ([`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §3.1) |
| Pre-fix, records past the snapshot, `-wal-accept-semantics 2` | any | starts. Any iceberg record whose total exceeds `MaxOrderQty`, or whose notional exceeds `MaxOrderNotional`, is now **refused during replay**, and the recovered book lacks that order |

Row 3 is the answer to the question, and it is the right one: a venue does not silently
rebuild a different book. It refuses, names both versions, and the operator decides.
That is the whole design of the stamp, and this is the first change since it was
introduced that genuinely needs it.

**Row 4 has a sharp edge and it must be written down.** `restoreEntry`'s `KindIceberg`
arm discards `ProcessIceberg`'s result (`wal.go:1782-1784`), so a replay-time refusal
drops the record on the floor with nothing in the recover report to say so. An operator
who accepted the semantics mismatch has been told the books may differ, but has not been
told *which orders are missing* — and unlike the iceberg-reserve refusal, which names
every record it refuses, this one names none. §10 requires the runbook paragraph; §12
names it as the residual, because the honest fix is a count in `RecoverReport` and that
is `pkg/wal`'s slice, not this one.

### 6.3 The snapshot path is untouched, and that asymmetry is deliberate

`LoadSnapshot` rebuilds each iceberg by writing `&types.IcebergOrder{...}` directly
(`snapshot.go:235-243`). It never calls `NewIcebergOrder` and never calls
`ProcessIceberg`, so no mutation happens and **no admission runs**. A snapshot-covered
iceberg over the cap comes back exactly as it was.

That is correct: a snapshot is *state*, and admission is a judgement about *commands*.
Re-admitting a resting order at restore time would cancel a client's live order because
the venue's policy changed while it rested, which no venue does.

It does mean the same order can survive one recovery and not another, depending on
whether the snapshot covers it. That is not new — it is the same shape as
[`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §4.5's covered-prefix behaviour and
[`BOUNDED-RECOVERY.md`](BOUNDED-RECOVERY.md) §5.2's — but this is the first time it
applies to an order the venue *would refuse today*, so it is said out loud here rather
than inherited.

### 6.4 The comment that says the opposite

`Config`'s block comment (`engine.go:213-217`) reads: *"These gate the live ingress path
only (they are bypassed on deterministic replay, which trusts the already-accepted
command log)."* Every clause of that is false — they are not bypassed, and the log does
not record accepted commands. It sits thirty lines above `SetReplaying`, which explains
at length that the opposite is true, and directly above the `MaxOrderQty` field this
document is about.

It is corrected in this slice because a reader deciding whether §6 is right will read
that comment first, and because it is the sentence that would let the next person "fix"
the replay divergence in §6.2 by adding `&& !e.replaying` — which is the mutation §11
row 8 runs.

## 7. Is this a matching-semantics change? Yes, and the corpus is blind to it

**`matching.SemanticsVersion` moves 2 → 3, and the corpus must be extended first.**

By [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §2.1's definition — *two builds share a
version if and only if, for every command sequence and every configuration, they produce
the same trades, events, verdicts and book* — this is not close. `ProcessIceberg(total 9,
display 3)` under `MaxOrderQty = 5` is `NEW` on one build and `REJECTED` on the other,
and every command after it meets a different book. That is the definition's own example.

This is the **opposite answer** to the one [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md)
§5 reached for its sibling defect, and the contrast is the clearest way to state the
boundary: that slice changed the **translation between bytes and commands** and left the
engine alone, so the same command sequence produced the same everything. This slice
changes **the engine's verdict on a command**. Same family, same cause, different layer,
different answer.

**And `internal/semcheck` cannot see it today.** Measured on the corpus as it stands:

- `conditional` is the only scenario with icebergs (`i1`, `i2`, `i3` at
  `corpus.go:264,356,382`) and its config has no size caps at all.
- `guarded` is the only scenario with size caps (`MaxOrderQty 40`, `MinOrderQty 2`,
  `MaxOrdersPerAccount 3`, `PriceBand ±10%`) and its script has no iceberg.

No scenario has both, so the golden is byte-identical after the fix, and
`TestMatchingSemanticsAreFrozen` outcome 4 fails the bump. This is
[`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §6.1 recurring — *"a behaviour the corpus never
reaches is a behaviour nobody can bump for"* — in the same file, one scenario over.

**The corpus gains iceberg commands in `guarded`, appended.** Appending to that
scenario's tail is the surgical choice: every existing line of every scenario is
untouched, the new lines are contiguous at the end of one block, and the caps are already
configured there so no existing verdict moves. Adding caps to `conditional` instead would
re-verdict a hundred lines of a script written for something else, and the diff would be
unreadable in exactly the way §5's third bullet refuses.

The commands, and each one is there because it changes a verdict:

| # | Command | Verdict before | Verdict after |
|---|---|---|---|
| 1 | `ICEBERG g6 S 100 qty 100 disp 3` | `NEW` | `REJECTED:ORDER_EXCEEDS_MAX_QTY` (cap 40) |
| 2 | `ICEBERG g6 S 100 qty 1 disp 1` | `REJECTED:ORDER_BELOW_MIN_QTY` | `REJECTED:ORDER_BELOW_MIN_QTY` — unchanged, and it is the control: a genuinely tiny iceberg is still dust |
| 3 | `ICEBERG g6 S 100 qty 30 disp 1` | `REJECTED:ORDER_BELOW_MIN_QTY` (floor 2) | `NEW`, 29 in reserve |
| 4 | `SUBMIT g7 B 100 qty 10` | takes 1, iceberg refills | same shape, different maker state — the line moves because #3's order exists now |
| 5 | `ICEBERG g6 S 101 qty 40 disp 5` | `NEW` | `NEW` — exactly at the cap, the boundary, unchanged |
| 6 | `ICEBERG g6 S 102 qty 41 disp 5` | `NEW` | `REJECTED:ORDER_EXCEEDS_MAX_QTY` — one over |
| 7 | `CANCEL_ALL g6` | — | closes the scenario as `guarded` already does for `g5` |

Rows 2 and 5 are the ones that earn their place by *not* moving: a fix that measured
something other than the total would move them, and a reviewer reading the regenerated
diff needs lines that prove the new rule is the total and not just "bigger".

**Reading the diff is a deliverable, not a step.** After
`SEMCHECK_UPDATE=1 go test ./internal/semcheck/`, every moved line must be explained by
one of: a new `guarded` line from the seven commands above; a coverage counter moving by
the number of new commands (`ICEBERG`, `SUBMIT`, `CANCEL_ALL`, the rejection names, the
verdicts, the event kinds); or the version line. **Any moved line outside `guarded` is a
behaviour change nobody argued for and stops the slice.** In particular `conditional`'s
three icebergs run under no caps and must be byte-identical — they are the evidence that
this change is confined to venues that configured a cap.

## 8. Rules that will look like bugs

| Rule | Why it will look like a bug |
|---|---|
| An iceberg showing 3 is refused by `MaxOrderQty = 5` | The book never displays anything the cap would refuse, and the client can see that. The reserve is the obligation being weighed, and admission is the only place it is ever weighed (§3.1) |
| A 90-lot iceberg showing 3 is now ACCEPTED by `MinOrderQty = 5`, and a 3-lot slice rests below the floor | The venue's dust floor is 5 and there are 3 lots on the book. The floor is about the *order*, and the order is 90 |
| A refilled slice of 1 rests on a venue whose floor is 5 | Same rule seen from the other end (§4.4). Refusing an order's own tail would leave the client holding a quantity the venue will neither trade nor return |
| Admission does not run on a refill, so a venue that tightens a cap mid-session sees resting icebergs finish under the old one | A live control that some orders ignore. They are not ignoring it; they were admitted before it changed, exactly as a resting plain order is |
| `MaxOrdersPerAccount` still counts an iceberg once, while the size caps now count all of it | Two checks in one function measuring different things. One counts orders and one counts lots, and an iceberg is one order (§3.3) |
| A snapshot restores an iceberg the running venue would now refuse | A restore that bypasses a control. A snapshot is state, not a command, and re-admitting resting orders at restore would cancel clients for a policy change (§6.3) |
| A venue that recovers cleanly today refuses to start after the upgrade | Only with unreplayed records from a pre-fix build. It is the semantics stamp doing its job for the first time on a change that genuinely needs it (§6.2) |
| `SemanticsVersion` moves for this and did NOT move for the journal fix, which also made the same log replay into a different book | The two look identical. One changed the reader; this one changes the matcher. [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §5 is the other half of the argument |
| Both pins now assert the opposite of what their names say | An inverted pin reads as a test flipped to make a change pass. It is the mechanism (`differential_findings_test.go:12-17`), and the names are kept deliberately |
| `settleInto` and `settleAdmitting` differ by one argument | Eight callers would otherwise repeat `order.Quantity`, and the ninth's difference would vanish in the noise (§4.2) |

## 9. What this deliberately does not do

- **It does not change what any market-data consumer sees.** `L2Feed` publishes the
  displayed slice, `Feed` publishes prints, and both are correct. This is stated first
  because it is the failure mode a careless fix has: §1.2 lists four consumers whose
  reading of the slice is the *right* reading, and a change that "made them consistent"
  would announce every reserve on the venue.
- **It does not fix `Engine.Reduce`.** A client still cannot reduce a 9-lot iceberg to 5,
  and a reduce to 2 still leaves 8 lots working (§1.4). The fix needs a decision about
  whether a reduce consumes the reserve or the slice first, and that decision changes
  verdicts, so it is its own slice with its own bump.
- **It does not fix `STPDecrement` stranding an iceberg's reserve** (§1.4), which
  destroys six lots and writes a snapshot `LoadSnapshot` will refuse. It belongs with
  [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md)'s family — a path that fails to undo what it
  did — and not with admission.
- **It does not record client-order-ids for iceberg, stop, OCO, pegged or trailing
  commands** (§1.4), so the de-duplication guard still covers plain orders only.
- **It does not add a display-size control** (§5), and it does not add one "temporarily"
  in `cmd/obgw` either, because an ingress-only floor is not reconstructed on replay and
  a half-built control is harder to argue about than none.
- **It does not change `MaxOrdersPerAccount`, the price band, the guardrail, the rate
  gate or the speed bump**, all of which were audited and are correct (§1.2, §3.3).
- **It does not model icebergs in `internal/refmatch`.** They stay tier 2
  ([`REFERENCE-MATCHER.md`](REFERENCE-MATCHER.md) §2.4), so the differential harness
  cannot reach any of this and a green sweep is evidence of nothing about it — stated
  because "the differential sweep is green" is exactly the sentence someone will offer.
- **It does not add a `RecoverReport` count for a replay-refused iceberg** (§6.2 row 4).
  That is `pkg/wal`'s slice; §12 names it as the residual this leaves behind.
- **It does not touch any exported signature.** `settleAdmitting` and `checkOrderCaps`
  are unexported. `internal/apicheck`'s `surface.txt` must not move, and that is
  deliverable 12 — the proof that a semantics change and an API change are different
  things.

## 10. Deliverables and acceptance criteria

| # | Deliverable | Done when |
|---|---|---|
| 1 | The rule is in the code, once | `checkOrderCaps`'s doc comment states Rule 1 and names the one order type it differs for; `settleAdmitting`'s states Rule 3 and what the sentinel means |
| 2 | All five checks measure the total | `MinOrderQty`, `MaxOrderQty`, `MinOrderNotional`, `MaxOrderNotional` and `checkedMul` read `admitQty`. No sixth check changed |
| 3 | The total travels as a parameter | `settleAdmitting` exists; `settleInto` delegates with `order.Quantity`; eight call sites unchanged; `ProcessIceberg` computes `ib.TotalRemaining()` **before** the first settle and passes it |
| 4 | Admission runs once | The refill loop passes the sentinel. Assert `MinOrderQty = 2`, depth 9, aggressive iceberg 10×3: **ten lots trade**, `Hidden == 0`, `RemainingQty == 0`, and no `REJECTED` reaches the sink. On today's build this fails with nine (§1.3) |
| 5 | Pin 1 inverted, name kept | `TestIcebergEvadesTheMaxOrderSizeCap`: the iceberg is `REJECTED` with `ErrOrderExceedsMaxQty`, nothing rests. **Every existing assertion kept** — the plain-9 control, and `ib.Hidden == 6`, which still holds and now says what the cap saw |
| 6 | Pin 2 inverted, name kept | `TestIcebergIsRefusedForAMinimumItExceeds`: the 90×3 iceberg is accepted, `RejectionReason` is nil, and 3 rests at the offer. The `errors.Is` assertion is inverted, not deleted |
| 7 | The notional pair is asserted, since no pin covers it | Two new tests mirroring the pins: `MaxOrderNotional = 500` refuses the iceberg that today rests with 6 in reserve; `MinOrderNotional = 500` accepts the 90×3 that today is refused |
| 8 | The overflow guard is asserted | A new test: `math.MaxInt64/50 + 1` lots at price 100 as an iceberg shown 3 is `REJECTED:NOTIONAL_OVERFLOW` and the book is empty. It must fail on today's build |
| 9 | The two correct checks are asserted, not assumed | `MaxOrdersPerAccount` counts an iceberg once (cap 2, 90-lot iceberg resting, 2nd `NEW`, 3rd `REJECTED`); a 9×3 iceberg outside the band is refused for the band. §3.3's claims stop being decoration |
| 10 | The displayed size did not move | `TestIceberg_ShowsOnlyDisplaySlice`, `TestIceberg_RefillsAsConsumed`, `TestIceberg_RefillGoesToTheBackOfTheQueue`, `TestIceberg_JitterConfigConserves` and `TestIceberg_ImmediateCrossRefills` pass **unmodified** |
| 11 | The corpus can see the change | `guarded` gains the seven commands in §7. `Coverage.Commands["ICEBERG"]` rises from 3; `conditional`'s three icebergs are byte-identical |
| 12 | The number moved deliberately | `matching.SemanticsVersion == 3`; `semantics.go`'s registry comment gains row 3; [`SEMANTICS-VERSION.md`](SEMANTICS-VERSION.md) §1.2's table gains the row with a changelog link; golden regenerated and **the diff read**, with every moved line accounted for per §7. `surface.txt` does **not** move |
| 13 | The documentation that says the opposite is corrected | `Config`'s comment at `engine.go:213-217` (§6.4); [`ICEBERG-DURABILITY.md`](ICEBERG-DURABILITY.md) §8's second bullet marked fixed with a date and a pointer here; §1.3's first two bullets likewise |
| 14 | The threat model gains the finding | [`THREAT-MODEL.md`](THREAT-MODEL.md) §3.5 gains a paragraph in its own terms — a control a client switched off by choosing an order type, found by audit, closed — and §4's pre-trade-caps row cites the total rule. §5 row 6 stays ✅ and stops being a claim nobody had checked |
| 15 | The runbook covers the new refusal | [`RUNBOOKS.md`](RUNBOOKS.md) gains "An iceberg refused on replay by a cap that did not exist for it", saying what row 4 of §6.2 costs and that the missing orders are not named |
| 16 | Nothing else moved | `go build ./...`, `go vet ./...`, `go test ./... -count=1 -p 1` green, and again with `-race`; `gofmt -l` clean on every touched file; `CHANGELOG.md` under *Changed* names the verdict change and the version bump |

### 10.1 The numbers to record when it is done

None is a pass criterion on its own; they are what §12 is graded against.

- **Golden lines that move outside `guarded`.** Expected **0**. Any number but zero stops
  the slice until it is explained.
- **Golden lines that move inside `guarded`.** Expected 7 new command lines plus the
  coverage block. Record the coverage deltas individually — a rejection counter that does
  *not* move is as informative as one that does.
- **Tests that fail with the fix applied and the pins not yet inverted.** Expected exactly
  2 — the two pins. Anything else is a behaviour change nobody argued for.
- **Tests that fail with `settleAdmitting` fixed but the refill still admitted.** Expected
  1 — deliverable 4. If it is 0, deliverable 4's test is not doing what it claims.
- **`BenchmarkProcessOrder` (or the package's hot-path benchmark) before and after**, plus
  `TestZeroAllocHotPath` unmodified. Expected: unchanged inside noise, 0 allocations. A
  parameter is free; if this moves, something else was added.
- **Venues affected, stated as a predicate.** A venue is affected if and only if it sets
  at least one of the five controls **and** accepts icebergs. Record how many of the
  repository's own configurations satisfy it — expected: `internal/semcheck`'s `guarded`
  after deliverable 11, and nothing else.

## 11. Sabotage runs required before this counts as done

Per [`TESTING.md`](TESTING.md), nothing above counts until every row has been run and its
result recorded — including rows whose honest result is "nothing failed".

| # | Sabotage | Must fail | Status |
|---|---|---|---|
| 1 | Fix `MaxOrderQty` only, leave `MinOrderQty` on the slice | Pin 2. If pin 1 also fails, the two are coupled in a way §3.2 did not describe | **RUN.** Pin 2 fails, pin 1 does not — not coupled. Also `TestIcebergRefilledTailRestsBelowTheDustFloor` and the golden |
| 2 | Fix both quantity controls, leave **both notional** controls on the slice | Deliverable 7's two tests, and nothing else. This row is §3.2's whole argument executed: it measures exactly how invisible the inconsistency would have been | **RUN TWICE.** Before §13.9: exactly those two in the whole tree, `internal/semcheck` and `pkg/wal` green — the fingerprint could not see this half at all, which is what got the corpus extended. **After §13.9: `internal/semcheck` fires too**, on `notional/0003` and `notional/0004` |
| 3 | Fix all four configured caps, leave `checkedMul` on `order.Quantity` | Deliverable 8 only. If nothing else fires, that one test is the entire protection for the one check no operator can configure, and it should say so in its own comment | **RUN TWICE.** Before §13.9: deliverable 8 only, and it says so in its own comment. **After §13.9 it is no longer the entire protection**: `notional/0007` flips `REJECTED:NOTIONAL_OVERFLOW → NEW` and the fingerprint refuses a build that rests an order whose notional does not fit in an `int64` |
| 4 | Pass `ib.TotalRemaining()` on every refill instead of the sentinel | Deliverable 4, late in the order's life: the number shrinks, so the tail is refused. §3.4's third bullet, demonstrated rather than asserted | **RUN.** Deliverable 4 fails on the tail lot. The golden does **not** move: `guarded`'s taker stops well short of the reserve's tail, so only the unit test sees it |
| 5 | Pass the ORIGINAL total on every refill (re-admit with a constant) | **Nothing**, and that is the row's purpose. It is observationally identical, so "admission runs once" is a design statement and not an asserted property. Record it so nobody later claims the suite enforces it | **RUN. Nothing failed** — `pkg/matching`, `internal/semcheck` and `pkg/wal` all green. The once-only rule is unasserted, exactly as predicted |
| 6 | Move the registry insert in `ProcessIceberg` to *after* the first settle | Nothing, under the design in §4.2 — the parameter does not read the registry. Run it anyway: it is the proof that candidate B's ordering dependency was removed rather than merely documented | **RUN. Nothing failed.** The dependency is gone rather than documented |
| 7 | Implement candidate B (registry lookup) instead of the parameter | The two pins pass and **deliverable 4 fails**. This is §4.3's objection (1) executed, and it is the row that justifies the whole of §4 | **RUN.** Both pins pass, both notional tests pass, the overflow test passes, deliverable 4 fails, and the golden is green. Candidate B is the patch that looks finished and leaves §1.3 live |
| 8 | Add `&& !e.replaying` to the four configured caps | Whatever asserts §6.1. If nothing fails, the claim that these controls run on replay is unasserted, and a test must be added — it is load-bearing for §6.2 and for `SetReplaying`'s doc comment | **RUN.** `TestIcebergAdmissionRunsDuringReplay` (new) and the pre-existing `TestReplayStillRejectsOversizeOrders`. The claim was already asserted for plain orders; it now is for icebergs |
| 9 | Fix it in `NewIcebergOrder` instead: stop shrinking `Quantity` | `TestIceberg_ShowsOnlyDisplaySlice` and every depth assertion. Record the full list: it is the measure of how well the display property is guarded against the plausible wrong fix | **RUN, and this row's prediction was wrong** — see §13.2. Ten tests fail across four packages and `TestIceberg_ShowsOnlyDisplaySlice` is **not** one of them |
| 10 | Make `MaxOrdersPerAccount` count `1 + refills` instead of resting orders | Deliverable 9's first test. If it fails nothing, §3.3's "correct as-is" is an unasserted claim about a check the slice deliberately did not move | **RUN.** Deliverable 9's first test, **and the golden** — after §7's extension `guarded` has both an iceberg and the account cap, so the fingerprint now guards this too |
| 11 | Make `L2Feed.remember` publish the iceberg's total | Something in `pkg/marketdata`, loudly. This row exists because §9's first bullet is the failure mode of a careless fix, and a repository that cannot detect a reserve leak has no business claiming it avoided one | **RUN. Nothing failed** — there was no iceberg anywhere in `pkg/marketdata`'s tests. §13.3: a guard was added, and the same sabotage now reads *"SELL 100 has 90 lots in the feed, 3 in the engine"* |
| 12 | Bump `SemanticsVersion` to 3 **without** the corpus extension | `TestMatchingSemanticsAreFrozen` outcome 4. §7's argument, executed | **RUN.** Outcome 4, in its own words: *"the engine's behaviour on the whole corpus is unchanged"*, plus `TestTheGoldenNamesThisBuildsSemantics`. §7's premise measured rather than argued |
| 13 | Extend the corpus **without** bumping | Outcome 2, and outcome 3 on any attempt to regenerate past it | **RUN.** Outcome 2 on the plain run, outcome 3 on `SEMCHECK_UPDATE=1`. This was the tree's actual state for one commit-worth of work, so it was not staged |
| 14 | Apply the whole fix and revert only the two pin inversions | Nothing new — the pins fail with their own messages, which is the point. Record that `internal/semcheck` is red for the right reason (the golden moved) and that `pkg/wal` and `examples/replication` stay green | **RUN.** Exactly 2 failures, both pins, with their own messages. `pkg/wal`, `examples/replication` and `internal/semcheck` all green — semcheck because by then the golden had been regenerated, which is the only part of this row's prediction the ordering changed |
| 15 | Add a display-size floor in `checkOrderCaps` | **Nothing**, which is the measurement that §5's "out of scope" is true rather than accidentally already in scope | **RUN.** No unit test failed — **but the golden moved**, because §7's `ICEBERG g6 S 100 qty 30 disp 1` shows one lot. §5's third reason is therefore literally true: a display floor's lines would land in the same fingerprint diff as this fix's |
| 16 | Spell the sentinel `admitQty <= 0`, as §4.2 sketched it | Added by the build, not by the spec: §13.6. `TestAdmissionMeasuresAnOrderWhoseQuantityIsNotPositive` | **RUN.** That test only — a hand-built order of -5 lots rests at -5 on a venue with a dust floor of 2. `internal/semcheck` and `pkg/wal` green |
| 17 | Spell it `admitQty == -1`, the **first build's own answer** to row 16 | The same test, on the value -1 alone. Added after an adversarial review found the first fix reintroduced the defect it was fixing, one value wide: §13.6 | **RUN.** That test only, twice — *"a hand-built order of quantity -1 ended NEW/`<nil>` under MinOrderQty=2"* and *"rested -1 lots at the offer"*. `internal/semcheck` and `pkg/wal` green, which is why the value had to be in a test rather than in an argument |
| 18 | Remove `ProcessIceberg`'s registry cleanup (§13.4) | `TestARefusedIcebergLeavesNoSnapshotEntry` on **every** row, and the venue must fail to restart after the runbook's own remediation | **RUN.** All seven subtests fail with *"the venue cannot load its own checkpoint"*, naming `LoadSnapshot: iceberg N has no resting displayed slice`. Measured in `pkg/wal` too: recover → checkpoint → restart dies on the same message. `TestAnAdmittedIcebergIsStillInTheSnapshot` stays green, so the assertion is not merely "the registry is empty" |

Rows 5, 6, 10 and 15 ask for a **measurement** rather than a failure. A sabotage nothing
catches is the most useful row in the table: it names which assertion is doing the work,
and four times above the honest answer may be "none".

**Rows 2, 3, 16 and 17 are why this table is worth its length.** Each was recorded
honestly as "nothing else failed", and in each case that answer was the finding: rows 2
and 3 got the corpus extended (§13.9), and rows 16 and 17 are the same defect being
fixed twice, the second time only because a reviewer ran the value the guard did not.

## 12. How this can fail, stated in advance

So whoever implements this is not graded on a curve.

- **The replay refusal in §6.2 row 4 is silent, and this slice leaves it silent.**
  `restoreEntry` discards `ProcessIceberg`'s result, so an operator who accepts a
  semantics mismatch gets a book missing specific orders with nothing naming them. The
  iceberg-reserve gate one layer over names every record it refuses; this refusal names
  none, and the asymmetry will be noticed by the first operator who hits it. The fix is a
  counter in `RecoverReport` and it is a `pkg/wal` slice. If that slice does not follow
  quickly, §10's deliverable 15 runbook paragraph is all the coverage there is, and a
  runbook is not a report.
- **Rule 3 may be wrong for a venue that changes a cap mid-session.** "Admission runs
  once" means a resting iceberg finishes under the policy it was admitted under. A venue
  that tightens `MaxOrderQty` in an incident may expect existing reserves to stop
  working, and this design says they do not. That is the same answer plain resting orders
  get, so it is consistent — but consistency is an argument, and an operator in an
  incident may reasonably want the other one. If that pressure arrives, the answer is a
  separate cancel-side control, not a change to admission.
- **§7's corpus extension may not be enough.** The seven commands cover the quantity caps
  at, over and under their boundaries. They do **not** cover the notional pair or the
  overflow guard, because `guarded` sets no notional caps and the corpus cannot express a
  1.8×10¹⁷-lot order without dominating every aggregate in the golden. So the golden will
  justify the bump on the strength of the quantity half of a five-part change, and the
  notional half rides along on the argument in §3.2 plus deliverables 7 and 8. If a
  reviewer thinks that is too thin, the remedy is a second scenario with the notional
  caps set, and the cost is a wider diff.
  **RESOLVED — the remedy was taken.** Two reviewers asked for it independently and it
  was not merely thin, it was empty: reverting the notional half alone left the whole
  fingerprint GREEN. The `notional` scenario is §13.9. The parenthetical fear above is
  also measured false — a 9.2×10¹⁶-lot order dominates nothing, because it is REJECTED
  and so never rests and never prints.
- **The sentinel is a magic number.** `admitQty <= 0` meaning "not a submission" is the
  kind of encoding that reads fine when written and confusingly a year later, and Go's
  type system will not stop somebody passing a stale zero. A named constant and a doc
  comment are the whole defence. If sabotage row 5 shows the once-only rule is unasserted
  *and* row 6 shows the parameter is not load-bearing, a reviewer should argue for a
  two-method split instead.
  **RESOLVED, and this bullet was the closest thing here to a correct prediction — it
  just aimed one step short.** A named constant and a doc comment were NOT the whole
  defence, because they are a defence against confusion and the problem was not
  confusion: no value of the quantity can mean "not a quantity". The two-method split
  named at the end of this bullet is what shipped, and it was reached the expensive way,
  through a `-1` that rested a hand-built order at -1 lots. §13.6.
- **The audit in §1 is only as good as its list.** It enumerated non-test reads of
  `order.Quantity` and the risk decisions in five packages. A consumer that derives a
  quantity indirectly — from a depth aggregate, from a snapshot field, from an event's
  order pointer — would not appear in that enumeration. `pkg/orderentry`'s private stream
  was found that way and is in §1.2 because it was looked for, not because a grep
  returned it. Something else may be hiding in the same shape.
- **"An iceberg's cap is its total" may simply not be every venue's policy.** §3.4 refuses
  the alternative on the grounds that the current behaviour is an accident and that a
  client-selectable control is not a control. Both hold. But a venue that genuinely wants
  slice-based caps now has no way to ask for it, and the honest response if that request
  arrives is a second knob argued in its own document — not a re-argument of this one.

## 13. What building it changed

Written after the code, and it is where this document was wrong.

### 13.1 §7's seven commands needed an eighth, and the seventh line of `guarded` is why

The seven commands in §7 were specified against a `guarded` scenario that ends with a
**resting privileged bid at 150** — `g3`'s exempt order, line `guarded/0003`, still on
the book at `guarded/0010` (`bid=150:2 n=1`). Every iceberg in §7 is a *sell* at
100–102, so all of them would have crossed it, printed at 150, and dragged the ±10%
band reference from 100 to 150 with them. The cases after the first would then have
been refused for `PRICE_OUTSIDE_BAND` — including row 5, the one that earns its place
by being accepted at exactly the cap.

So the block opens with `CANCEL g3`, and it is labelled in the corpus as plumbing
rather than a case. The seven commands and their verdicts are unchanged; what changed
is that they now measure what §7 says they measure. Measured, before and after the
fix:

| # | Command | Before | After |
|---|---|---|---|
| 1 | `ICEBERG g6 S 100 qty 100 disp 3` | `NEW`, 3 at the offer | `REJECTED:ORDER_EXCEEDS_MAX_QTY` |
| 2 | `ICEBERG g6 S 100 qty 1 disp 1` | `REJECTED:ORDER_BELOW_MIN_QTY` | `REJECTED:ORDER_BELOW_MIN_QTY` |
| 3 | `ICEBERG g6 S 100 qty 30 disp 1` | `REJECTED:ORDER_BELOW_MIN_QTY` | `NEW`, 1 at the offer, 29 in reserve |
| 4 | `SUBMIT g7 B 100 qty 10` | `FILLED` on 4 prints (3,3,3,1) against #1 | `FILLED` on 10 one-lot prints against #3's reserve |
| 5 | `ICEBERG g6 S 101 qty 40 disp 5` | `NEW` | `NEW` |
| 6 | `ICEBERG g6 S 102 qty 41 disp 5` | `NEW` | `REJECTED:ORDER_EXCEEDS_MAX_QTY` |
| 7 | `CANCEL_ALL g6` | `CANCELLED n=3` | `CANCELLED n=2` |

Rows 2 and 5 hold their verdicts, which is what they are for.

### 13.2 §11 row 9's prediction was wrong, and the honest list is more interesting

The row says that stopping `NewIcebergOrder` from shrinking `Quantity` must fail
`TestIceberg_ShowsOnlyDisplaySlice` "and every depth assertion". **It does not.** That
test passes under the mutation, because the book publishes `RemainingQty` and the
mutation leaves `RemainingQty` at the display size. The display property is not guarded
where this document assumed it was.

What actually fails, in full — ten tests across four packages:

- `pkg/types`: `TestNewIcebergOrder_ShrinksVisibleSlice`, `TestNoOrderWrapperConstructorMutatesItsOrder`
- `pkg/matching`: `TestFailingFOKCorruptsAnIcebergsReserve`,
  `TestFailingFOKRestoresAPartiallyConsumedIcebergSlice`,
  `TestFailingFOKRestoresAnIcebergAtItsOwnPlaceInTheQueue`,
  `TestRejectedFOKPreservesEveryLevelsQueueOrder`, `TestEventStreamReconstructsBook`,
  `FuzzExoticOrders`
- `pkg/wal`: `TestLogOnlyRecoveryLosesAnIcebergsReserve`
- `internal/semcheck`: `TestMatchingSemanticsAreFrozen`

The guard is real and it is broad. It just lives in the constructor's own tests, in the
fill-or-kill restore, and in event-stream reconstruction — not in the test whose name
says "shows only display slice".

### 13.3 §11 row 11 failed to fail, so a guard was added

The reserve-leak sabotage — `L2Feed.remember` publishing the whole quantity — broke
**nothing** in `pkg/marketdata`, in either form (with the constructor as it is, and
with §11 row 9 applied so `Quantity` really is the client's total). The reason is
simple and was not visible from the engine side: **there was no iceberg anywhere in
`pkg/marketdata`'s tests.** §9's first bullet claims this slice does not leak hidden
size, and nothing in the package that publishes hidden size was checking.

`TestL2FeedPublishesOnlyAnIcebergsDisplayedSlice` is that assertion, over a 90-lot
iceberg showing 3, before and after a refill. With it in place the same sabotage reads:

```
l2feed_test.go:269: after an iceberg rests: SELL 100 has 90 lots in the feed, 3 in the engine
```

This is the one place the slice touched a package outside `pkg/matching` and
`internal/semcheck`, and it is test-only.

### 13.4 A fifth consumer of the same mutation, found on the way — deferred, then fixed

**This section was written as "not fixed" and is now the opposite. The reasoning that
deferred it is kept, because it was wrong for a reason worth naming.**

**A REJECTED iceberg stayed in `e.icebergOrders`, and the next snapshot could not be
loaded.** `ProcessIceberg` registers the wrapper *before* settling — the maker-side
refill in `match()` needs to find it there — and never removed it when the settle
refused, so `TakeSnapshot` wrote an `IcebergEntry` for an order that is not resting and
`LoadSnapshot` refuses such a snapshot with *"iceberg N has no resting displayed
slice"*. Measured:

```
halted venue:                    snapshot orders=0 icebergs=1  LoadSnapshot: iceberg 1 has no resting displayed slice
dust iceberg (1 shown 1):        snapshot orders=0 icebergs=1  LoadSnapshot: iceberg 1 has no resting displayed slice
over MaxOrderQty (9 shown 3):    snapshot orders=0 icebergs=1  LoadSnapshot: iceberg 1 has no resting displayed slice
post-only that would cross:      snapshot orders=1 icebergs=1  LoadSnapshot: iceberg 2 has no resting displayed slice
fill-or-kill that cannot fill:   snapshot orders=0 icebergs=1  LoadSnapshot: iceberg 1 has no resting displayed slice
immediate-or-cancel, no depth:   snapshot orders=0 icebergs=1  LoadSnapshot: iceberg 1 has no resting displayed slice   (CANCELLED, not rejected)
1000 refused over-cap icebergs:  registry=1000, snapshot icebergs=1000
```

**Why it was deferred:** the halted and dust paths are pre-existing, the halted one
never reaches `checkOrderCaps` at all, it is not a quantity defect, and it has the shape
of [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md)'s family — a path that fails to undo what it
did. All of that is true and none of it was the point.

**Why deferring it was wrong.** Two independent reviews came back with the same
objection and it holds: *this slice makes it the routine consequence of its own headline
rejection.* Before the slice an over-cap iceberg rested and the venue checkpointed
cleanly; after it, that iceberg is refused, and one refusal is enough to make every
later checkpoint unloadable **for the life of the engine** — measured: two ordinary
orders trade and rest normally afterwards and the snapshot still will not load. It is
also unbounded, so the reject path the caps exist to make cheap grows memory per client.
And it lands on this slice's own upgrade path: §6.2's remediation and
[`RUNBOOKS.md`](RUNBOOKS.md)'s new section tell an operator to accept the semantics
mismatch, start, and **take a checkpoint** — measured end to end, that produced a venue
that could not restart. Documented remediation must not strand a venue.

**The fix is one condition in the function this slice already rewrote**, and it is not
"delete on REJECTED":

```go
if _, resting := e.book.Get(ib.Order.ID); !resting {
	delete(e.icebergOrders, ib.Order.ID)
}
```

*The registry holds icebergs whose displayed slice is RESTING*, because that is the only
thing it is read for — refilling a slice a taker consumed, and writing the reserve into
a snapshot. Stated that way it covers the CANCELLED immediate-or-cancel row too, which a
rejection-only test would have left open and which a fix aimed at the new row would have
missed. `TestARefusedIcebergLeavesNoSnapshotEntry` runs all seven rows, asserts the
`LoadSnapshot` that follows, and asserts it still loads after further trading;
`TestAnAdmittedIcebergIsStillInTheSnapshot` is the control that a resting iceberg keeps
its reserve across the round trip and still refills after it.

**What it does NOT fix, stated so nobody reads more into it.** The cleanup runs in
`ProcessIceberg`, so it reaches only the command that submitted the iceberg. The
identical `LoadSnapshot` symptom reached from §1.4's `STPDecrement` path — where
`match()` removes a maker iceberg without refilling it — is untouched, measured after
the fix: `Hidden=6`, `registry=1`, same refusal. That one must stay loud until its own
missing refill is fixed, because a loadable snapshot there would mean quietly discarding
six lots of a client's order.

**Cost to the fingerprint: 7 lines, DIGEST ONLY.** `guarded/0012`–`0018` move their hash
and nothing else — identical verdicts, identical events, identical book state, identical
counters. That is the registry contents entering `EngineSnapshot.Digest`, which is
correct: a snapshot taken at those points genuinely differs, and it differs by being
loadable. Classified mechanically rather than by eye: of every line present in both
goldens, 7 changed and **0 changed by more than the digest**.

### 13.5 Deliverable 4's arithmetic

The deliverable asks for "depth 9 ... ten lots trade". Ten lots cannot trade against
nine. The test uses **depth 10**, which is what makes every count in it exact — ten
traded, `Hidden == 0`, `RemainingQty == 0` — and it fails on the pre-fix build with
nine, exactly as §1.3 measured. §1.3's own probe used depth 9 and observed the tail
*evaporating* rather than resting; both shapes are asserted, the second by
`TestIcebergRefilledTailRestsBelowTheDustFloor`.

### 13.6 §4.2's sentinel was wrong, and the first attempt to fix it was wrong the same way

**This section was written twice. Both drafts are kept, because the second one is only
interesting next to the first.**

§4.2 sketches the sentinel as `admitQty <= 0`. Under that spelling, an order whose
`Quantity` is zero or negative skips **every** check in `checkOrderCaps`, including the
ones a venue configured. That is not hypothetical: `types.NewOrder` refuses a
non-positive quantity, but `pkg/wal`'s `restoreEntry` replays `e.Order.Fresh()` straight
out of a decoded record, so a corrupt or hand-edited log entry *is* a hand-built order —
the case `checkOrderCaps`'s own doc comment says the overflow guard exists for.

**The first draft's answer was a named constant of -1 tested for equality**, on the
argument that a *stale zero* is the failure §12 names and equality refuses it. That
draft ended: *"the sentinel is a value no command can carry, so the only verdicts this
slice moves are the ones it argued for."*

**That sentence was false, and the guard test written to prove it omitted the one value
that breaks it.** The hole did not close; it shrank to width one and moved. An order of
quantity exactly -1 skipped every cap and rested on the book at -1 lots, where the
pre-fix build refused it — a REGRESSION introduced by the fix for a regression. The
guard looped over `{0, -5}`, which is why it looked safe: 0, -2 and -5 are all still
refused. It was found by an adversarial review that ran the value the test did not.

Measured with `MinOrderQty = 2` and an order struct built by hand, on all three
encodings:

```
admitQty <= 0        quantity  0 -> NEW,      not resting
admitQty <= 0        quantity -5 -> NEW,      RESTING at -5 lots
admitQty == -1       quantity -1 -> NEW,      RESTING at -1 lots
admitQty == -1       quantity  0 -> REJECTED, ORDER_BELOW_MIN_QTY
a separate method    quantity -1 -> REJECTED, ORDER_BELOW_MIN_QTY  (the pre-fix verdict)
a separate method    quantity MinInt64 -> REJECTED, ORDER_BELOW_MIN_QTY
```

**The general lesson is worth more than the fix.** There is no `int64` an order cannot
carry, so no encoding of "this is not a client submission" as a *value of the quantity*
can be sound. `<= 0` and `== -1` are the same mistake at different widths, and picking a
rarer constant only makes the surviving case harder to find. The distinction is a
property of the CALL, not of the number, so it has to travel out of band.

**What shipped is §12's own escape hatch, the two-method split.** `settleRefill` is a
separate method that passes `submitted = false` to the shared `settle` body;
`settleAdmitting` passes `true` and the quantity; `settleInto` passes `true` and
`order.Quantity`. `checkOrderCaps` has no "skip me" branch at all any more and measures
every value it is given. Decision 2 is unchanged — the quantity still travels as an
explicit parameter, and Rule 3 still holds — but nothing a client sends can forge the
exemption, because the exemption is not something a client can send.

`TestAdmissionMeasuresAnOrderWhoseQuantityIsNotPositive` now loops over
`{0, -1, -2, -5, math.MinInt64}`. Against the `-1` encoding it fails with
*"a hand-built order of quantity -1 ended NEW/<nil> under MinOrderQty=2 ... rested -1
lots at the offer"*, measured, so it fires against the encoding it argues with. The
golden does not move under any of the three: every order in the corpus is built through
`types.NewOrder`.

### 13.7 The numbers §10.1 asked for

These are the FINAL numbers, after §13.4's registry fix and §13.9's `notional`
scenario. The first draft of this section was measured before both.

- **Golden lines that CHANGE, anywhere in the file: 7**, and every one of them changes
  **the digest and nothing else** — same verdict, same events, same book state, same
  counters. They are `guarded/0012`–`0018`, which are themselves lines this slice
  appended, so **no line that existed before this slice moved at all**. Classified
  mechanically, not by eye: of every line present in both goldens, 7 differ and 0 differ
  by more than the digest. The cause is §13.4's registry cleanup entering
  `EngineSnapshot.Digest`.
- **Golden lines that are APPENDED: 19.** Eight in `guarded` — §7's seven cases plus
  §13.1's cancel — and eleven in the new `notional` scenario (§13.9), which is added
  last precisely so it appends and never inserts. Plus the version line, the scenario
  header and nine coverage lines.
- **Golden lines that are DELETED: 0.**
- **`fifo`, `prorata-shard7`, `capped-decrement-shard3`, `auction` and `conditional` are
  byte-identical.** `conditional`'s three icebergs run under no caps and did not move,
  which is the evidence that this change is confined to venues that configured one. So
  are `guarded`'s own first eleven lines.
- **Coverage deltas, individually.** `ICEBERG 3→13`, `CANCEL 127→128`,
  `CANCEL_ALL 26→28`, `SUBMIT 767→772`; `ACCEPTED 429→450`, `REJECTED 409→418`,
  `TRADE 159→175`, `CANCELED 134→138`; `NEW 219→223`, `FILLED 86→88`,
  `CANCELLED 93→96`, `REJECTED 648→657`; `ORDER_EXCEEDS_MAX_QTY 1→3`,
  `ORDER_BELOW_MIN_QTY 1→2`, and three rejection kinds **that the corpus had never
  reached at all** — `ORDER_EXCEEDS_MAX_NOTIONAL 0→3`, `ORDER_BELOW_MIN_NOTIONAL 0→2`,
  `NOTIONAL_OVERFLOW 0→1`. `LIMIT 412→433`, `GTC 369→390`; `fifo 906→924`;
  `trades 143→159`; `refills 3→18`.
- **The three new rejection kinds are the point of §13.9.** A counter going 0→n is the
  fingerprint reaching a verdict it had no way to reach before, which is the difference
  between a corpus that can justify this bump and one that cannot.
- **The informative deltas are still the ones that are ZERO**: `TOO_MANY_ORDERS` and
  `PRICE_OUTSIDE_BAND` both stay at 1, which is §3.3's two unmoved checks showing up as
  arithmetic; `icebergRestores` stays at 2 and `cascadeTerminals` at 1, so the previous
  slice's evidence is intact; `MARKET`, `IOC`, `FOK`, `DAY`, `GTD` and every `stpModes`
  entry are unchanged, because nothing here touches a time-in-force or an STP mode.
- **Tests that fail with the fix applied and the pins not yet inverted: exactly 2**,
  both pins (§11 row 14).
- **Tests that fail with `settleAdmitting` fixed but the refill still admitted: 1**,
  deliverable 4 (§11 rows 4 and 7).
- **Hot path.** `TestZeroAllocHotPath` passes unmodified. `BenchmarkEngine_MatchInto`
  and `BenchmarkEngine_CancelReplaceInto` hold **0 allocs/op and 0 B/op** before and
  after, including after §13.6's two-method split added a second scalar parameter
  (`MatchInto` 348–370 ns/op, `CancelReplaceInto` 180–245 ns/op, measured after; the
  spread within a single run of two is wider than the difference from before, which is
  the honest way to say the wall-clock column is noise on this machine). The allocation
  columns are the load-bearing half of this measurement, and two scalar parameters are
  free.
- **Venues affected, as a predicate.** A venue is affected if and only if it sets at
  least one of the five controls **and** accepts icebergs. In this repository:
  `internal/semcheck`'s `guarded` after §7 and its `notional` after §13.9, and nothing
  else. `cmd/obgw`'s defaults set none of the five.

### 13.8 What is still true and uncomfortable

Two of §12's five have been closed since it was written (§13.6, §13.9) and are marked
RESOLVED there. What remains:

- **The refill exemption is unasserted, and the split did not change that.** §11 row 5
  confirms it: re-admitting every refill against the client's total is observationally
  identical on everything the suite measures, so nothing enforces "admission runs once"
  *as such*. What the suite enforces is that admission does not run against a
  *shrinking* number (row 4). That is the property that matters, and the weaker one is a
  design statement — written down as such rather than assumed. Moving from a sentinel to
  `settleRefill` made the exemption unforgeable, not observable.
- **The replay refusal is still silent.** §12's first bullet is untouched: `restoreEntry`
  discards `ProcessIceberg`'s result, so an operator who accepts a semantics mismatch
  gets a book missing specific orders with nothing counting them. The fix is a counter in
  `RecoverReport` and it is a `pkg/wal` slice. [`RUNBOOKS.md`](RUNBOOKS.md) is all the
  coverage there is, and a runbook is not a report. What HAS changed is that the
  runbook's own remediation no longer strands the venue — §13.4.
- **The audit is still only as good as its list**, and §13.4 is the argument for
  pessimism: a fifth consumer of the same mutation was found by walking the reject path
  rather than by grepping, after §1 had declared the enumeration complete.

### 13.9 §12's corpus prediction was right, and the remedy cost less than it feared

§12 warned that `guarded` sets no notional caps, so the golden would justify the bump on
the quantity half of a five-part change. Two reviewers checked and it was worse than
"thin": reverting **only** `checkedMul(order.Price, admitQty)` to `order.Quantity` —
reopening both notional caps *and* the int64 overflow guard, the one an operator cannot
configure off — left `internal/semcheck` **byte-identical**. Three unit tests fired and
the fingerprint did not. That is [`PINNED-DEFECTS.md`](PINNED-DEFECTS.md) §6.1's blind
spot recurring one control over, which is exactly what §7's extension existed to stop.

The remedy §12 named is now the `notional` scenario: `MinOrderNotional = 500`,
`MaxOrderNotional = 5000`, eleven commands. Three plain orders are the controls — inside
the window, under the floor, over the ceiling — and each iceberg case carries the same
notional as one of them, so the rule under test reads off the page: **an iceberg gets the
same verdict as the plain order of the same notional.** It covers the ceiling evaded by
`displayQty` (6000 total, 1000 slice), the floor refusing real size (5000 total, 300
slice), the boundary from both sides, a control that is dust under either rule, a
refill walk whose every slice is 300 and under the floor, and the overflow guard.

Two things §12 got wrong about the cost:

- **The 9.2×10¹⁶-lot order dominates nothing.** §12 feared it would swamp every
  aggregate. It is REJECTED, so it never rests and never prints: it contributes one
  line, one `REJECTED` event and one `NOTIONAL_OVERFLOW` counter, exactly like every
  other refusal. That is a fact about rejection, not about the number.
- **The diff is not wider than the scenario.** Appended last as its own scenario rather
  than merged into `guarded`, it moves **no existing line** — not an id, not an event id,
  not a digest. Separate rather than merged because `guarded`'s two size caps would have
  decided most of these commands before the notional pair saw them.

Sabotaged, it now fires: with the notional half reverted, `notional/0003` flips
`REJECTED:ORDER_EXCEEDS_MAX_NOTIONAL → NEW`, `0004` flips `NEW →
REJECTED:ORDER_BELOW_MIN_NOTIONAL`, and `notional/0007` — the overflow — goes
`REJECTED:NOTIONAL_OVERFLOW → NEW`, an order whose notional does not fit in an `int64`
**resting on the book**, `ask=100:29 n=3`.

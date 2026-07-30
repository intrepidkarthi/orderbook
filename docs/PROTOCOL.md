# Order-entry protocol

The binary order-entry protocol spoken by `cmd/obgw`, the reference gateway.

It exists to demonstrate that the library's pieces compose into a working venue
edge. It is **not** a standard, and it is not FIX, OUCH or SBE. If you need one of
those, write an adapter — the seam this protocol sits on (`pkg/orderentry`) is the
supported surface, and the codec is deliberately unexported.

---

## What this is and is not

| | |
|---|---|
| **Framing** | SoupBinTCP 3.00, taken from the published spec |
| **Payloads** | This repository's own, fixed-width big-endian |
| **Dependencies** | None. A 2-byte length and a 1-byte type. |
| **Transport security** | **None.** Assumes a trusted network or a TLS wrapper below. |
| **Credentials** | A shared secret in the clear. Suitable for a lab, not a venue. |
| **Instruments** | One per gateway |
| **Stability** | Frozen by `internal/wire/testdata/*.hex`; changing a layout means bumping `Version` |
| **Current version** | 2 |

Framing is borrowed rather than invented so the session rules — heartbeats,
sequenced replay, login and logout — are somebody else's well-tested design.
Payloads are ours because no standard order-entry payload matches this engine's
order surface.

---

## What is deliberately absent from the wire

A client **never names an account** and **never sees an engine order id**. Orders
are referenced only by the client's own `ClOrdID`, scoped to the authenticated
session.

That is a security boundary, not a simplification. The engine cancels by
`(orderID, userID)`, and self-trade prevention lets one account observe another's
resting orders. A wire that carried either field would let a client name an order
it does not own; there is simply no field in which to express that. Two accounts
using the identical `ClOrdID` string cannot reach each other's orders, and there
is a test for exactly that.

Also absent, deliberately:

- **`STPMode`** — self-trade-prevention policy belongs to the venue, not the
  client.
- **The privileged flag** — it is a liquidation capability. Client-settable, it
  would bypass every pre-trade cap.
- **Symbol on a cancel** — the gateway serves one instrument.

---

## Session

```
client                          server
  |  LoginRequest ('L')            |
  |------------------------------->|
  |                                |  LoginAccepted ('A')  -> session id, sequence
  |<-------------------------------|  or LoginRejected ('J') -> one reason byte
  |                                |
  |  Unsequenced ('U') Enter       |
  |------------------------------->|
  |                                |  Sequenced ('S') Accepted / Executed / ...
  |<-------------------------------|
  |  ClientHeartbeat ('R')         |
  |------------------------------->|
  |  LogoutRequest ('O')           |
  |------------------------------->|
```

Login must be the first packet. Anything else drops the connection with no reply:
an unauthenticated peer learns nothing about the venue.

**Authentication defaults to deny.** A gateway with no accounts configured rejects
every login. An empty configuration must not produce an open venue.

### Login rejection codes

| Byte | Meaning |
|---|---|
| `A` | Not authorised — unknown user or wrong password |
| `S` | Unknown session — the cursor belongs to a different venue incarnation |
| `Q` | Bad sequence — the requested point is no longer retained |

---

## Resume, and why a session id is not decoration

Reconnect with the session id you were given and the last sequence you received,
and the server replays everything since — including executions that landed while
you were disconnected.

**The guarantee, stated so it can be falsified:** *for one venue incarnation, an
account's outbound sequence is dense and gap-free from 1, and any suffix still
within the retention ring can be replayed exactly once, in order.*

Three ways that can fail, and what happens instead of failing silently:

- **The venue restarted.** Sequence numbers only mean anything within one run. A
  restart mints a new incarnation id, so a stale cursor is refused with `S` rather
  than served different content under numbers you believe you already have.
- **You are too far behind.** The per-account ring is bounded, because an
  unbounded one turns a client that never reconnects into a venue-wide memory
  leak. A cursor older than what is retained is refused with `Q`. You must
  reconcile out of band; you are not told you are up to date.
- **You are ahead of the venue.** Claiming messages that were never sent means you
  are out of step, and is refused rather than ignored.

The reason resume works at all is that an account's outbound **stream outlives any
connection**. A `Session` is a socket; a `Stream` is the account's sequence. If
outbound events belonged to the connection, a maker whose resting order filled
while its TCP connection was down would never learn about the fill — the worst
failure an order-entry system can have, because the client's position is now wrong
and it cannot tell.

---

## Messages

All payloads are big-endian, fixed-width, and begin with two bytes: the **message
type**, then the **protocol version**. Both are checked on decode — a payload that
would decode cleanly as the wrong message is precisely what this header prevents.

Fixed-width string fields are NUL-padded; an over-long value is a hard error
rather than a truncation, because a truncated `ClOrdID` would collide with another
of your own orders.

> **v1 → v2.** Version 1 had no type byte and distinguished messages by payload
> length. Any future message sharing a length with an existing one would have been
> silently misread as it. The type byte is why the version freeze exists, and this
> is what spending it looks like.

### Inbound

**Enter** — a new order.

| Field | Bytes | Notes |
|---|---:|---|
| MsgType | 1 | `E` |
| Version | 1 | |
| ClOrdID | 20 | your identifier, unique within your session |
| Symbol | 16 | must match the gateway's instrument |
| Side | 1 | `B` buy, `S` sell |
| Type | 1 | `L` limit, `M` market |
| TIF | 1 | `G` GTC, `I` IOC, `F` FOK |
| PostOnly | 1 | |
| Price | 8 | ticks; 0 for market |
| Quantity | 8 | lots |

**Cancel** — MsgType `C` (1) + Version (1) + ClOrdID (20).

**Reduce** — MsgType `M` (1) + Version (1) + ClOrdID (20) + Quantity (8). Shrinks
a resting order **in place, keeping its queue position**, and is answered by a
`Replaced`.

This is the one order-entry operation a client provably cannot build for itself.
Cancel-then-new is the obvious substitute and it is wrong: it sends the order to
the back of its price level, which for a maker managing size is a material loss.

Three properties are load-bearing:

- **`Quantity` is the new total, not a delta.** A delta cannot be made safe
  against a concurrent fill — the venue and the client would be subtracting from
  different numbers, and the resulting size would depend on which of the two the
  venue believed. A total is unambiguous whatever arrived in between.
- **It is a reduction only.** An increase, or a price change, forfeits priority;
  a resting order that could grow ahead of the queue would let a participant
  reserve a place in line. Those remain cancel-then-new and are refused here
  rather than silently reinterpreted.
- **Zero is not a cancel.** A client that means to cancel must send a `Cancel`.
  Reinterpreting a reduce-to-zero would give one message two meanings.

Unlike a cancel, a refused reduce is always reported, because it fails for reasons
the client caused and can correct: asking to grow (`14` invalid quantity), asking
to shrink below what is already filled (also `14`), or naming an order that is not
yours or no longer live (`2` unknown order). A client that heard nothing could not
distinguish a refusal from a reduce still in flight.

**A reduce is subject to the venue's minimum resting time**, exactly as a cancel
is, and is refused with `17` until the order has met it. That control targets the
spoofing pattern of posting size and pulling it before it can fill; a reduce from
1000 lots to 1 withdraws 999 of them, so exempting it would have left the pattern
available behind a different verb. Retry once the floor has elapsed. The floor is
off unless the venue configures one.

Reduce is durable: the command is written to the WAL before it is applied, so the
size a client was told is the size the venue holds after a restart. This was not
true when `Engine.Reduce` was first added — the log recorded submits and cancels
only, so recovery silently restored the pre-reduce size.

**MassCancel** — MsgType `F` (1) + Version (1). Cancels every order the account has
resting, and is answered by a **MassCancelAck** `G` (Count, Seq).

This is the control a market maker reaches for when its own state is wrong or it
needs out of the market now, and it is the difference between a venue you can test
against and one you would quote on. Each removed order still produces its own
`Canceled` on the stream; the ack follows them all and says how many there were, so
a completed sweep of zero orders is distinguishable from a connection that died
mid-sweep.

The ack is written only after every `Canceled` it accounts for has been queued for
your connection. An ack that overtook them would have you briefly believing you hold
a book the venue has already emptied.

**CancelOnDisconnect** — MsgType `B` (1) + Version (1) + Enabled (1), answered by
**CODAck** `V`. Asks the venue to pull your book if this session drops. Idempotent,
so re-assert it freely.

It is a message rather than a `LoginRequest` field because adding a field there would
move every byte after it and invalidate a committed golden vector — which is what the
type byte exists to avoid.

Two caveats worth knowing before you enable it:

- **The sweep is account-wide.** Orders are not tagged with the session that entered
  them, so an account holding two connections — one with this enabled — loses its
  whole book when that one drops, including orders entered on the other.
- **A venue shutdown does not trigger it.** A graceful shutdown drops every
  connection at once, and firing the sweep there would permanently destroy books that
  are meant to come back after the restart. The control means "if I lose my session",
  not "if the venue closes".

**Query** — MsgType `Q` (1) + Version (1). Carries nothing: the account is the
session's and the gateway serves one instrument.

### Outbound

| Message | Payload | Carries |
|---|---|---|
| **Accepted** `A` | ClOrdID, Price, Quantity, Side | the order is live |
| **Rejected** `R` | ClOrdID, Reason | the engine looked and declined |
| **Executed** `X` | ClOrdID, Price, Quantity, LeavesQty, Aggressor | a fill |
| **Canceled** `D` | ClOrdID, Reason | the order left the book |
| **Replaced** `P` | ClOrdID, LeavesQty | size changed in place, queue kept |
| **CmdReject** `K` | ClOrdID, Reason | the venue would not look at the command |
| **OpenOrder** `O` | ClOrdID, Price, LeavesQty, Side | one live order, in reply to a Query |
| **QueryEnd** `T` | Count, Seq | the Query reply is complete |
| **MassCancelAck** `G` | Count, Seq | the mass cancel is complete |
| **CODAck** `V` | Enabled | the cancel-on-disconnect setting in force |

Each is preceded by its type byte and the version, as above.

`Rejected` and `CmdReject` are distinct on purpose: one means the engine evaluated
your order and refused it, the other means the command never reached the engine —
malformed, throttled, or the matcher was saturated. A client that conflates them
retries the wrong things.

`Canceled` arrives whether you asked or not. Self-trade prevention, an OCO twin
filling, an IOC remainder, and an operator kill switch all remove orders you did
not cancel.

`Replaced` likewise has two causes: a `Reduce` you sent, and self-trade-prevention
DECREMENT shrinking a maker you did not touch. A client that assumes it only ever
follows its own `Reduce` will drift on the second.

**`LeavesQty` is trustworthy** because the engine's event stream is proven to
reconstruct per-order remaining quantity — see `TestEventStreamReconstructsBook`.
Without that proof the field would have been a guess, and once the golden vectors
were committed it could never have been added.

### Reason codes

| Code | Meaning | | Code | Meaning |
|---:|---|---|---:|---|
| 0 | None | | 8 | Post-only would cross |
| 1 | Other | | 9 | FOK cannot fill |
| 2 | Unknown order | | 10 | Halted |
| 3 | Duplicate ClOrdID | | 11 | Throttled |
| 4 | Too small | | 12 | Overloaded |
| 5 | Too large | | 13 | Not authorised |
| 6 | Price band | | 14 | Malformed |
| 7 | Self-trade | | 15 | Shutting down |
| | | | 16 | Invalid quantity |
| | | | 17 | Too soon |

Code `16` is distinct from `14`: malformed means the venue would not look at the
message, invalid quantity means it looked at a real order of yours and the size
you asked for is not one it can take.

Code `17` is the only refusal here worth simply retrying. It means the venue runs a
minimum resting time and the order has not met it yet — see below.

The vocabulary is deliberately narrow and lossy. Mirroring the engine's internal
error set would mean that adding a sentinel — an ordinary, non-breaking change —
silently became a protocol change. Anything unrecognised maps to `Other`, which a
client must already handle.

---

## Reconciliation, when resume is not available

Resume can legitimately fail: an evicted cursor (`Q`) or a restarted venue (`S`).
Send a **Query** and the server replies with one **OpenOrder** per live order,
then a **QueryEnd**.

The report is the venue's authoritative view, read from the book on the matching
goroutine — not from any consumer's shadow copy, which is the point, since you are
asking precisely because you no longer trust your own picture.

Two details that make it usable rather than merely present:

- **The terminator is not optional.** `QueryEnd.Count` lets you verify you
  received the whole report. Without it you cannot distinguish "you have nothing
  open" from "the connection died mid-report" — opposite conclusions.
- **`QueryEnd.Seq` names the point in your own stream the report is consistent
  with.** The server reads the book, drains its publisher, and only then writes
  the report, so every event up to that instant has already reached you.
  Everything after `Seq` is a change to apply on top. Reading the book without
  draining first would let an execution from before the read arrive after the
  report, and you would apply it twice.

Pending stops and trailing stops are not included. They are not resting orders
and a client reconciling its book should not treat them as such.

## Session liveness

The server sends a heartbeat every 5s on an idle outbound path, so a client can
tell a quiet venue from a dead one. An unauthenticated peer has 10s to send its
login; an authenticated session has 30s of read idle before it is dropped. Any
inbound packet, including a client heartbeat, refreshes it.

Connect-and-say-nothing is the cheapest resource-exhaustion attack there is, so
the unauthenticated timeout is the tightest of the three.

## Durability

`obgw -wal path` turns on the write-ahead log: every command is written before it
is applied, group-committed every 20ms, and replayed on start. With `-snapshot`
and `-checkpoint` it also snapshots on a cadence, so a restart replays only the
tail after the last checkpoint rather than all history.

Records are CRC-32C-checksummed behind a magic header. A crash mid-write leaves a
torn tail and recovery stops at it cleanly; a complete record whose checksum
disagrees is media corruption and recovery **refuses to start** rather than serving
a book that does not match its log. The record length is bounded, so a corrupted
prefix cannot turn a restart into a multi-gigabyte allocation.

Every command that mutates the book is logged: Enter, Cancel, Reduce, and the
operator's account-wide cancel. That list is the whole contract — a mutating
command missing from it is not "not yet logged", it is a book the log cannot
reproduce, which is exactly how a reduced order used to come back at its original
size and a pulled account used to get its book handed back.

Without `-wal` the gateway runs with no durability at all and says so on startup.
That is a legitimate configuration for a test harness and an indefensible one for
anything else.

**Recovery restores the session layer's index too, not just the book.** On start
the gateway seeds its `ClOrdID` → order-id map from the recovered book, so an order
that outlived a restart can still be named in a `Cancel` or a `Reduce`, and a fill
against it still produces an execution report.

That last one is why this matters most. Without the index the publisher held no
record of a recovered order, so a trade against it was dropped rather than
reported: a maker whose resting order filled while the venue was down would never
have been told, and its position would have been wrong with no way to notice. It is
the same failure the [stream-outliving-the-connection](#resume-and-why-a-session-id-is-not-decoration)
design exists to prevent, and recovery had been reintroducing it.

Adoption restores the index, not the conversation. Nothing is re-announced on any
stream: those orders were acknowledged in a previous incarnation, and replaying
them into a fresh sequence space would be inventing history. A client that wants
the current picture asks for it with a `Query`.

## Backpressure

Three bounded queues, each of which drops or refuses rather than blocking, because
the alternative is one participant stalling the venue:

1. **Inbound.** The matcher's command queue is bounded. A full queue yields
   `CmdReject` with `Overloaded`; the client sheds.
2. **Publisher.** Bounded between the matching goroutine and the fan-out pump.
   Overflow drops the oldest and increments a counter. Blocking here would stop
   the venue; growing without limit would end it differently.
3. **Per connection.** A bounded send queue. A client that stops reading is
   **disconnected** rather than allowed to back up into the venue.

A client that misses messages discovers it through a sequence gap and can resume.
It is never told it is up to date when it is not.

---

## Running it

```sh
go run ./cmd/obgw -addr 127.0.0.1:9000 -symbol BTC-USD -accounts alice:s3cret,bob:hunter2
```

With durability:

```sh
go run ./cmd/obgw -addr 127.0.0.1:9000 -symbol BTC-USD \
  -accounts alice:s3cret -wal obgw.wal -snapshot obgw.snap -checkpoint 30s
```

`cmd/obgw/server_test.go` is a working client: login, enter, cancel, resume, and
the failure paths. It is the most useful reference for writing another one.

## On the golden vectors

`internal/wire/testdata/*.hex` were generated by running the encoder. They prove
the layout has not changed **accidentally** — which is their job, and a real one.
They do not prove the layout is correct; nothing here does that except reading it.
Treat them as a ratchet, not as a specification.

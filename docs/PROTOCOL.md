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

All payloads are big-endian, fixed-width, and begin with a one-byte `Version`.
Fixed-width string fields are NUL-padded; an over-long value is a hard error
rather than a truncation, because a truncated `ClOrdID` would collide with another
of your own orders.

Outbound message types are distinguished by payload length.

### Inbound

**Enter** — a new order.

| Field | Bytes | Notes |
|---|---:|---|
| Version | 1 | |
| ClOrdID | 20 | your identifier, unique within your session |
| Symbol | 16 | must match the gateway's instrument |
| Side | 1 | `B` buy, `S` sell |
| Type | 1 | `L` limit, `M` market |
| TIF | 1 | `G` GTC, `I` IOC, `F` FOK |
| PostOnly | 1 | |
| Price | 8 | ticks; 0 for market |
| Quantity | 8 | lots |

**Cancel** — Version (1) + ClOrdID (20).

### Outbound

| Message | Payload | Carries |
|---|---|---|
| **Accepted** | Version, ClOrdID, Price, Quantity, Side | the order is live |
| **Rejected** | Version, ClOrdID, Reason | the engine looked and declined |
| **Executed** | Version, ClOrdID, Price, Quantity, LeavesQty, Aggressor | a fill |
| **Canceled** | Version, ClOrdID, Reason | the order left the book |
| **Replaced** | Version, ClOrdID, LeavesQty | size changed in place, queue kept |
| **CmdReject** | Version, ClOrdID, Reason | the venue would not look at the command |

`Rejected` and `CmdReject` are distinct on purpose: one means the engine evaluated
your order and refused it, the other means the command never reached the engine —
malformed, throttled, or the matcher was saturated. A client that conflates them
retries the wrong things.

`Canceled` arrives whether you asked or not. Self-trade prevention, an OCO twin
filling, an IOC remainder, and an operator kill switch all remove orders you did
not cancel.

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

The vocabulary is deliberately narrow and lossy. Mirroring the engine's internal
error set would mean that adding a sentinel — an ordinary, non-breaking change —
silently became a protocol change. Anything unrecognised maps to `Other`, which a
client must already handle.

---

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

`cmd/obgw/server_test.go` is a working client: login, enter, cancel, resume, and
the failure paths. It is the most useful reference for writing another one.

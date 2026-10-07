# FIX 4.4 Order Entry — the Codec, Without the Session

Status: **implemented; step 2.3 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md), issue #6** ·
Author: Karthikeyan NG · 2026-10-07

## 1. Scope

Trading systems speak FIX. `pkg/fix` lets a FIX 4.4 client, or a test harness that
speaks FIX, enter orders into this engine and receive execution reports. It covers the
**application messages** and the **wire format**. The session layer is out of scope:
logon, heartbeats, sequence-number recovery and resend. A session engine supplies
those, and it hands this package one application message at a time.

## 2. The wire: `fix.Parse` and `fix.Builder`

- **A message is a sequence of `tag=value` fields, each ended by SOH (0x01).** It
  starts with `8=FIX.4.4`, then `9=<BodyLength>`, then the body, and ends with
  `10=<CheckSum>`.
- **`Parse(b) (Message, n, error)`** reads one message from the front of `b`. It returns
  `ErrIncomplete` if `b` holds only part of one, so a stream reader knows to read more.
  It refuses with an error:
  - **a wrong BodyLength**: the count of bytes from the field after `9=` up to and
    including the SOH before `10=`;
  - **a wrong CheckSum**: the byte sum mod 256 of everything before `10=`, as three
    digits;
  - **a BeginString other than `FIX.4.4`**;
  - a field without `=`, a tag that is not a positive integer, or the header fields
    out of their required order.
- **`Message`** keeps fields in wire order, repeated tags included, and gives access by
  tag. It is not a `map[int]string`: order and repetition are part of FIX.
- **`Builder`** writes the body fields, then frames them with BodyLength and CheckSum.
  It is the only way this package produces bytes.

## 3. In: NewOrderSingle (D) and OrderCancelRequest (F)

`DecodeNewOrderSingle` and `DecodeOrderCancelRequest` refuse any other MsgType (35), as
`internal/wire` refuses a message it was not asked for.

| Tag | Field | Use |
|---|---|---|
| 11 | ClOrdID | required; becomes `Order.ClientOrderID` |
| 1 | Account | the order's user; if absent, SenderCompID (49) |
| 55 | Symbol | must be the `Instrument`'s symbol |
| 54 | Side | 1 buy, 2 sell; anything else refused |
| 38 | OrderQty | decimal, converted to lots; refused unless it is a whole number of lots |
| 40 | OrdType | 1 market, 2 limit; anything else refused |
| 44 | Price | required for limit; decimal, converted to ticks; refused unless on the tick grid |
| 59 | TimeInForce | 0 Day (also when absent), 1 GTC, 3 IOC, 4 FOK; anything else refused |
| 18 | ExecInst | contains `6` (participate don't initiate): post-only |
| 41 | OrigClOrdID | F: the order to cancel |

- **Prices and quantities never pass through a float.** They are parsed as decimals
  and converted with `types.Instrument`.
- **Off-grid values are refused, never rounded.** `Instrument.PriceToTicks` rounds, and
  a venue that rounds a client's price trades at a price nobody sent.

## 4. Out: ExecutionReport (8) and OrderCancelReject (9)

`fix.Reporter` is a `matching.EventSink`. It turns the engine's events into
ExecutionReports, one per affected order, each addressed to that order's owner:

| Event | ExecType (150) | OrdStatus (39) |
|---|---|---|
| Accepted | 0 New | 0 New |
| Trade, for maker and for taker | F Trade, with LastPx (31) and LastQty (32) | 1 Partially filled, or 2 Filled |
| Canceled (including an IOC or market remainder) | 4 Canceled | 4 Canceled |
| Rejected | 8 Rejected, with OrdRejReason (103) and Text (58) | 8 Rejected |

- **Every report carries** OrderID (37, the engine's id), ClOrdID (11), ExecID (17,
  unique per report), Symbol, Side, OrderQty, LeavesQty (151), CumQty (14) and AvgPx
  (6).
- **The Reporter keeps each order's quantities itself**, from the events in sequence.
  It does not read them through an event's `*Order`, because that pointer shows the
  order's state at publication, not at the event ([`BENCH-GATE.md`](BENCH-GATE.md)
  §3.2).
- **OrdRejReason** goes through `pkg/orderentry`'s mapping, so the two protocols cannot
  drift apart:

  | Code | Meaning | Engine refusals |
  |---|---|---|
  | 2 | exchange closed | halted |
  | 3 | exceeds limit | too large |
  | 5 | unknown order | unknown order |
  | 6 | duplicate order | duplicate ClOrdID |
  | 11 | unsupported characteristic | a DAY order with no session close |
  | 13 | incorrect quantity | too small, invalid quantity |
  | 99 | other | everything else, such as a post-only that would cross |

  Text (58) is the engine's error in every case.
- **A cancel the engine refuses publishes no event.** So `fix.Entry` answers it with an
  OrderCancelReject: 434=1, CxlRejReason 102 = 1 unknown order or 0 too late to
  cancel.

`fix.Entry` holds the ClOrdID-to-order-id map that turns a cancel's OrigClOrdID into an
engine id. It refuses a NewOrderSingle whose ClOrdID it has already seen.

## 5. How it is tested (issue #6's checklist)

- **A captured round trip.** `testdata/session.in` holds a short client sequence:
  - a resting order;
  - an order that partly fills it;
  - one that fills the rest;
  - a cancel;
  - a post-only that would cross;
  - a cancel of an unknown order.

  `testdata/session.out` holds every report the engine sends back, byte for byte, with
  `|` for SOH in both files. The clock and the ExecID counter are fixed, so the output
  is reproducible.
- **Framing.** A corrupted CheckSum, a BodyLength off by one each way, a wrong
  BeginString and a truncated message are each refused. Each test is watched passing
  against a decoder with that check removed.
- **Decoding.** Each refusal rule in §3 has its own case.
- **Every built message parses**, with its CheckSum and BodyLength recomputed by an
  independent loop in the test.
- [`INTEGRATION.md`](INTEGRATION.md)'s "What to build around the core" points here, and
  the exported API snapshot is regenerated.

## 6. Not in this step

- The session layer, and a TCP acceptor.
- OrderCancelReplaceRequest (G), and the drop-copy, market-data and post-trade
  messages.
- FIX 5.0 and FIXT.
